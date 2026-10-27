package gusset

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestTakeResult_CloseCannotFreeTheViewBeingUsed pins the Rust-owned half of I1
// for a result the library hands to a waiter.
//
// A result over 4 KiB is promoted onto a Rust buffer by the worker and reaches
// the waiter as a Go slice over that buffer. Wait copies it out and WaitBuffer
// wraps it; both read Rust memory that Close frees, and Close frees it under
// cgoMu held exclusively. The v0.0.3 write-after-free (CHANGELOG 0.0.3, 38f0296)
// was the same shape on the path WaitBuffer no longer has for inline results: a
// Rust buffer used after cgoMu was released. TestWaitBuffer_CloseCannotFreeTheResultBeingFilled
// stopped asserting lock ordering when an inline result moved to Go memory, so
// this test holds the ordering for the paths that still touch Rust: a Close that
// starts while the view is about to be used must not finish until the view has
// been copied or wrapped.
//
// Before this test, the take paths were covered only by races
// (TestPitfall_WaitBufferOfLargeResultDoesNotDangleAcrossClose and the stress
// hammers), which pass whenever the scheduler does not land a Close in the gap;
// the hook holds the gap open every run.
func TestTakeResult_CloseCannotFreeTheViewBeingUsed(t *testing.T) {
	for _, size := range []int{inlineResultBytes + 1, 16 << 10} {
		for _, viaBuffer := range []bool{false, true} {
			name := "Wait"
			if viaBuffer {
				name = "WaitBuffer"
			}
			t.Run(name+"/"+sizeName(size), func(t *testing.T) {
				h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
				if err != nil {
					t.Fatal(err)
				}
				s := h.state
				ctx := context.Background()
				want := echoInput(size)
				in, err := h.NewBuffer(size)
				if err != nil {
					t.Fatal(err)
				}
				copy(in.Bytes(), want)
				tk, err := h.Submit(ctx, in)
				if err != nil {
					t.Fatal(err)
				}

				hooked := false
				closeFinishedFirst := false
				testHookBeforeTakeView = func(s *handleState) {
					hooked = true
					go func() { _ = h.Close() }()
					select {
					case <-s.closeDone:
						closeFinishedFirst = true
					case <-time.After(500 * time.Millisecond):
					}
				}
				defer func() { testHookBeforeTakeView = nil }()

				var (
					out  []byte
					buf  *Buffer
					werr error
				)
				if viaBuffer {
					buf, werr = h.WaitBuffer(ctx, tk)
				} else {
					out, werr = h.Wait(ctx, tk)
				}
				testHookBeforeTakeView = nil
				if !hooked {
					t.Fatalf("the view hook never ran (%s: %v); the result did not take the Rust buffer path", name, werr)
				}
				if closeFinishedFirst {
					t.Fatalf("Close freed the handle's Rust buffers while %s had yet to use the take buffer: the use reads freed memory", name)
				}
				if werr != nil {
					t.Fatalf("%s: %v", name, werr)
				}
				if viaBuffer {
					if buf.ID() == 0 {
						t.Fatal("a result over 4 KiB is Go memory; it should be the Rust take buffer")
					}
				} else if !bytes.Equal(out, want) {
					t.Fatalf("Wait copied a corrupted result: len %d, want %d", len(out), len(want))
				}

				// The use is over, so Close can finish. The buffer is Rust
				// memory Close has freed or is about to free, so it is read
				// only through Bytes, which withdraws it.
				select {
				case <-s.closeDone:
				case <-time.After(5 * time.Second):
					t.Fatal("Close did not finish once the take buffer was released")
				}
				if viaBuffer {
					if b := buf.Bytes(); b != nil {
						t.Fatalf("Bytes after Close = %d bytes, want nil", len(b))
					}
					if err := buf.Free(); err != nil {
						t.Fatalf("Free after Close: %v", err)
					}
				}
				_ = in.Free()
			})
		}
	}
}

func sizeName(n int) string {
	if n == inlineResultBytes+1 {
		return "smallest-promoted"
	}
	return "16KiB"
}
