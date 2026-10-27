package gusset

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// boundaryEcho is the diagnostic request whose result is the same n bytes:
// mode 0 echoes its input, mode byte included.
func boundaryEcho(n int) []byte {
	in := make([]byte, n)
	for i := 1; i < n; i++ {
		in[i] = byte(i*13 + 5)
	}
	return in
}

// The sizes a result changes path at: 48 is the largest carried whole in the
// completion record, 49 the smallest stored and promoted onto a Rust buffer by
// the worker, 4096 and 4097 the old promotion threshold either side.
var boundarySizes = []int{48, 49, 4096, 4097}

// Every egress returns the bytes the engine produced, whichever side of a
// threshold the result falls: Call, Submit+Wait, Submit+WaitBuffer and
// CallBuffer, at 48/49/4096/4097 bytes.
func TestResultBoundary_IdenticalBytesThroughEveryEgress(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, n := range boundarySizes {
		t.Run(fmt.Sprintf("%dB", n), func(t *testing.T) {
			want := boundaryEcho(n)

			// A request over 4 KiB must travel in a Buffer.
			var in any = want
			var inBuf *Buffer
			if n > inlineResultBytes {
				var err error
				inBuf, err = h.NewBuffer(n)
				if err != nil {
					t.Fatal(err)
				}
				defer inBuf.Free()
				copy(inBuf.Bytes(), want)
				in = inBuf
			} else {
				b, err := h.NewBuffer(n)
				if err != nil {
					t.Fatal(err)
				}
				defer b.Free()
				copy(b.Bytes(), want)
				inBuf = b
			}

			if n <= inlineResultBytes {
				got, err := h.Call(ctx, want)
				if err != nil {
					t.Fatalf("Call: %v", err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("Call returned %d bytes that differ from the %d sent", len(got), n)
				}
			}

			tk, err := h.Submit(ctx, in)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			got, err := h.Wait(ctx, tk)
			if err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("Wait returned %d bytes that differ from the %d sent", len(got), n)
			}

			tk, err = h.Submit(ctx, in)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			wb, err := h.WaitBuffer(ctx, tk)
			if err != nil {
				t.Fatalf("WaitBuffer: %v", err)
			}
			if !bytes.Equal(wb.Bytes(), want) {
				t.Errorf("WaitBuffer returned %d bytes that differ from the %d sent", len(wb.Bytes()), n)
			}
			if err := wb.Free(); err != nil {
				t.Errorf("Free: %v", err)
			}

			cb, err := h.CallBuffer(ctx, inBuf)
			if err != nil {
				t.Fatalf("CallBuffer: %v", err)
			}
			if !bytes.Equal(cb.Bytes(), want) {
				t.Errorf("CallBuffer returned %d bytes that differ from the %d sent", len(cb.Bytes()), n)
			}
			if err := cb.Free(); err != nil {
				t.Errorf("Free: %v", err)
			}
		})
	}
}

// A stored result is held as a Rust take buffer exactly when it is over the
// record limit, not the 4 KiB submit limit: drainPipe parks the buffer id in
// takeIDs for 49 bytes as for 4097, and a 48-byte result never reaches
// takeIDs because it arrived in its record. This is the Go-side proof that the
// worker promoted, which the bytes alone cannot show.
func TestResultBoundary_TakeBufferHeldExactlyAboveTheRecordLimit(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, n := range []int{48, 49, 256, 4096} {
		want := boundaryEcho(n)
		tk, err := h.Submit(ctx, want)
		if err != nil {
			t.Fatalf("%dB: Submit: %v", n, err)
		}
		discardTestWait(t, s, "the result to be stored", func() bool {
			_, done := s.completed[tk]
			return done
		})
		s.mu.Lock()
		held := s.takeIDs[tk] != 0
		s.mu.Unlock()
		if wantHeld := n > ffi.InlineResultMax; held != wantHeld {
			t.Errorf("%dB: take buffer held = %v, want %v", n, held, wantHeld)
		}
		got, err := h.Wait(ctx, tk)
		if err != nil {
			t.Fatalf("%dB: Wait: %v", n, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%dB: Wait returned different bytes", n)
		}
		s.mu.Lock()
		if _, left := s.takeIDs[tk]; left {
			t.Errorf("%dB: Wait left the take buffer id behind", n)
		}
		s.mu.Unlock()
	}
}

// No egress leaks Rust memory at a boundary size: after every result is
// consumed, discarded, or abandoned, the allocator's live bytes return to where
// they started. Results of 49..4096 bytes were Go-heap copies before the worker
// promoted them, so each of these paths now owns a Rust buffer it must free.
func TestResultBoundary_NoRustBufferOutlivesItsResult(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	round := func() {
		for _, n := range boundarySizes {
			want := boundaryEcho(n)
			var in any = want
			if n > inlineResultBytes {
				b, err := h.NewBuffer(n)
				if err != nil {
					t.Fatal(err)
				}
				copy(b.Bytes(), want)
				defer b.Free()
				in = b
			}
			// Wait consumes the take buffer.
			tk, err := h.Submit(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.Wait(ctx, tk); err != nil {
				t.Fatal(err)
			}
			// WaitBuffer hands it to the caller, who frees it.
			tk, err = h.Submit(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			wb, err := h.WaitBuffer(ctx, tk)
			if err != nil {
				t.Fatal(err)
			}
			if err := wb.Free(); err != nil {
				t.Fatal(err)
			}
			// Discard frees a result stored but never waited on.
			tk, err = h.Submit(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.Discard(tk); err != nil {
				t.Fatal(err)
			}
		}
	}

	round() // warm: lazily created state is not a leak
	settleLive := func() uint64 {
		// A discarded ticket whose job is still running is freed when it lands.
		discardTestWait(t, h.state, "abandoned and stored results to clear", func() bool {
			return len(h.state.abandoned) == 0 && len(h.state.completed) == 0 && len(h.state.takeIDs) == 0
		})
		var live uint64
		for i := 0; i < 200; i++ {
			live = Stats().LiveBytes
			time.Sleep(5 * time.Millisecond)
			if next := Stats().LiveBytes; next == live {
				return live
			}
		}
		return live
	}
	base := settleLive()
	for i := 0; i < 20; i++ {
		round()
	}
	if after := settleLive(); after > base {
		t.Fatalf("live Rust bytes grew from %d to %d over 20 rounds at the boundary sizes", base, after)
	}
}
