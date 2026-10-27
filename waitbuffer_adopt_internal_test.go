package gusset

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"unsafe"
)

func dataPtr(b []byte) uintptr {
	return uintptr(unsafe.Pointer(unsafe.SliceData(b)))
}

// adoptHeapBuffer takes the slice itself when it is 64-byte aligned, and copies
// otherwise. The alignment is tested here on slices whose placement the test
// chooses, so neither arm depends on where Go's allocator put anything.
func TestAdoptHeapBuffer_AdoptsAlignedAndCopiesUnaligned(t *testing.T) {
	const n = 100
	backing := make([]byte, n+2*heapBufferAlign)
	off := 0
	if rem := int(dataPtr(backing) % heapBufferAlign); rem != 0 {
		off = heapBufferAlign - rem
	}
	fill := func(b []byte) {
		for i := range b {
			b[i] = byte(i*7 + 3)
		}
	}

	t.Run("aligned is adopted", func(t *testing.T) {
		data := backing[off : off+n : off+n]
		fill(data)
		buf := adoptHeapBuffer(nil, data)
		if dataPtr(buf.data) != dataPtr(data) {
			t.Fatal("an aligned result was copied; want it adopted as the Buffer's backing")
		}
		if len(buf.data) != n || cap(buf.data) != n {
			t.Fatalf("len %d cap %d, want %d/%d", len(buf.data), cap(buf.data), n, n)
		}
		if buf.id != 0 {
			t.Fatalf("id %d, want 0", buf.id)
		}
	})

	t.Run("aligned with spare capacity is cut to cap == len", func(t *testing.T) {
		data := backing[off : off+n] // cap runs on past n
		fill(data)
		buf := adoptHeapBuffer(nil, data)
		if dataPtr(buf.data) != dataPtr(data) {
			t.Fatal("an aligned result was copied; want it adopted")
		}
		if cap(buf.data) != n {
			t.Fatalf("cap %d, want %d: an append would write past the result", cap(buf.data), n)
		}
	})

	t.Run("unaligned is copied", func(t *testing.T) {
		data := backing[off+1 : off+1+n : off+1+n]
		fill(data)
		buf := adoptHeapBuffer(nil, data)
		if dataPtr(buf.data) == dataPtr(data) {
			t.Fatal("an unaligned result was adopted")
		}
		if !isAligned64(buf.data) {
			t.Fatal("the copy is not 64-byte aligned")
		}
		if !bytes.Equal(buf.data, data) {
			t.Fatal("the copy differs")
		}
		if cap(buf.data) != n {
			t.Fatalf("cap %d, want %d", cap(buf.data), n)
		}
		// The copy is independent of its source.
		buf.data[0] ^= 0xff
		if buf.data[0] == data[0] {
			t.Fatal("the copy shares its source")
		}
	})

	t.Run("empty is never adopted", func(t *testing.T) {
		buf := adoptHeapBuffer(nil, backing[off:off])
		if len(buf.data) != 0 {
			t.Fatalf("len %d, want 0", len(buf.data))
		}
	})
}

// completedData returns the result a finished ticket is holding in completed,
// the very slice WaitBuffer is about to be handed.
func completedData(t *testing.T, s *handleState, ticket uint64) []byte {
	t.Helper()
	var data []byte
	eventually(t, "the result to complete", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		res, ok := s.completed[ticket]
		data = res.data
		return ok
	})
	return data
}

