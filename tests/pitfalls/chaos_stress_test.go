package pitfalls_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestHardening_NilReceiverSafety tests that calling methods on nil *Handle or
// nil *Buffer does not crash with a nil pointer dereference.
func TestHardening_NilReceiverSafety(t *testing.T) {
	var h *gusset.Handle
	ctx := context.Background()

	if err := h.Close(); err == nil {
		t.Error("expected error on (*Handle)(nil).Close(), got nil")
	}
	if _, err := h.Call(ctx, []byte{0}); err == nil {
		t.Error("expected error on (*Handle)(nil).Call(), got nil")
	}
	if _, err := h.Submit(ctx, []byte{0}); err == nil {
		t.Error("expected error on (*Handle)(nil).Submit(), got nil")
	}
	if _, err := h.Wait(ctx, 1); err == nil {
		t.Error("expected error on (*Handle)(nil).Wait(), got nil")
	}
	if _, err := h.WaitBuffer(ctx, 1); err == nil {
		t.Error("expected error on (*Handle)(nil).WaitBuffer(), got nil")
	}
	if _, err := h.NewBuffer(1024); err == nil {
		t.Error("expected error on (*Handle)(nil).NewBuffer(), got nil")
	}

	var b *gusset.Buffer
	if got := b.Bytes(); got != nil {
		t.Errorf("expected nil for (*Buffer)(nil).Bytes(), got %v", got)
	}
	if got := b.ID(); got != 0 {
		t.Errorf("expected 0 for (*Buffer)(nil).ID(), got %d", got)
	}
	if err := b.Free(); err != nil {
		t.Errorf("expected nil error for (*Buffer)(nil).Free(), got %v", err)
	}
}

// TestPitfall_NilContextIsRejected pins the boundary against a nil context.
//
// Call/Submit/Wait pass ctx to select on ctx.Done() and to extractCallHeader.
// A nil context panics there and takes the calling goroutine down, which is a
// process-level failure for a library that exists to keep the Go process alive.
func TestPitfall_NilContextIsRejected(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("nil context panicked the process: %v", rec)
		}
	}()

	if _, err := h.Call(nil, []byte{0}); err == nil {
		t.Fatal("Call(nil, ...) must be rejected")
	}
	if _, err := h.Submit(nil, []byte{0}); err == nil {
		t.Fatal("Submit(nil, ...) must be rejected")
	}
	if _, err := h.Wait(nil, 1); err == nil {
		t.Fatal("Wait(nil, ...) must be rejected")
	}
	if _, err := h.WaitBuffer(nil, 1); err == nil {
		t.Fatal("WaitBuffer(nil, ...) must be rejected")
	}
}

