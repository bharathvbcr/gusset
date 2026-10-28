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
	"github.com/bharathvbcr/gusset/internal/hostprobe"
)

// shutdownWithin runs Shutdown(d) and fails the test if it does not return
// within guard. A clamp that is wrong in the other direction (a negative budget
// read as 49 days) would otherwise hang the binary until the go test timeout.
//
// It also returns the instant the goroutine took just before calling Shutdown,
// which is the latest the call's first read of the in-flight count can precede:
// a caller that needs the job to have been running at that read compares it, not
// the time the caller itself started waiting, with the job's floor.
func shutdownWithin(t *testing.T, d time.Duration, guard time.Duration) (time.Duration, time.Time, error) {
	t.Helper()
	type result struct {
		err     error
		elapsed time.Duration
		called  time.Time
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		err := gusset.Shutdown(d)
		done <- result{err, time.Since(start), start}
	}()
	select {
	case r := <-done:
		return r.elapsed, r.called, r.err
	case <-time.After(guard):
		t.Fatalf("Shutdown(%v) did not return within %v", d, guard)
		return 0, time.Time{}, nil
	}
}

// engineStartWait bounds the wait for the engine to take the job.
const engineStartWait = 10 * time.Second

// readEngineGauge reads diagnostic mode 18: the peak of the engine's
// process-wide in-flight gauge since the last read. A read resets the peak to
// the current level, so one taken while no mode 17 job is in the engine zeroes
// it.
func readEngineGauge(ctx context.Context, t *testing.T, h *gusset.Handle) uint64 {
	t.Helper()
	out, err := h.Call(ctx, []byte{18})
	if err != nil {
		t.Fatalf("reading the engine gauge (mode 18): %v", err)
	}
	if len(out) != 8 {
		t.Fatalf("engine gauge answered %d bytes, want 8", len(out))
	}
	return binary.LittleEndian.Uint64(out)
}

// waitUntilTheEngineIsRunning returns once a diagnostic mode 17 job is inside
// the engine. Only the engine raises the gauge, so a peak of 1 or more means a
// worker has dequeued a job and entered the engine. Bounded: a job that never
// starts fails the test with that message instead of hanging it.
func waitUntilTheEngineIsRunning(ctx context.Context, t *testing.T, h *gusset.Handle) {
	t.Helper()
	deadline := time.Now().Add(engineStartWait)
	for {
		if readEngineGauge(ctx, t, h) >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no worker entered the engine within %v: the job was never started", engineStartWait)
		}
		time.Sleep(time.Millisecond)
	}
}

const (
	// engineHold is how long diagnostic mode 17 holds the job at the least: its
	// argument is a byte, so 250 ms is the most one job can hold, and a sleep is
	// never shorter than asked. The hold starts at some instant after Submit
	// began, so the job is certainly still in the engine until Submit's start plus
	// engineHold, on any host.
	engineHold = 250 * time.Millisecond

	// startShutdownBy is how long after Submit the first Shutdown may be called.
	// The three calls below take about 15 ms together and the third one needs
	// the job to still be running when it starts, so a run that begins this late
	// still has most of the hold to spare; a run that begins later was stalled.
	// It only chooses between running again and going on, never a verdict: every
	// answer below is judged against engineHold.
	startShutdownBy = 100 * time.Millisecond

	// maxAttempts bounds the runs started again because the host stalled one
	// before it reached Shutdown.
	maxAttempts = 10
)