// Every inline size yields a correct, 64-byte aligned, cap == len, id-0 Buffer
// through WaitBuffer, whichever way it was built. A result that arrives aligned
// is the Buffer's own backing; one that does not is a copy. Which of the two
// happens is decided by the run-time check alone, so both are asserted from the
// slice the waiter was actually handed, not from a size.
func TestWaitBuffer_EverySizeIsAlignedAndCorrectWhetherAdoptedOrCopied(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()

	var adopted, copied int
	for n := 1; n <= inlineResultBytes; n++ {
		in := echoInput(n)
		tk, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		held := completedData(t, h.state, tk)
		wasAligned := dataPtr(held)%heapBufferAlign == 0
		buf, err := h.WaitBuffer(ctx, tk)
		if err != nil {
			t.Fatalf("%d B: %v", n, err)
		}
		got := buf.Bytes()
		if buf.ID() != 0 || !bytes.Equal(got, in) || !isAligned64(got) || cap(got) != len(got) {
			t.Fatalf("%d B: id %d, intact %v, aligned %v, len %d cap %d",
				n, buf.ID(), bytes.Equal(got, in), isAligned64(got), len(got), cap(got))
		}
		if shared := dataPtr(got) == dataPtr(held); shared != wasAligned {
			t.Fatalf("%d B: result aligned=%v but adopted=%v; the run-time check alone decides", n, wasAligned, shared)
		}
		if wasAligned {
			adopted++
		} else {
			copied++
		}
		if err := buf.Free(); err != nil {
			t.Fatal(err)
		}
	}
	// Both arms must have run, or this test proved nothing about one of them.
	// A result of 4096 bytes is a whole size class and lands on a page-aligned
	// span, so the adopting arm always runs; the sizes between 64-byte classes
	// are placed at 16-byte multiples and so exercise the copying arm.
	if adopted == 0 || copied == 0 {
		t.Fatalf("adopted %d, copied %d: the sweep did not exercise both arms", adopted, copied)
	}
	t.Logf("adopted %d sizes, copied %d, of %d", adopted, copied, inlineResultBytes)
}

// CallBuffer reaches the same Buffer through waitBuffer, from a Go-heap input
// and, for sizes over 4 KiB, from a Rust-owned one.
func TestCallBuffer_EverySizeIsAlignedAndCorrect(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()

	check := func(n int, in *Buffer, want []byte) {
		t.Helper()
		out, err := h.CallBuffer(ctx, in)
		if err != nil {
			t.Fatalf("%d B: %v", n, err)
		}
		defer out.Free()
		got := out.Bytes()
		if !bytes.Equal(got, want) {
			t.Fatalf("%d B: result differs (%d bytes)", n, len(got))
		}
		if !isAligned64(got) {
			t.Fatalf("%d B: result is not 64-byte aligned", n)
		}
		if n <= inlineResultBytes {
			if out.ID() != 0 || cap(got) != len(got) {
				t.Fatalf("%d B: id %d cap %d len %d, want id 0 and cap == len", n, out.ID(), cap(got), len(got))
			}
		} else if out.ID() == 0 {
			t.Fatalf("%d B: a result over the inline limit lost its Rust id", n)
		}
	}

	for n := 1; n <= inlineResultBytes; n++ {
		in := echoInput(n)
		seed, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		src, err := h.WaitBuffer(ctx, seed)
		if err != nil {
			t.Fatal(err)
		}
		check(n, src, in)
		_ = src.Free()
	}
	for _, n := range []int{inlineResultBytes + 1, inlineResultBytes + 64, 5000, 16384, 65536} {
		in, err := h.NewBuffer(n)
		if err != nil {
			t.Fatal(err)
		}
		want := echoInput(n)
		copy(in.Bytes(), want)
		check(n, in, want)
		_ = in.Free()
	}
}