// TestStress_MassiveConcurrentChaos hammers Submit/Wait/WaitBuffer/Call/cancel
// and Close against one handle, then against handle churn, and fails on any
// unexpected error or hang. Poisoning is isolated: a panic byte is not mixed
// into the healthy-handle swarm, because that would turn every later error into
// ErrPoisoned and hide I4/I3 failures.
func TestStress_MassiveConcurrentChaos(t *testing.T) {
	const numGoroutines = 32
	const iterationsPerGoroutine = 20

	h, err := gusset.Open(gusset.WithPoolSize(8), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	var wg sync.WaitGroup
	var completed atomic.Int64
	var cancelled atomic.Int64

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iterationsPerGoroutine; i++ {
				switch (gid + i) % 4 {
				case 0:
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					res, err := h.Call(ctx, []byte{0, byte(gid), byte(i)})
					cancel()
					if err != nil {
						if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
							cancelled.Add(1)
							return
						}
						t.Errorf("Call: %v", err)
						return
					}
					if len(res) < 3 || res[1] != byte(gid) || res[2] != byte(i) {
						t.Errorf("corrupted Call response: %v", res)
						return
					}
					completed.Add(1)

				case 1:
					buf, err := h.NewBuffer(512)
					if err != nil {
						t.Errorf("NewBuffer: %v", err)
						return
					}
					b := buf.Bytes()
					b[0] = 0
					b[1] = byte(gid)
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					ticket, err := h.Submit(ctx, buf)
					if err != nil {
						cancel()
						_ = buf.Free()
						t.Errorf("Submit buffer: %v", err)
						return
					}
					res, err := h.Wait(ctx, ticket)
					cancel()
					_ = buf.Free()
					if err != nil {
						if errors.Is(err, context.DeadlineExceeded) {
							cancelled.Add(1)
							return
						}
						t.Errorf("Wait: %v", err)
						return
					}
					if len(res) < 2 || res[1] != byte(gid) {
						t.Errorf("corrupted buffer echo: %v", res)
						return
					}
					completed.Add(1)

				case 2:
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					ticket, err := h.Submit(ctx, []byte{0, 42, 99})
					if err != nil {
						cancel()
						t.Errorf("Submit: %v", err)
						return
					}
					outBuf, err := h.WaitBuffer(ctx, ticket)
					cancel()
					if err != nil {
						if errors.Is(err, context.DeadlineExceeded) {
							cancelled.Add(1)
							return
						}
						t.Errorf("WaitBuffer: %v", err)
						return
					}
					got := outBuf.Bytes()
					if len(got) != 3 || got[1] != 42 || got[2] != 99 {
						t.Errorf("corrupted WaitBuffer: %v", got)
					}
					if err := outBuf.Free(); err != nil {
						t.Errorf("WaitBuffer Free: %v", err)
					}
					completed.Add(1)

				case 3:
					ctx, cancel := context.WithTimeout(context.Background(), 500*time.Microsecond)
					_, err := h.Call(ctx, []byte{5, 8})
					cancel()
					if err != nil {
						cancelled.Add(1)
					} else {
						completed.Add(1)
					}
				}
			}
		}(g)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("chaos swarm hung")
	}

	t.Logf("chaos: completed=%d cancelled=%d", completed.Load(), cancelled.Load())
	if completed.Load() == 0 {
		t.Fatal("chaos completed no successful calls")
	}

	// Contract probes mixed into the same handle after the swarm: these fail
	// closed rather than hang or copy a megabyte on the cgo thread.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := h.Call(ctx, make([]byte, 4097)); err == nil {
		t.Fatal("chaos: 4097-byte []byte must be refused (R16)")
	}
	if _, err := h.Wait(ctx, 1<<40); !errors.Is(err, gusset.ErrUnknownTicket) {
		t.Fatalf("chaos: Wait on a never-issued ticket must be ErrUnknownTicket, got %v", err)
	}
}

// TestHardening_ConcurrentSubmitClosePermitIntegrity races Submit/Wait against
// Close across handle lifecycles. The assertion is that nothing hangs: Close
// must drain every permit it still owns, and a waiter must not block forever
// on a handle that has already shut its completion pipe.
func TestHardening_ConcurrentSubmitClosePermitIntegrity(t *testing.T) {
	for trial := 0; trial < 10; trial++ {
		h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("trial %d: Open failed: %v", trial, err)
		}

		var wg sync.WaitGroup
		const submitters = 20
		hung := atomic.Bool{}
		for s := 0; s < submitters; s++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 25; i++ {
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					ticket, submitErr := h.Submit(ctx, []byte{0, 1, 2})
					if submitErr == nil {
						_, _ = h.Wait(ctx, ticket)
					}
					cancel()
				}
			}()
		}

		time.Sleep(2 * time.Millisecond)
		if err := h.Close(); err != nil {
			t.Fatalf("trial %d: Close failed: %v", trial, err)
		}

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			hung.Store(true)
		}
		if hung.Load() {
			t.Fatalf("trial %d: Submit/Wait hung after Close", trial)
		}
	}
}

