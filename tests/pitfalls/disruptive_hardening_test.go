package pitfalls_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestDisrupt_CrossHandleIsolationAttacks attacks handle isolation by attempting
// to pass tickets and buffers allocated on Handle A into Handle B under concurrent execution.
func TestDisrupt_CrossHandleIsolationAttacks(t *testing.T) {
	hA, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open hA failed: %v", err)
	}
	defer hA.Close()

	hB, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open hB failed: %v", err)
	}
	defer hB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Submit on A, try to Wait on B
	ticketA, err := hA.Submit(ctx, []byte{0, 1, 2, 3})
	if err != nil {
		t.Fatalf("hA.Submit failed: %v", err)
	}

	_, err = hB.Wait(ctx, ticketA)
	if err == nil {
		t.Fatal("expected hB.Wait on ticketA to fail, got nil")
	}
	if !errors.Is(err, gusset.ErrUnknownTicket) {
		t.Fatalf("expected ErrUnknownTicket, got: %v", err)
	}

	// Ticket A should still be waitable on A
	resA, err := hA.Wait(ctx, ticketA)
	if err != nil {
		t.Fatalf("hA.Wait on ticketA failed: %v", err)
	}
	if len(resA) != 4 || resA[1] != 1 {
		t.Fatalf("unexpected resA: %v", resA)
	}

	// 2. Allocate Buffer on A, try to submit to B
	bufA, err := hA.NewBuffer(64)
	if err != nil {
		t.Fatalf("hA.NewBuffer failed: %v", err)
	}
	defer bufA.Free()

	_, err = hB.Submit(ctx, bufA)
	if err == nil {
		t.Fatal("expected hB.Submit(bufA) to fail, got nil")
	}
	if !errors.Is(err, errors.New("gusset: buffer belongs to a different handle")) &&
		err.Error() != "gusset: buffer belongs to a different handle" {
		t.Fatalf("expected 'buffer belongs to a different handle', got: %v", err)
	}

	_, err = hB.CallBuffer(ctx, bufA)
	if err == nil {
		t.Fatal("expected hB.CallBuffer(bufA) to fail, got nil")
	}
	if err.Error() != "gusset: buffer belongs to a different handle" {
		t.Fatalf("expected 'buffer belongs to a different handle', got: %v", err)
	}
}

// TestDisrupt_ConcurrentPoisonChurnAssault runs 16 concurrent goroutines creating handles,
// deliberately inducing panics, verifying immediate handle poisoning, verifying no contagion
// across other concurrent handles, and cleanly closing.
func TestDisrupt_ConcurrentPoisonChurnAssault(t *testing.T) {
	const routines = 16
	const roundsPerRoutine = 10
	var wg sync.WaitGroup

	for r := 0; r < routines; r++ {
		wg.Add(1)
		go func(routineID int) {
			defer wg.Done()
			for round := 0; round < roundsPerRoutine; round++ {
				h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
				if err != nil {
					t.Errorf("routine %d round %d: Open failed: %v", routineID, round, err)
					return
				}

				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)

				// Normal echo should succeed first
				res, err := h.Call(ctx, []byte{0, 42})
				if err != nil {
					cancel()
					h.Close()
					t.Errorf("routine %d round %d: pre-panic Call failed: %v", routineID, round, err)
					return
				}
				if len(res) != 2 || res[1] != 42 {
					cancel()
					h.Close()
					t.Errorf("routine %d round %d: bad echo: %v", routineID, round, res)
					return
				}

				// Trigger deliberate panic (Mode 1: plain panic, or Mode 2: NUL byte panic)
				panicMode := byte(1 + (routineID+round)%3)
				_, err = h.Call(ctx, []byte{panicMode})
				if err == nil {
					cancel()
					h.Close()
					t.Errorf("routine %d round %d: expected panic error, got nil", routineID, round)
					return
				}
				if !errors.Is(err, gusset.ErrPanic) {
					cancel()
					h.Close()
					t.Errorf("routine %d round %d: expected ErrPanic, got: %v", routineID, round, err)
					return
				}

				// Handle MUST be permanently poisoned now (I2)
				for i := 0; i < 5; i++ {
					_, postErr := h.Call(ctx, []byte{0, 99})
					if !errors.Is(postErr, gusset.ErrPoisoned) {
						cancel()
						h.Close()
						t.Errorf("routine %d round %d: expected ErrPoisoned, got: %v", routineID, round, postErr)
						return
					}
				}

				cancel()
				if closeErr := h.Close(); closeErr != nil {
					t.Errorf("routine %d round %d: Close failed: %v", routineID, round, closeErr)
					return
				}
			}
		}(r)
	}

	wg.Wait()
}

