package shutdown_incomplete_test

// gusset.Shutdown with work it cannot drain.
//
// Its own package, so its own test binary: Shutdown latches a process-global
// flag that refuses every later submission and is cleared only by package init,
// so a test that calls it cannot share a process with any test that submits
// afterwards (the same reason tests/shutdown and tests/rust_shutdown.rs are
// binaries of their own). tests/shutdown cannot cover this: its work is
// cooperative, so every drain there is clean, and its comment on
// TestShutdown_ReportsWorkItCouldNotDrain says "nothing in flight" and accepts
// a nil error. A Shutdown that returned nil with undrained work passed it.
//
// Diagnostic mode 9 sleeps without calling JobContext::check, which is the
// shape of a real adopter engine and the only kind of work Shutdown cannot
// drain. The Rust half of this contract is
// tests/rust_shutdown.rs (shutdown_reports_non_cooperative_work_that_outlives_its_budget).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// shutdownWithin runs Shutdown(d) and fails the test if it does not return
// within guard. A clamp that is wrong in the other direction (a negative budget
// read as 49 days) would otherwise hang the binary until the go test timeout.
func shutdownWithin(t *testing.T, d time.Duration, guard time.Duration) (time.Duration, error) {
	t.Helper()
	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		err := gusset.Shutdown(d)
		done <- result{err, time.Since(start)}
	}()
	select {
	case r := <-done:
		return r.elapsed, r.err
	case <-time.After(guard):
		t.Fatalf("Shutdown(%v) did not return within %v", d, guard)
		return 0, nil
	}
}

func TestShutdown_ReportsWorkItCannotDrain(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Mode 9, 150 units of 10 ms: 1.5 s of work that never checks its
	// context, so the cancel Shutdown issues cannot stop it.
	ticket, err := h.Submit(ctx, []byte{9, 150})
	if err != nil {
		if errors.Is(err, gusset.ErrShutdown) {
			t.Skip("runtime already shut down in this process (-count > 1): the latch is one-way")
		}
		t.Fatalf("Submit: %v", err)
	}

	// Let the worker take the job and start its sleep. A unit still queued when
	// Shutdown sets its cancel flag is cancelled before the engine runs and
	// drains at once, which would make this a test of queueing instead of an
	// engine that ignores the flag.
	time.Sleep(200 * time.Millisecond)

	// 1. A budget the work outlives is reported, not rounded up to success.
	elapsed, err := shutdownWithin(t, 10*time.Millisecond, 10*time.Second)
	if !errors.Is(err, gusset.ErrShutdownIncomplete) {
		t.Fatalf("Shutdown(10ms) with a second of non-cooperative work in flight: want ErrShutdownIncomplete, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Shutdown(10ms) took %v", elapsed)
	}

	// 2. A negative budget is zero, not a wrapped uint32 of 49 days: it
	// answers at once, and with the work still in flight it says so.
	elapsed, err = shutdownWithin(t, -time.Hour, 10*time.Second)
	if !errors.Is(err, gusset.ErrShutdownIncomplete) {
		t.Fatalf("Shutdown(-1h) with work in flight: want ErrShutdownIncomplete, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Shutdown(-1h) took %v: a negative budget must clamp to zero", elapsed)
	}

	// 3. A budget past MaxDrain is capped to MaxDrain, not wrapped. MaxDrain
	// plus 100 ms wraps to a 99 ms budget in a uint32 of milliseconds, which
	// the second of work outlives, so a Shutdown that did not clamp would
	// report ErrShutdownIncomplete here. Clamped, it waits the work out.
	elapsed, err = shutdownWithin(t, gusset.MaxDrain+100*time.Millisecond, 30*time.Second)
	if err != nil {
		t.Fatalf("Shutdown(MaxDrain+100ms) must wait for the work, not wrap to a short budget; got %v after %v", err, elapsed)
	}

	// The job was never killed. Collect it so Close has nothing outstanding;
	// it finished, or was cut off by the latch, and either is an answer.
	if _, err := h.Wait(ctx, ticket); err != nil && !errors.Is(err, gusset.ErrShutdown) {
		t.Fatalf("Wait on the drained job: %v", err)
	}
}