// TestPitfall_WaitBufferOfLargeResultDoesNotDangleAcrossClose is the large-result
// twin of TestStress_ConcurrentZeroCopyEgressAndCloseRace.
//
// That test submits 3-byte payloads, so drainPipe copies onto the Go heap and
// WaitBuffer allocates a fresh Buffer. The take-buffer path (R16 egress, >4 KiB)
// stores a Go slice over Rust memory in completed/takeIDs and wraps it later
// without holding cgoMu. Close can HandleClose that memory between waitInternal
// returning and newBufferFromRaw, and WaitBuffer then returns (buf, nil) whose
// Bytes() is nil — success wrapping freed pages. The existing close-race used
// payloads too small to take that path.
func TestPitfall_WaitBufferOfLargeResultDoesNotDangleAcrossClose(t *testing.T) {
	const payload = 8192
	for trial := 0; trial < 8; trial++ {
		h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("trial %d: Open failed: %v", trial, err)
		}

		const submitters = 16
		var wg sync.WaitGroup
		var badSuccess atomic.Int64
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)

		for s := 0; s < submitters; s++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				in, err := h.NewBuffer(payload)
				if err != nil {
					return
				}
				defer in.Free()
				b := in.Bytes()
				if b == nil {
					return
				}
				b[0] = 0
				for i := 1; i < len(b); i++ {
					b[i] = byte(i)
				}
				ticket, err := h.Submit(ctx, in)
				if err != nil {
					return
				}
				out, err := h.WaitBuffer(ctx, ticket)
				if err != nil {
					return
				}
				defer out.Free()
				got := out.Bytes()
				// nil Bytes after a successful wrap is Close winning the API
				// contract (handle gone → view withdrawn). The bug is a
				// non-nil view of the wrong length or torn contents: that is
				// a wrap of already-freed pages, which the 3-byte close-race
				// never reached because drainPipe copied those onto the Go heap.
				if got == nil {
					return
				}
				if len(got) != payload {
					badSuccess.Add(1)
					t.Errorf("WaitBuffer succeeded with a truncated view: len=%d", len(got))
					return
				}
				first := got[100]
				// This test used to read got after out.Free() and while Close
				// ran, which the Bytes contract forbids: it measured its own
				// use-after-free (about 1 run in 30, on main as well). Close
				// sets closed before it frees anything, so a non-nil Bytes()
				// after the read proves the read saw live memory; only then
				// is a wrong byte the wrap-of-freed-pages bug this test hunts.
				if out.Bytes() == nil {
					return
				}
				if first != byte(100) {
					badSuccess.Add(1)
					t.Errorf("WaitBuffer succeeded with corrupted bytes: got[100]=%d", first)
				}
			}()
		}

		time.Sleep(time.Duration(2+trial) * time.Millisecond)
		_ = h.Close()
		cancel()

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Fatalf("trial %d: WaitBuffer hung after Close", trial)
		}
		if badSuccess.Load() > 0 {
			t.Fatalf("trial %d: WaitBuffer returned success wrapping freed Rust memory", trial)
		}
	}
}

// TestPitfall_BufferSizeIsBounded pins the allocation ceiling.
//
// NewBuffer used to feed the caller's int straight into posix_memalign. A
// production caller that passed 1<<40 either truncated or asked the OS for
// more memory than the process can hold. The ceiling is refused, not clamped,
// matching WithPoolSize.
func TestPitfall_BufferSizeIsBounded(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	if _, err := h.NewBuffer(gusset.MaxBufferBytes + 1); err == nil {
		t.Fatal("NewBuffer above MaxBufferBytes must be refused, not allocated")
	} else if !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("refusal must name the maximum, got: %v", err)
	}

	buf, err := h.NewBuffer(64)
	if err != nil {
		t.Fatalf("NewBuffer(64) must still succeed, got %v", err)
	}
	if err := buf.Free(); err != nil {
		t.Fatalf("Free failed: %v", err)
	}
}
