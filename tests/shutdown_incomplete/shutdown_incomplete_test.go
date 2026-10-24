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
// Diagnostic mode 17 holds without calling JobContext::check, which is the
// shape of a real adopter engine and the only kind of work Shutdown cannot
// drain. The Rust half of this contract is
// tests/rust_shutdown.rs (shutdown_reports_non_cooperative_work_that_outlives_its_budget).

import (
	"context"
	"encoding/binary"
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

// engineStartWait bounds the wait for the engine to take the job.
const engineStartWait = 10 * time.Second

// waitUntilTheEngineIsRunning returns once a diagnostic mode 17 job is inside
// the engine. Mode 18 reads the peak of the engine's process-wide in-flight
// gauge since the last read; only the engine raises it, so a peak of 1 or more
// means a worker has dequeued a job and entered the engine. Bounded: a job that
// never starts fails the test with that message instead of hanging it.
func waitUntilTheEngineIsRunning(ctx context.Context, t *testing.T, h *gusset.Handle) {
	t.Helper()
	deadline := time.Now().Add(engineStartWait)
	for {
		out, err := h.Call(ctx, []byte{18})
		if err != nil {
			t.Fatalf("reading the engine gauge (mode 18): %v", err)
		}
		if len(out) != 8 {
			t.Fatalf("engine gauge answered %d bytes, want 8", len(out))
		}
		if binary.LittleEndian.Uint64(out) >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no worker entered the engine within %v: the job was never started", engineStartWait)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestShutdown_ReportsWorkItCannotDrain(t *testing.T) {
	// Two workers: one holds the job, the other answers the gauge read below.
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Mode 17, 250 ms (the most its byte allows): work that never checks its
	// context, so the cancel Shutdown issues cannot stop it, and that counts
	// itself in the engine's in-flight gauge.
	ticket, err := h.Submit(ctx, []byte{17, 250})
	if err != nil {
		if errors.Is(err, gusset.ErrShutdown) {
			t.Skip("runtime already shut down in this process (-count > 1): the latch is one-way")
		}
		t.Fatalf("Submit: %v", err)
	}

	// Shutdown only meets a job the engine is running. A unit still queued when
	// it sets its cancel flag is cancelled before the engine runs and drains at
	// once, which would make this a test of queueing instead of an engine that
	// ignores the flag. Wait for the engine's own signal rather than a guess.
	waitUntilTheEngineIsRunning(ctx, t, h)

	// 1. A budget the work outlives is reported, not rounded up to success.
	elapsed, err := shutdownWithin(t, 10*time.Millisecond, 10*time.Second)
	if !errors.Is(err, gusset.ErrShutdownIncomplete) {
		t.Fatalf("Shutdown(10ms) with non-cooperative work in flight: want ErrShutdownIncomplete, got %v", err)
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
	// the remaining ~200 ms of work outlives, so a Shutdown that did not clamp would
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
