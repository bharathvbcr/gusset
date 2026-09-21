package pitfalls_test

// Deadline enforcement against a *non-cooperative* engine.
//
// Every deadline test that existed before this file drove diagnostic mode 5,
// which calls `ctx.check()` on every iteration. A cooperative engine cancels
// itself, Rust posts a `Cancelled` completion, and that completion is what wakes
// the waiting Go caller — so those tests measured the engine stopping, not
// Gusset enforcing the caller's deadline. They pass identically against a Gusset
// that ignores the deadline entirely.
//
// Diagnostic mode 9 is mode 5 with the `ctx.check()` removed and nothing else
// changed, which isolates the single variable. It stands in for what an adopter
// engine actually looks like: a tokenizer, a regex scan, a proof verifier or a
// SIMD transform runs a tight loop and never asks whether it should stop.
//
// The contract these tests pin down:
//
//   - `Call`/`Wait` return at the caller's deadline whatever the engine does.
//   - The pool permit is NOT released at that moment. The worker is still busy,
//     so releasing it would let a further submission in and breach I4. The
//     permit belongs to the work, not to the waiter.
//   - The abandoned result — bytes, Rust buffer, or panic — is still reclaimed
//     when it eventually arrives, and a panic still poisons the handle.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// nonCoop builds a mode 9 payload that sleeps for d without ever checking for
// cancellation. Resolution is 10 ms and the byte caps the sleep at 2.55 s.
func nonCoop(d time.Duration) []byte {
	units := d.Milliseconds() / 10
	if units < 1 {
		units = 1
	}
	if units > 255 {
		units = 255
	}
	return []byte{9, byte(units)}
}

// nonCoopThen builds a mode 9 delay prefix in front of another diagnostic
// payload, so the engine finishes with that behaviour d after it starts. It is
// what makes "abandoned" reachable: an instant result is collected by the caller
// before its deadline can fire.
func nonCoopThen(d time.Duration, then ...byte) []byte {
	return append(nonCoop(d), then...)
}

// A caller's deadline must release the caller even when the engine never
// cooperates.
//
// Pre-fix, `waitInternal`'s ctx.Done branch issued a best-effort cancel and then
// did an unconditional `<-ticketCh`. Cancellation is cooperative, so against an
// engine that never calls `check()` that receive parked until the job finished
// on its own: a 50 ms deadline on a 1 s job returned after 1 s. The context
// deadline was decorative for precisely the workloads Gusset exists to host.
func TestDeadline_NonCooperativeEngineStillReleasesTheCaller(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const engineRuns = 1500 * time.Millisecond
	const deadline = 80 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	start := time.Now()
	_, err = h.Call(ctx, nonCoop(engineRuns))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a deadline error from a job that outlives the context, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	// The bound is relative to the engine, not an absolute millisecond figure.
	//
	// The contract is "the caller returns before the engine does", and half the
	// engine's runtime tests exactly that with room to spare: the pre-fix code
	// returned at 1.503 s, which is past the whole engine, so it cannot pass by
	// luck. An absolute bound (500 ms) tested the same thing but failed on a
	// loaded machine — `go test ./...` runs package binaries in parallel — which
	// makes a real regression indistinguishable from a busy CI box.
	if elapsed > engineRuns/2 {
		t.Fatalf(
			"Call returned after %v for an %v deadline: the caller was parked until the "+
				"non-cooperative engine finished (%v), so the deadline was not enforced",
			elapsed, deadline, engineRuns,
		)
	}
	t.Logf("caller released after %v (deadline %v, engine %v)", elapsed, deadline, engineRuns)
}

// An explicit cancel must release the caller on the same terms as a deadline.
func TestDeadline_ExplicitCancelReleasesTheCallerFromANonCooperativeEngine(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const engineRuns = 1500 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err = h.Call(ctx, nonCoop(engineRuns))
	elapsed := time.Since(start)
	cancel()

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// Relative to the engine, for the reason given above.
	if elapsed > engineRuns/2 {
		t.Fatalf("Call returned after %v of a %v engine: explicit cancel did not release the caller",
			elapsed, engineRuns)
	}
}