// Results that were produced together share no memory: writing through one
// Buffer, or appending to its Bytes, changes nothing in any other result, and
// no two results overlap. Sizes below 16 bytes come from Go's tiny allocator,
// which packs several objects into one block; the rest cover the size classes.
func TestWaitBuffer_ResultsDoNotAliasEachOther(t *testing.T) {
	h, err := Open(WithPoolSize(8), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()

	sizes := []int{1, 2, 3, 7, 8, 15, 16, 17, 48, 49, 63, 64, 65, 100, 128, 256, 1024, 1152, 4095, 4096}
	const workers = 8
	const perWorker = 40
	type result struct {
		buf  *Buffer
		want []byte
	}
	results := make([]result, workers*perWorker)
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				idx := w*perWorker + i
				n := sizes[idx%len(sizes)]
				in := echoInput(n)
				// Distinct content per job in the tail; byte 0 stays the echo mode.
				for j := 1; j < n; j++ {
					in[j] ^= byte(idx * 31)
				}
				tk, err := h.Submit(ctx, in)
				if err != nil {
					errs <- err
					return
				}
				buf, err := h.WaitBuffer(ctx, tk)
				if err != nil {
					errs <- err
					return
				}
				results[idx] = result{buf: buf, want: in}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// No two live results overlap.
	type span struct{ lo, hi uintptr }
	spans := make([]span, 0, len(results))
	for _, r := range results {
		b := r.buf.Bytes()
		spans = append(spans, span{dataPtr(b), dataPtr(b) + uintptr(len(b))})
	}
	for i := range spans {
		for j := i + 1; j < len(spans); j++ {
			if spans[i].lo < spans[j].hi && spans[j].lo < spans[i].hi {
				t.Fatalf("results %d and %d overlap in memory", i, j)
			}
		}
	}

	// Scribble over each result in turn, and append to it; every other result
	// must still hold exactly what its job returned.
	for i, r := range results {
		b := r.buf.Bytes()
		if !bytes.Equal(b, r.want) {
			t.Fatalf("result %d was changed before it was touched", i)
		}
		for j := range b {
			b[j] = 0xA5
		}
		grown := append(b, bytes.Repeat([]byte{0x5A}, 64)...)
		if len(b) > 0 && &grown[0] == &b[0] {
			t.Fatalf("result %d: append wrote in place, cap %d len %d", i, cap(b), len(b))
		}
		for k := i + 1; k < len(results); k++ {
			if !bytes.Equal(results[k].buf.Bytes(), results[k].want) {
				t.Fatalf("scribbling over result %d changed result %d", i, k)
			}
		}
	}
	for i, r := range results {
		for _, c := range r.buf.Bytes() {
			if c != 0xA5 {
				t.Fatalf("result %d holds %#x after its own scribble; something else wrote to it", i, c)
			}
		}
		if err := r.buf.Free(); err != nil {
			t.Fatal(err)
		}
	}
}

// A WaitBuffer result of 4096 bytes is the slice drainPipe made, with nothing
// copied: the whole of what it costs beyond Wait is the *Buffer.
//
// 4096 is the size this exists for. It is a full size class, so the result is
// 64-byte aligned on every Go runtime this library supports; if that stops
// being true the fast path silently stops being taken, and this test says so.
func TestWaitBuffer_AlignedResultCostsOnlyTheBufferHeader(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()
	in := echoInput(inlineResultBytes)

	tk, err := h.Submit(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if held := completedData(t, h.state, tk); dataPtr(held)%heapBufferAlign != 0 {
		t.Fatalf("a %d-byte result is not 64-byte aligned (%#x): the size-class placement this fast path relies on no longer holds", inlineResultBytes, dataPtr(held))
	}
	if err := h.Discard(tk); err != nil {
		t.Fatal(err)
	}

	waitAllocs := testing.AllocsPerRun(200, func() {
		tk, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Wait(ctx, tk); err != nil {
			t.Fatal(err)
		}
	})
	waitBufferAllocs := testing.AllocsPerRun(200, func() {
		tk, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		buf, err := h.WaitBuffer(ctx, tk)
		if err != nil {
			t.Fatal(err)
		}
		_ = buf.Free()
	})
	t.Logf("allocs/op at %d B: Wait %v, WaitBuffer %v", inlineResultBytes, waitAllocs, waitBufferAllocs)
	// Wait allocates the result once (drainPipe). WaitBuffer may add the
	// *Buffer, and nothing else: a second backing array is the copy.
	if waitBufferAllocs > waitAllocs+1 {
		t.Fatalf("WaitBuffer costs %v allocs/op against Wait's %v: the result was copied instead of adopted", waitBufferAllocs, waitAllocs)
	}
}