func TestShutdown_ReportsWorkItCannotDrain(t *testing.T) {
	// Two workers: one holds the job, the other answers the gauge read below.
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// What this test claims depends on the job still being in the engine when
	// each Shutdown reads it, and a stalled host can let a 250 ms job end first.
	// That must neither fail a correct Shutdown nor let an unclamped one pass on
	// a job that was already gone. Shutdown is one-way, so the stall is dealt with
	// in two places. Before the first call, a late start is discarded and the
	// job run again. After it, the job is certainly running while less than
	// engineHold has passed since Submit, so an unexpected answer in that time is
	// a failure and one after it is the host's (hostprobe.Skip), never a pass.
	var ticket uint64
	var submitted time.Time
	for attempt := 1; ; attempt++ {
		if attempt > maxAttempts {
			hostprobe.Skip(t, "the host stalled %d runs past the point where Shutdown still meets the job: the check could not run", maxAttempts)
			return
		}
		// A previous run's job left its peak in the gauge, which would pass for
		// this job having started.
		readEngineGauge(ctx, t, h)

		// Mode 17, 250 ms (the most its byte allows): work that never checks its
		// context, so the cancel Shutdown issues cannot stop it, and that counts
		// itself in the engine's in-flight gauge.
		submitted = time.Now()
		ticket, err = h.Submit(ctx, []byte{17, 250})
		if err != nil {
			if errors.Is(err, gusset.ErrShutdown) {
				t.Skip("runtime already shut down in this process (-count > 1): the latch is one-way")
			}
			t.Fatalf("Submit: %v", err)
		}

		// Shutdown only meets a job the engine is running. A unit still queued
		// when it sets its cancel flag is cancelled before the engine runs and
		// drains at once, which would make this a test of queueing instead of an
		// engine that ignores the flag. Wait for the engine's own signal rather
		// than a guess.
		waitUntilTheEngineIsRunning(ctx, t, h)
		if time.Since(submitted) < startShutdownBy {
			break
		}
		// Nothing has latched yet: collect this job and run the scenario again.
		if _, err := h.Wait(ctx, ticket); err != nil {
			t.Fatalf("Wait on a job the test discarded after a stall: %v", err)
		}
	}

	// unexpected reports an answer that contradicts the contract: a failure while
	// the job was certainly running, the host's doing after that.
	unexpected := func(format string, args ...any) {
		t.Helper()
		if since := time.Since(submitted); since >= engineHold {
			hostprobe.Skip(t, "the host stalled the test %v past Submit, longer than the job's %v hold, so the job may have ended before Shutdown met it: "+format,
				append([]any{since, engineHold}, args...)...)
			return
		}
		t.Fatalf(format, args...)
	}

	// 1. A budget the work outlives is reported, not rounded up to success.
	elapsed, _, err := shutdownWithin(t, 10*time.Millisecond, 10*time.Second)
	if !errors.Is(err, gusset.ErrShutdownIncomplete) {
		unexpected("Shutdown(10ms) with non-cooperative work in flight: want ErrShutdownIncomplete, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Shutdown(10ms) took %v", elapsed)
	}

	// 2. A negative budget is zero, not a wrapped uint32 of 49 days: it
	// answers at once, and with the work still in flight it says so.
	elapsed, _, err = shutdownWithin(t, -time.Hour, 10*time.Second)
	if !errors.Is(err, gusset.ErrShutdownIncomplete) {
		unexpected("Shutdown(-1h) with work in flight: want ErrShutdownIncomplete, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Shutdown(-1h) took %v: a negative budget must clamp to zero", elapsed)
	}

	// 3. A budget past MaxDrain is capped to MaxDrain, not wrapped. MaxDrain
	// plus 1 ms is 2^32 ms, which wraps to a 0 ms budget in a uint32 of
	// milliseconds, so a Shutdown that did not clamp would report
	// ErrShutdownIncomplete here for any job still running when it started.
	// Clamped, it waits the work out. A job that had already ended would let the
	// wrapped budget pass too, so the answer counts only if the call started
	// before the job's floor: judged from the instant the call itself began, not
	// from a guess at how long a stalled goroutine takes to reach it.
	//
	// That instant is the latest one this test can see, not the instant
	// Shutdown reads the in-flight count. A stall longer than the rest of the
	// hold between the two, inside the call, lets an unclamped Shutdown read a
	// finished job and pass here: a clamped call and a stalled wrapped one both
	// return after the job ends, and nothing after the return tells them apart.
	// boundary_inputs_internal_test.go owns the clamp deterministically, at
	// drainMillis; this step checks it end to end on every run that is not so
	// stalled.
	elapsed, called, err := shutdownWithin(t, gusset.MaxDrain+time.Millisecond, 30*time.Second)
	if since := called.Sub(submitted); since >= engineHold {
		hostprobe.Skip(t, "the host stalled the test until %v after Submit, longer than the job's %v hold, so the job may have ended before Shutdown(MaxDrain+1ms) read it and a wrapped budget could not be told from a clamped one (it answered %v)", since, engineHold, err)
		return
	}
	if err != nil {
		t.Fatalf("Shutdown(MaxDrain+1ms) must wait for the work, not wrap to a short budget; got %v after %v", err, elapsed)
	}

	// The job was never killed. Collect it so Close has nothing outstanding;
	// it finished, or was cut off by the latch, and either is an answer.
	if _, err := h.Wait(ctx, ticket); err != nil && !errors.Is(err, gusset.ErrShutdown) {
		t.Fatalf("Wait on the drained job: %v", err)
	}
}