// Releasing the caller must not release the pool permit (I4).
//
// This is the invariant the pre-fix blocking receive preserved by accident, and
// the one most at risk from any fix that simply returns early. The worker is
// still executing the abandoned job; if a fresh submission could take its place,
// in-flight work would exceed the pool size and the OS-thread bound Gusset sells
// would be gone.
//
// Pool size 1, so the single permit is the whole budget.
func TestDeadline_AbandonedJobKeepsHoldingItsPoolPermit(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// Occupy the only worker for ~1 s, abandoning the wait after 50 ms.
	const engineRuns = 1000 * time.Millisecond
	abandonCtx, abandonCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer abandonCancel()
	start := time.Now()
	if _, err := h.Call(abandonCtx, nonCoop(engineRuns)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the first Call to be released by its deadline, got %v", err)
	}
	released := time.Since(start)
	// Relative to the engine: the contract is "released before the engine ends".
	if released > engineRuns/2 {
		t.Fatalf("first Call was not released at its deadline (took %v of a %v engine)",
			released, engineRuns)
	}

	// The worker is still busy with the abandoned job for another ~950 ms. A
	// second submission must therefore find no permit and time out.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer probeCancel()
	ticket, err := h.Submit(probeCtx, []byte{0})
	if err == nil {
		_, _ = h.Wait(context.Background(), ticket)
		t.Fatal(
			"a second submission acquired the pool permit while the abandoned job was " +
				"still running on the only worker: in-flight work exceeded the pool size (I4)",
		)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the probe Submit to block on the held permit and time out, got %v", err)
	}

	// Once the abandoned job really finishes, the permit must come back — an
	// abandoned ticket must not strand its permit for the life of the handle.
	freeCtx, freeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer freeCancel()
	if _, err := h.Call(freeCtx, []byte{0}); err != nil {
		t.Fatalf("the permit was never returned after the abandoned job finished: %v", err)
	}
}

// A ticket abandoned by its owner must not be waitable again.
//
// The result is destined for the bin, and a second waiter parking for a
// completion that will be discarded is the same forever-park this whole file
// exists to remove.
func TestDeadline_AbandonedTicketIsNotWaitableAgain(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	submitCtx, submitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer submitCancel()
	ticket, err := h.Submit(submitCtx, nonCoop(800*time.Millisecond))
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer waitCancel()
	if _, err := h.Wait(waitCtx, ticket); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the first Wait to be released by its deadline, got %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.Wait(context.Background(), ticket)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, gusset.ErrUnknownTicket) {
			t.Fatalf("a re-Wait on an abandoned ticket must be refused, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a re-Wait on an abandoned ticket parked instead of being refused")
	}
}