// TestDisrupt_BufferBoundaryAttacks tests negative sizes, zero sizes, sizes exceeding 1 GiB,
// double frees, and accessing Bytes() after Free().
func TestDisrupt_BufferBoundaryAttacks(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// 1. Zero size
	if _, err := h.NewBuffer(0); err == nil {
		t.Fatal("expected NewBuffer(0) to fail, got nil")
	}

	// 2. Negative size
	if _, err := h.NewBuffer(-5); err == nil {
		t.Fatal("expected NewBuffer(-5) to fail, got nil")
	}

	// 3. Above MaxBufferBytes (1 GiB)
	if _, err := h.NewBuffer(gusset.MaxBufferBytes + 1); err == nil {
		t.Fatal("expected NewBuffer(MaxBufferBytes + 1) to fail, got nil")
	}

	// 4. Double free is a safe no-op
	buf, err := h.NewBuffer(64)
	if err != nil {
		t.Fatalf("NewBuffer(64) failed: %v", err)
	}
	if err := buf.Free(); err != nil {
		t.Fatalf("first Free() failed: %v", err)
	}
	// Bytes() must immediately return nil after Free()
	if slice := buf.Bytes(); slice != nil {
		t.Fatalf("expected nil from Bytes() after Free, got %v", slice)
	}
	// Second Free must return nil without panicking or double-freeing
	if err := buf.Free(); err != nil {
		t.Fatalf("second Free() failed: %v", err)
	}

	// 5. Free after Handle.Close()
	h2, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open h2 failed: %v", err)
	}
	buf2, err := h2.NewBuffer(32)
	if err != nil {
		t.Fatalf("NewBuffer(32) failed: %v", err)
	}
	_ = h2.Close()
	// Bytes() must return nil because handle is closed
	if slice := buf2.Bytes(); slice != nil {
		t.Fatalf("expected nil from Bytes() on closed handle, got %v", slice)
	}
	// Free() on closed handle must succeed cleanly without UAF
	if err := buf2.Free(); err != nil {
		t.Fatalf("buf2.Free() on closed handle failed: %v", err)
	}
}

// TestDisrupt_NonCooperativeEngineAbandonmentUnderHighConcurrency tests that when non-cooperative
// engines (Mode 9) sleep past caller deadlines, the callers are detached promptly at their
// context deadline, the permits are not leaked, and subsequent calls succeed.
func TestDisrupt_NonCooperativeEngineAbandonmentUnderHighConcurrency(t *testing.T) {
	const poolSize = 4
	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// Mode 9 with input[1]=5 sleeps for 50ms without calling ctx.check()
	// Submit 8 concurrent calls with a tight deadline of 10ms
	const calls = 8
	var wg sync.WaitGroup
	var timeoutCount atomic.Int32

	start := time.Now()
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
			defer cancel()

			_, err := h.Call(ctx, []byte{9, 5, 0, byte(id)})
			if err != nil && errors.Is(err, context.DeadlineExceeded) {
				timeoutCount.Add(1)
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)

	// All callers should have detached within ~30ms, not waited for 50ms+
	if timeoutCount.Load() != calls {
		t.Fatalf("expected %d DeadlineExceeded errors, got %d", calls, timeoutCount.Load())
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("callers were not detached promptly: took %v", elapsed)
	}

	// Wait for the workers to finish sleeping (50ms) so permits are reclaimed
	time.Sleep(80 * time.Millisecond)

	// Now all pool permits should be restored and normal calls should succeed instantly
	normalCtx, normalCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer normalCancel()

	for i := 0; i < poolSize*2; i++ {
		res, err := h.Call(normalCtx, []byte{0, byte(i)})
		if err != nil {
			t.Fatalf("post-abandonment call %d failed: %v", i, err)
		}
		if len(res) != 2 || res[1] != byte(i) {
			t.Fatalf("post-abandonment call %d returned corrupted response: %v", i, res)
		}
	}
}
