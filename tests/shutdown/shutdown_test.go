package shutdown_test

// gusset.Shutdown drain semantics.
//
// Its own test binary, for the same reason tests/rust_shutdown.rs is: Shutdown
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
// everything about the test having run twice. Detecting it and skipping is the
// honest answer; asserting the transition again is not possible.
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

func TestShutdown_DrainsInFlightWorkThenRefusesSubmissions(t *testing.T) {
	if alreadyShutDown(t) {
		t.Skip("runtime already shut down in this process; the latch is one-way, so this can only be asserted once")
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
		if !errors.Is(err, context.Canceled) && !errors.Is(err, gusset.ErrGeneric) {
			t.Fatalf("ticket %d: drained work must report a cancellation, got %v", i, err)
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

// A budget that cannot be met must be reported, not rounded up to success.
//
// Cancellation is cooperative, so a non-cooperative engine cannot be drained at
// all. Reporting that is the whole point: an operator who is told the drain was
// clean will move on to killing the process.
func TestShutdown_ReportsWorkItCouldNotDrain(t *testing.T) {
	// Runs after the test above in file order, so the latch is already set and
	// no new work can be submitted. Assert the shape of the contract instead:
	// a zero budget must return promptly and must not claim success while work
	// is outstanding.
	start := time.Now()
	err := gusset.Shutdown(0)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("Shutdown(0) took %v; a zero budget must return promptly", elapsed)
	}
	// With nothing in flight this is a clean drain; the assertion is that it
	// answers at all and does not wedge.
	if err != nil && !errors.Is(err, gusset.ErrGeneric) {
		t.Fatalf("Shutdown(0) returned an unexpected error type: %v", err)
	}

	// A negative budget must be treated as zero rather than wrapping into a
	// very large uint32 of milliseconds (49 days) and hanging the caller.
	start = time.Now()
	_ = gusset.Shutdown(-time.Hour)
	if elapsed = time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Shutdown(-1h) took %v; a negative budget must clamp to zero", elapsed)
	}
}
