//go:build goexperiment.simd

package bench_test

// Go 1.27's portable SIMD (GOEXPERIMENT=simd) versus a Gusset round trip, on
// byte-identical work.
//
// Gusset's Go side does no numeric work of its own — its hot path is a ticket
// read and runtime memmove, which is already vectorized — so SIMD does not make
// Gusset faster. What it changes is the adopter's decision docs/choosing.md is
// about: a kernel that vectorizes in pure Go may no longer be worth crossing
// into Rust. This measures that crossover on the diagnostic engine's mode 10
// (sum of squares of bytes), which the Rust side vectorizes by checking
// cancellation per 4 KiB chunk.
//
// Run:
//
//	GOEXPERIMENT=simd go test ./bench -run 'SIMD' -bench 'SumSquares' -benchtime=1s

import (
	"context"
	"fmt"
	"math/rand"
	"simd"
	"testing"
	"unsafe"

	"github.com/bharathvbcr/gusset"
)

func sumSquaresScalar(in []byte) uint64 {
	var acc uint64
	for _, b := range in {
		v := uint64(b)
		acc += v * v
	}
	return acc
}

// sumSquaresSIMD is the same kernel with the portable simd package.
//
// The portable API has no byte-widening loads, so the input is read as uint32
// lanes (4 bytes each) and every byte is masked out and squared in 32-bit
// lanes, where 255² cannot overflow. A 32-bit lane accumulates at most 4 squares
// per step, so it is flushed to 64 bits well before 2³² (every 4096 steps).
func sumSquaresSIMD(in []byte) uint64 {
	// Reinterpreting as []uint32 needs 4-byte alignment (checkptr enforces it
	// under -race); take any unaligned head scalar.
	var total uint64
	if len(in) > 0 {
		if rem := uintptr(unsafe.Pointer(unsafe.SliceData(in))) % 4; rem != 0 {
			head := min(int(4-rem), len(in))
			total = sumSquaresScalar(in[:head])
			in = in[head:]
		}
	}
	words := len(in) / 4
	if words > 0 {
		u32 := unsafe.Slice((*uint32)(unsafe.Pointer(unsafe.SliceData(in))), words)
		mask := simd.BroadcastUint32s(0xFF)
		var acc simd.Uint32s
		lanes := acc.Len()
		lane := make([]uint32, lanes)
		flush := func() {
			acc.Store(lane)
			for _, v := range lane {
				total += uint64(v)
			}
			acc = simd.Uint32s{}
		}
		steps := 0
		i := 0
		for ; i+lanes <= words; i += lanes {
			x := simd.LoadUint32s(u32[i:])
			b0 := x.And(mask)
			b1 := x.ShiftAllRight(8).And(mask)
			b2 := x.ShiftAllRight(16).And(mask)
			b3 := x.ShiftAllRight(24)
			acc = acc.Add(b0.Mul(b0)).Add(b1.Mul(b1)).Add(b2.Mul(b2)).Add(b3.Mul(b3))
			if steps++; steps == 4096 {
				flush()
				steps = 0
			}
		}
		flush()
		total += sumSquaresScalar(in[i*4 : words*4])
	}
	return total + sumSquaresScalar(in[words*4:])
}

// The three implementations must agree before their timings mean anything.
func TestSIMDSumSquaresMatchesScalarAndRust(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	rng := rand.New(rand.NewSource(1))
	for _, n := range []int{0, 1, 3, 4, 63, 64, 65, 1000, 4095, 70_000, 1 << 20} {
		in := make([]byte, n)
		rng.Read(in)
		// Worst case for the 32-bit lanes: every byte 255.
		if n == 70_000 {
			for i := range in {
				in[i] = 255
			}
		}
		want := sumSquaresScalar(in)
		if got := sumSquaresSIMD(in); got != want {
			t.Fatalf("n=%d: simd %d != scalar %d", n, got, want)
		}
		for off := 1; off < 4 && off < n; off++ { // unaligned starts
			if got, w := sumSquaresSIMD(in[off:]), sumSquaresScalar(in[off:]); got != w {
				t.Fatalf("n=%d off=%d: simd %d != scalar %d", n, off, got, w)
			}
		}
		if got := rustSumSquares(t, h, in); got != want {
			t.Fatalf("n=%d: rust %d != scalar %d", n, got, want)
		}
	}
}

// rustSumSquares runs diagnostic mode 10 through Gusset, inline or by Buffer.
func rustSumSquares(tb testing.TB, h *gusset.Handle, in []byte) uint64 {
	tb.Helper()
	ctx := context.Background()
	var out []byte
	var err error
	if len(in)+1 <= 4096 {
		out, err = h.Call(ctx, append([]byte{10}, in...))
	} else {
		buf, e := h.NewBuffer(len(in) + 1)
		if e != nil {
			tb.Fatal(e)
		}
		defer buf.Free()
		b := buf.Bytes()
		b[0] = 10
		copy(b[1:], in)
		var t uint64
		t, err = h.Submit(ctx, buf)
		if err == nil {
			out, err = h.Wait(ctx, t)
		}
	}
	if err != nil {
		tb.Fatal(err)
	}
	var v uint64
	for i := 7; i >= 0; i-- {
		v = v<<8 | uint64(out[i])
	}
	return v
}

var sink uint64

func BenchmarkSumSquares(b *testing.B) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		b.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()

	for _, n := range []int{256, 4000, 64 << 10, 1 << 20} {
		in := make([]byte, n)
		rand.New(rand.NewSource(2)).Read(in)

		b.Run(fmt.Sprintf("%dB/GoScalar", n), func(b *testing.B) {
			b.SetBytes(int64(n))
			for i := 0; i < b.N; i++ {
				sink += sumSquaresScalar(in)
			}
		})
		b.Run(fmt.Sprintf("%dB/GoSIMD", n), func(b *testing.B) {
			b.SetBytes(int64(n))
			for i := 0; i < b.N; i++ {
				sink += sumSquaresSIMD(in)
			}
		})
		b.Run(fmt.Sprintf("%dB/Gusset", n), func(b *testing.B) {
			b.SetBytes(int64(n))
			if n+1 <= 4096 {
				payload := append([]byte{10}, in...)
				for i := 0; i < b.N; i++ {
					if _, err := h.Call(ctx, payload); err != nil {
						b.Fatal(err)
					}
				}
				return
			}
			// Filled once: the Buffer is the zero-copy input path, so the
			// measurement is the round trip plus the Rust kernel.
			buf, err := h.NewBuffer(n + 1)
			if err != nil {
				b.Fatal(err)
			}
			defer buf.Free()
			bb := buf.Bytes()
			bb[0] = 10
			copy(bb[1:], in)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				t, err := h.Submit(ctx, buf)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := h.Wait(ctx, t); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