// An abandoned job that returns a large result must still have its Rust buffer
// freed. Nothing is waiting to consume it, so if the completion path does not
// reclaim it the memory is lost for the life of the handle.
func TestDeadline_AbandonedLargeResultDoesNotLeakRustMemory(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// 64 KiB echoes: over the 4 KiB inline ceiling, so each completion carries a
	// Rust-owned buffer. The mode 9 delay prefix makes the echo land 200 ms
	// after a 20 ms deadline has already released the caller, so every round
	// really does abandon rather than racing to a collected result.
	const payload = 64 * 1024
	const rounds = 24
	const engineRuns = 200 * time.Millisecond

	fill := func(b *gusset.Buffer) {
		s := b.Bytes()
		prefix := nonCoopThen(engineRuns, 0)
		copy(s, prefix)
		for i := len(prefix); i < len(s); i++ {
			s[i] = byte(i)
		}
	}

	warm, warmCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer warmCancel()
	buf, err := h.NewBuffer(payload)
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}
	fill(buf)
	warmTicket, err := h.Submit(warm, buf)
	if err != nil {
		t.Fatalf("warm-up Submit failed: %v", err)
	}
	if _, err := h.Wait(warm, warmTicket); err != nil {
		t.Fatalf("warm-up Wait failed: %v", err)
	}
	if err := buf.Free(); err != nil {
		t.Fatalf("Free failed: %v", err)
	}

	baseline := gusset.Stats().LiveBytes
	abandonedRounds := 0

	for i := 0; i < rounds; i++ {
		b, err := h.NewBuffer(payload)
		if err != nil {
			t.Fatalf("NewBuffer %d failed: %v", i, err)
		}
		fill(b)

		submitCtx, submitCancel := context.WithTimeout(context.Background(), 30*time.Second)
		ticket, err := h.Submit(submitCtx, b)
		submitCancel()
		if err != nil {
			_ = b.Free()
			t.Fatalf("round %d: Submit failed: %v", i, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err = h.Wait(ctx, ticket)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			_ = b.Free()
			t.Fatalf("round %d: expected DeadlineExceeded, got %v", i, err)
		}
		abandonedRounds++
		if err := b.Free(); err != nil {
			t.Fatalf("round %d: Free failed: %v", i, err)
		}
	}
	if abandonedRounds != rounds {
		t.Fatalf("only %d of %d rounds abandoned; the test measured nothing", abandonedRounds, rounds)
	}

	// Let every abandoned completion land and be reclaimed.
	deadline := time.Now().Add(30 * time.Second)
	var live uint64
	for time.Now().Before(deadline) {
		live = gusset.Stats().LiveBytes
		if live <= baseline+payload {
			t.Logf("%d abandoned %d-byte results reclaimed; live %d vs baseline %d",
				rounds, payload, live, baseline)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf(
		"abandoned large results were not reclaimed: live bytes %d, baseline %d, "+
			"%d rounds of %d-byte results",
		live, baseline, rounds, payload,
	)
}

// A panic in an abandoned job must still poison the handle (I2).
//
// This pins the user-visible invariant — after the engine panics, the handle
// refuses work — and not any particular latch. Poison has three writers: the
// Rust worker sets `Handle.poisoned` inside its catch_unwind, which is the
// authority and makes the next `gusset_submit` return FFI_POISONED on its own;
// drainPipe and submit mirror it on the Go side to save a cgo round trip. The
// pre-detach code latched it a fourth time off the blocking receive in
// waitInternal, and removing that receive is exactly the kind of change that
// could have dropped the invariant on the floor, so it is asserted end to end
// rather than assumed from any one of the three.
func TestDeadline_PanicInAnAbandonedJobStillPoisonsTheHandle(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	submitCtx, submitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer submitCancel()

	// Panic 300 ms in, behind a 20 ms deadline: the caller is guaranteed to
	// abandon before the panic happens, so the only code that can latch the
	// poison is the completion path. A bare {1} panics instantly and is
	// collected by the caller, which tests the path that already worked.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, callErr := h.Call(ctx, nonCoopThen(300*time.Millisecond, 1))
	cancel()
	if !errors.Is(callErr, context.DeadlineExceeded) {
		t.Fatalf("expected the caller to be released by its deadline before the panic, got %v", callErr)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, err := h.Call(submitCtx, []byte{0})
		if errors.Is(err, gusset.ErrPoisoned) || errors.Is(err, gusset.ErrPanic) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("handle still accepted work after an abandoned job panicked (I2)")
}

// Close joins the pool, so it waits for whatever the engine is still doing.
//
// This is the operational counterpart to the detach: a caller's deadline now
// bounds the *caller*, but it does not bound shutdown. Close cancels
// cooperatively and then joins the worker threads, and joining is the right
// choice — detaching them would leak OS threads running against a handle whose
// Rust memory is about to be freed. The consequence is that a non-cooperative
// engine sets the floor on how long Close takes, and an adopter deploying one
// needs to know that rather than discover it during a rolling restart.
//
// Pinned as a measurement, not a limit: the assertion is that Close waits for
// the work (the join really happens) and then returns (it does not hang).
func TestShutdown_CloseWaitsForARunningNonCooperativeEngine(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const engineRuns = 700 * time.Millisecond

	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := h.Submit(sctx, nonCoop(engineRuns)); err != nil {
		scancel()
		t.Fatalf("Submit failed: %v", err)
	}
	scancel()

	// Let the worker actually pick the job up, so Close lands mid-execution
	// rather than before dispatch.
	time.Sleep(50 * time.Millisecond)

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- h.Close() }()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("Close failed: %v", err)
		}
		if elapsed < engineRuns/2 {
			t.Fatalf(
				"Close returned after %v without waiting for the running engine (%v): "+
					"the pool was not joined, so worker threads outlived the handle",
				elapsed, engineRuns,
			)
		}
		t.Logf("Close joined a non-cooperative engine in %v (engine runs %v): "+
			"shutdown latency is bounded by the engine, not by Close", elapsed, engineRuns)
	case <-time.After(20 * time.Second):
		t.Fatal("Close hung on a non-cooperative engine")
	}
}

// Close must reclaim permits and buffers held by abandoned tickets rather than
// deadlocking on them or leaving the semaphore short.
func TestDeadline_CloseReclaimsAbandonedTickets(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	var wg sync.WaitGroup
	var abandoned atomic.Int64
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if _, err := h.Call(ctx, nonCoop(600*time.Millisecond)); errors.Is(err, context.DeadlineExceeded) {
				abandoned.Add(1)
			}
		}()
	}
	wg.Wait()
	if abandoned.Load() == 0 {
		t.Skip("no call was abandoned; the engine outran the deadline on this machine")
	}

	done := make(chan error, 1)
	go func() { done <- h.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close failed with abandoned tickets outstanding: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Close deadlocked with abandoned tickets outstanding")
	}
}
