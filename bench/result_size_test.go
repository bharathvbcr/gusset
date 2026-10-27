package bench_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/bharathvbcr/gusset"
)

// resultSizes straddle every egress boundary a successful result crosses:
//
//	0       an empty result (no buffer at all)
//	48      the largest result carried whole in the completion record
//	        (INLINE_RESULT_MAX): no gusset_take, no registry buffer
//	49      the smallest result that is stored and fetched with gusset_take
//	256     typical small payloads, 1024 a typical RPC body
//	4096    the largest result Go copies out of take()'s buffer in drainPipe
//	        (inlineResultBytes), and the largest the worker leaves unpromoted
//	4097    the smallest result the worker promotes onto a Buffer, so take()
//	        returns a pointer and the waiter owns the free
//
// Each size is a sub-benchmark named "<n>B" so benchstat can compare one size
// across two recordings (`benchstat -filter '/1024B'`) and a boundary pair
// side by side (`-col /size`).
var resultSizes = []int{0, 48, 49, 256, 1024, 4096, 4097}

// maxInlineInput is MAX_INLINE_INPUT: the largest input copied during
// gusset_submit. A request over it must travel in a Rust-owned Buffer.
const maxInlineInput = 4096

// echoRequest builds a diagnostic-engine request whose result is exactly n
// bytes: mode 0 echoes its whole input, so the request is the result. The
// diagnostic engine has no mode that returns a chosen number of bytes from a
// fixed-size request (mode 16 does, but returns its output as an adopted
// Buffer whatever the size, so it never reaches the result-size thresholds
// this file measures).
//
// An empty request is the engine's "empty in, empty out" case.
func echoRequest(n int) []byte {
	req := make([]byte, n)
	for i := 1; i < n; i++ {
		req[i] = byte(i)
	}
	return req
}

// echoInput is the Submit argument for an n-byte echo: the bytes themselves
// while they fit the inline input limit, and a Buffer above it. Only 4097B
// takes the Buffer branch, and so only that sub-benchmark also pays the
// buffer-input submit path (a buffer-id lookup instead of a copy); read it
// against 4096B with that in mind. The returned release function frees the
// Buffer, and is a no-op for inline input.
func echoInput(b *testing.B, h *gusset.Handle, n int) (in any, release func()) {
	b.Helper()
	req := echoRequest(n)
	if n <= maxInlineInput {
		return req, func() {}
	}
	buf, err := h.NewBuffer(n)
	if err != nil {
		b.Fatalf("NewBuffer(%d): %v", n, err)
	}
	copy(buf.Bytes(), req)
	return buf, func() { _ = buf.Free() }
}

func openForResultSize(b *testing.B) *gusset.Handle {
	b.Helper()
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	b.Cleanup(func() { _ = h.Close() })
	return h
}

// BenchmarkGussetCallResult is Call with the result size as the variable: a
// serial round trip whose request and result are both n bytes. There is no
// 4097B case: Call refuses a []byte request over 4 KiB (ErrInputTooLarge), and
// a result over 4 KiB needs a request that large for the echo to produce it.
// BenchmarkGussetSubmitWaitResult covers 4097B through a Buffer request.
func BenchmarkGussetCallResult(b *testing.B) {
	for _, n := range resultSizes {
		if n > maxInlineInput {
			continue
		}
		b.Run(fmt.Sprintf("%dB", n), func(b *testing.B) {
			h := openForResultSize(b)
			ctx := context.Background()
			req := echoRequest(n)

			// One checked call before the clock starts: a run that reports a
			// figure has returned the right bytes.
			got, err := h.Call(ctx, req)
			if err != nil {
				b.Fatalf("Call failed: %v", err)
			}
			if !bytes.Equal(got, req) {
				b.Fatalf("Call returned %d bytes, want the %d it sent", len(got), n)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := h.Call(ctx, req)
				if err != nil {
					b.Fatalf("Call failed: %v", err)
				}
				if len(res) != n {
					b.Fatalf("Call returned %d bytes, want %d", len(res), n)
				}
			}
		})
	}
}

// BenchmarkGussetSubmitWaitResult is Submit then Wait at each result size,
// including 4097B (see echoInput). Wait copies the result onto the Go heap, so
// this is the egress path Call shares; it differs from Call only in boxing the
// request into an `any`.
func BenchmarkGussetSubmitWaitResult(b *testing.B) {
	for _, n := range resultSizes {
		b.Run(fmt.Sprintf("%dB", n), func(b *testing.B) {
			h := openForResultSize(b)
			ctx := context.Background()
			in, release := echoInput(b, h, n)
			defer release()
			want := echoRequest(n)

			ticket, err := h.Submit(ctx, in)
			if err != nil {
				b.Fatalf("Submit failed: %v", err)
			}
			got, err := h.Wait(ctx, ticket)
			if err != nil {
				b.Fatalf("Wait failed: %v", err)
			}
			if !bytes.Equal(got, want) {
				b.Fatalf("Wait returned %d bytes, want the %d it sent", len(got), n)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ticket, err := h.Submit(ctx, in)
				if err != nil {
					b.Fatalf("Submit failed: %v", err)
				}
				res, err := h.Wait(ctx, ticket)
				if err != nil {
					b.Fatalf("Wait failed: %v", err)
				}
				if len(res) != n {
					b.Fatalf("Wait returned %d bytes, want %d", len(res), n)
				}
			}
		})
	}
}

// BenchmarkGussetWaitBufferResultSize is Submit then WaitBuffer then Free at
// each result size: the zero-copy egress, where a result already in a Rust
// buffer is wrapped and one in Go memory is adopted when 64-byte aligned and
// copied into a new buffer otherwise.
func BenchmarkGussetWaitBufferResultSize(b *testing.B) {
	for _, n := range resultSizes {
		b.Run(fmt.Sprintf("%dB", n), func(b *testing.B) {
			h := openForResultSize(b)
			ctx := context.Background()
			in, release := echoInput(b, h, n)
			defer release()
			want := echoRequest(n)

			ticket, err := h.Submit(ctx, in)
			if err != nil {
				b.Fatalf("Submit failed: %v", err)
			}
			first, err := h.WaitBuffer(ctx, ticket)
			if err != nil {
				b.Fatalf("WaitBuffer failed: %v", err)
			}
			if !bytes.Equal(first.Bytes(), want) {
				b.Fatalf("WaitBuffer returned %d bytes, want the %d it sent", len(first.Bytes()), n)
			}
			_ = first.Free()

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ticket, err := h.Submit(ctx, in)
				if err != nil {
					b.Fatalf("Submit failed: %v", err)
				}
				out, err := h.WaitBuffer(ctx, ticket)
				if err != nil {
					b.Fatalf("WaitBuffer failed: %v", err)
				}
				if len(out.Bytes()) != n {
					b.Fatalf("WaitBuffer returned %d bytes, want %d", len(out.Bytes()), n)
				}
				_ = out.Free()
			}
		})
	}
}
