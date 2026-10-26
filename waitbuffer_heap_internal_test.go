package gusset

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"unsafe"
)

// echoInput is a diagnostic-engine request whose result is itself: mode 0
// echoes, so an n-byte request yields an n-byte result (n >= 1).
func echoInput(n int) []byte {
	in := make([]byte, n)
	for i := 1; i < n; i++ {
		in[i] = byte(i*13 + 5)
	}
	return in
}

func isAligned64(b []byte) bool {
	return len(b) == 0 || uintptr(unsafe.Pointer(unsafe.SliceData(b)))%64 == 0
}

// An inline result (<= inlineResultBytes) is carried in 64-byte aligned Go
// memory with id 0; anything larger stays a Rust take buffer. Both are used the
// same way: Bytes, Free (idempotent, Bytes nil after), Submit input,
// CallBuffer input. The Go-memory kind owns nothing in Rust.
func TestWaitBuffer_InlineResultIsGoMemoryWithTheSameContract(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()

	for _, n := range []int{1, 48, 256, 1024, inlineResultBytes} {
		t.Run(fmt.Sprintf("%dB", n), func(t *testing.T) {
			in := echoInput(n)
			tk, err := h.Submit(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			buf, err := h.WaitBuffer(ctx, tk)
			if err != nil {
				t.Fatal(err)
			}
			if buf.ID() != 0 {
				t.Fatalf("inline result of %d bytes has Rust id %d, want a Go-memory buffer (id 0)", n, buf.ID())
			}
			got := buf.Bytes()
			if !bytes.Equal(got, in) {
				t.Fatalf("Bytes() = %d bytes, want the %d-byte result", len(got), n)
			}
			if !isAligned64(got) {
				t.Fatal("result is not 64-byte aligned")
			}
			if buf.owner != h {
				t.Fatal("the Buffer does not keep its Handle reachable")
			}

			// Reused as input, by Submit and by CallBuffer: its bytes travel inline.
			tk2, err := h.Submit(ctx, buf)
			if err != nil {
				t.Fatalf("Submit(result buffer): %v", err)
			}
			out, err := h.Wait(ctx, tk2)
			if err != nil || !bytes.Equal(out, in) {
				t.Fatalf("Wait of a job fed the result buffer: %d bytes, err %v", len(out), err)
			}
			cb, err := h.CallBuffer(ctx, buf)
			if err != nil {
				t.Fatalf("CallBuffer(result buffer): %v", err)
			}
			if cb.ID() != 0 || !bytes.Equal(cb.Bytes(), in) {
				t.Fatalf("CallBuffer output: id %d, %d bytes", cb.ID(), len(cb.Bytes()))
			}
			if err := cb.Free(); err != nil {
				t.Fatal(err)
			}

			if err := buf.Free(); err != nil {
				t.Fatalf("Free: %v", err)
			}
			if err := buf.Free(); err != nil {
				t.Fatalf("second Free: %v", err)
			}
			if buf.Bytes() != nil {
				t.Fatal("Bytes() after Free must be nil")
			}
			if _, err := h.Submit(ctx, buf); err == nil || errors.Is(err, ErrClosed) {
				t.Fatalf("Submit of a freed result buffer on an open handle: %v, want a non-ErrClosed refusal", err)
			}
			// The handle is still fully usable.
			if out, err := h.Call(ctx, in); err != nil || !bytes.Equal(out, in) {
				t.Fatalf("Call after: %d bytes, err %v", len(out), err)
			}
		})
	}

	t.Run("over the inline limit stays Rust-owned", func(t *testing.T) {
		const n = inlineResultBytes + 1
		in, err := h.NewBuffer(n)
		if err != nil {
			t.Fatal(err)
		}
		defer in.Free()
		copy(in.Bytes(), echoInput(n))
		tk, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		out, err := h.WaitBuffer(ctx, tk)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Free()
		if out.ID() == 0 {
			t.Fatalf("a %d-byte result is a take buffer and must keep its Rust id", n)
		}
		if !bytes.Equal(out.Bytes(), echoInput(n)) {
			t.Fatal("result differs")
		}
	})
}

// An inline result already stored when Close begins is refused, as a take
// result is: claimLocked hands out a stored result before it looks at closed,
// and the Go-memory path takes no lock that would refuse it.
func TestWaitBuffer_InlineResultAfterCloseBeginsIsErrClosed(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	s := h.state
	ctx := context.Background()
	for _, n := range []int{0, 48, 1024} {
		in := echoInput(n)
		tk, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "the result to complete", func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			_, ok := s.completed[tk]
			return ok
		})
		s.closed.Store(true)
		_, err = h.WaitBuffer(ctx, tk)
		s.closed.Store(false)
		wantClosed(t, fmt.Sprintf("WaitBuffer of a stored %d-byte inline result after close began", n), err, "gusset: handle is closed")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

// A Go-memory result raced against Close: WaitBuffer either refuses with
// ErrClosed, or returns a Buffer whose bytes are intact, whose Free works, and
// whose slice, if one was taken before the close, stays valid after Close and a
// GC (Go memory, unlike a Rust view, is never released under it).
func TestWaitBuffer_InlineResultSurvivesConcurrentClose(t *testing.T) {
	const trials = 100
	for trial := 0; trial < trials; trial++ {
		h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
		if err != nil {
			t.Fatal(err)
		}
		in := echoInput(973)
		ctx := context.Background()
		tk, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}

		var (
			wg   sync.WaitGroup
			buf  *Buffer
			werr error
			got  []byte
		)
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			buf, werr = h.WaitBuffer(ctx, tk)
			if buf != nil {
				got = buf.Bytes()
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = h.Close()
		}()
		close(start)
		wg.Wait()
		_ = h.Close()

		if werr != nil {
			if !errors.Is(werr, ErrClosed) {
				t.Fatalf("trial %d: WaitBuffer raced with Close: %v, want a result or ErrClosed", trial, werr)
			}
			continue
		}
		runtime.GC()
		runtime.GC()
		if got != nil && !bytes.Equal(got, in) {
			t.Fatalf("trial %d: a slice taken before Close changed after it", trial)
		}
		if buf.ID() != 0 || !bytes.Equal(buf.data, in) {
			t.Fatalf("trial %d: result buffer id %d, intact=%v", trial, buf.ID(), bytes.Equal(buf.data, in))
		}
		if b := buf.Bytes(); b != nil {
			t.Fatalf("trial %d: Bytes after Close = %d bytes, want nil", trial, len(b))
		}
		if err := buf.Free(); err != nil {
			t.Fatalf("trial %d: Free after Close: %v", trial, err)
		}
		if err := buf.Free(); err != nil {
			t.Fatalf("trial %d: second Free: %v", trial, err)
		}
	}
}
