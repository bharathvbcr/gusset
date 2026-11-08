package shutdown_test

// gusset.Shutdown drain semantics.
//
// Its own test binary, for the same reason tests/rust/rust_shutdown.rs is: Shutdown
// latches a process-global flag that refuses every later submission, so any
// test sharing this binary would see its work rejected by whichever test ran
// first. Every assertion about "before shutdown" therefore has to happen in
// this file, ahead of the single Shutdown call.
//
// What this covers that the Rust-side test cannot: that the budgeted drain is
// reachable from Go at all. Rust has implemented gusset_shutdown since the
// README started advertising "graceful shutdown and worker drain", internal/ffi
// has bound it just as long, and no exported Go function called it — so the one
// bounded shutdown Gusset offers was unreachable by the audience it is for.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// alreadyShutDown reports whether this process has already latched.
//
// Shutdown is deliberately one-way and process-global, so this test can only
// assert the transition once per process. Under `go test -count=2` the second
// run would open a handle against a runtime that is already refusing work and
// fail on its own warm-up call — a failure that says nothing about Gusset and
// everything about the test having run twice. Skipping would turn that run into
// a green test that examined nothing, so the transition is re-asserted in a
// fresh process instead (runInFreshProcess).
func alreadyShutDown(t *testing.T) bool {
	t.Helper()
	probe, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		return true
	}
	defer probe.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = probe.Call(ctx, []byte{0})
	return err != nil && strings.Contains(err.Error(), "shutting down")
}

// freshProcessEnv marks the child started by runInFreshProcess, so a child that
// somehow finds the latch already set fails instead of re-executing forever.
const freshProcessEnv = "GUSSET_SHUTDOWN_FRESH_PROCESS"

// runInFreshProcess re-executes this test binary for the one test named, in a
// process whose latch is still open, and requires that the test ran and passed
// there. Not a skip: the child's -v output must report PASS for the test and no
// SKIP, or this fails.
func runInFreshProcess(t *testing.T, name string) {
	t.Helper()
	if os.Getenv(freshProcessEnv) == "1" {
		t.Fatalf("%s found the shutdown latch already set in a process started to run it alone", name)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary to re-execute %s: %v", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^"+name+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), freshProcessEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed in a fresh process: %v\n%s", name, err, out)
	}
	if !strings.Contains(string(out), "--- PASS: "+name) || strings.Contains(string(out), "--- SKIP") {
		t.Fatalf("%s did not run to a pass in a fresh process:\n%s", name, out)
	}
}

func TestShutdown_DrainsInFlightWorkThenRefusesSubmissions(t *testing.T) {
	if alreadyShutDown(t) {
		runInFreshProcess(t, "TestShutdown_DrainsInFlightWorkThenRefusesSubmissions")
		return
	}

	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// Prove the handle works before the latch, so a later refusal is evidence
	// about Shutdown rather than about the handle.
	warm, warmCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if _, err := h.Call(warm, []byte{0, 7}); err != nil {
		warmCancel()
		t.Fatalf("pre-shutdown Call failed: %v", err)
	}
	warmCancel()

	// Mode 5 is the cooperative sleep loop: it checks between units, so the
	// cancellation Shutdown issues can actually land and the drain can be clean.
	// 8 units of 10 ms each.
	tickets := make([]uint64, 0, 4)
	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ticket, err := h.Submit(ctx, []byte{5, 8})
		cancel()
		if err != nil {
			t.Fatalf("Submit %d failed: %v", i, err)
		}
		tickets = append(tickets, ticket)
	}

	start := time.Now()
	if err := gusset.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("Shutdown did not drain cooperative work inside its budget: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("Shutdown overran its 5s budget, taking %v", elapsed)
	}
	t.Logf("drained 4 in-flight cooperative jobs in %v", elapsed)

	// Collect the drained tickets before testing the latch.
	//
	// Their permits are the handle's whole budget. Left uncollected, the next
	// Call blocks on the semaphore until its context expires and returns a
	// non-nil error that looks exactly like a refusal — the test would pass
	// while proving nothing about the latch at all.
	for i, ticket := range tickets {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := h.Wait(ctx, ticket)
		cancel()
		if err == nil {
			continue // finished before the cancel landed
		}
		// The caller's context is live: this was Shutdown, not the caller.
		// It used to read as context.Canceled.
		if !errors.Is(err, gusset.ErrShutdown) || errors.Is(err, context.Canceled) {
			t.Fatalf("ticket %d: drained work must report ErrShutdown, not context.Canceled; got %v", i, err)
		}
	}

	// The latch must now refuse work on this handle, and refuse it *promptly*:
	// a refusal is a decision, not a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	refuseStart := time.Now()
	_, err = h.Call(ctx, []byte{0})
	refuseElapsed := time.Since(refuseStart)
	if err == nil {
		t.Fatal("a submission succeeded after Shutdown; the refusal latch did not take")
	}
	if refuseElapsed > time.Second {
		t.Fatalf(
			"the post-shutdown Call took %v to fail: that is a blocked permit expiring, "+
				"not the refusal latch answering (%v)",
			refuseElapsed, err,
		)
	}

	// And on a handle opened afterwards: the latch is process-wide, not
	// per-handle, and an adopter who reopens expecting a fresh start must be
	// refused rather than quietly served.
	fresh, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		// Refusing the open outright is also a correct answer.
		return
	}
	defer fresh.Close()
	if _, err := fresh.Call(ctx, []byte{0}); err == nil {
		t.Fatal("a handle opened after Shutdown accepted work; the latch is not process-wide")
	}
}

// Shutdown with nothing in flight answers promptly and cleanly for every
// budget, including zero, negative and past MaxDrain.
//
// This is not the test of an undrained budget: its work is cooperative and
// already drained, so there is nothing to report. ErrShutdownIncomplete with
// non-cooperative work in flight is asserted in tests/shutdown_incomplete.
func TestShutdown_WithNothingInFlightReturnsCleanlyForAnyBudget(t *testing.T) {
	// Runs after the test above in file order, so the latch is already set and
	// all its work was drained and collected.
	start := time.Now()
	err := gusset.Shutdown(0)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("Shutdown(0) took %v; a zero budget must return promptly", elapsed)
	}
	// Nothing is outstanding, so a zero budget is met. Reporting
	// ErrShutdownIncomplete here would tell an operator to kill a process that
	// has nothing left to kill.
	if err != nil {
		t.Fatalf("Shutdown(0) with nothing in flight must be a clean drain, got %v", err)
	}

	// A negative budget must be treated as zero rather than wrapping into a
	// very large uint32 of milliseconds (49 days) and hanging the caller.
	start = time.Now()
	_ = gusset.Shutdown(-time.Hour)
	if elapsed = time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Shutdown(-1h) took %v; a negative budget must clamp to zero", elapsed)
	}

	// A budget past MaxDrain is capped to it, and the cap is a drain, not a
	// failure. Nothing is in flight, so the call returns at once; what it must
	// not do is error, panic or wedge on the out-of-range value. (The clamp
	// itself, which a wrapped value would pass here, is pinned by
	// TestDrainMillis_ClampsInsteadOfWrapping.)
	for _, d := range []time.Duration{gusset.MaxDrain, gusset.MaxDrain + time.Second, time.Duration(1<<63 - 1)} {
		start = time.Now()
		if err := gusset.Shutdown(d); err != nil {
			t.Fatalf("Shutdown(%v) with nothing in flight: %v", d, err)
		}
		if elapsed = time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("Shutdown(%v) took %v with nothing in flight", d, elapsed)
		}
	}
}
