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
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
	"github.com/bharathvbcr/gusset/internal/hostprobe"
	"github.com/bharathvbcr/gusset/tests/internal/livebytes"
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

	// Occupy the only worker for ~1 s and abandon the ticket at once.
	//
	// Abandoned with Discard, not a 50 ms deadline: Go hands the deadline to
	// Rust, and on a loaded host it could pass while the job was still queued.
	// A job cancelled before it starts never runs the engine and rightly returns
	// its permit, which the probe below then took and reported as an I4 breach.
	// The release-at-the-deadline half is TestDeadline_NonCooperativeEngineStillReleasesTheCaller
	// above; this test is only about the permit. Discard does not cancel.
	const engineRuns = 1000 * time.Millisecond
	submitCtx, submitCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer submitCancel()
	start := time.Now()
	abandoned, err := h.Submit(submitCtx, nonCoop(engineRuns))
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	if err := h.Discard(abandoned); err != nil {
		t.Fatalf("Discard failed: %v", err)
	}

	// The worker is busy with the abandoned job for engineRuns from some point
	// after the submit. A second submission must therefore find no permit and
	// time out. Admission before engineRuns has elapsed since the submit is a
	// breach at any load; admission later proves nothing.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer probeCancel()
	ticket, err := h.Submit(probeCtx, []byte{0})
	if err == nil {
		admitted := time.Since(start)
		_ = h.Discard(ticket)
		if admitted < engineRuns {
			t.Fatalf(
				"a second submission acquired the pool permit %v after a %v job was submitted, "+
					"while the abandoned job was still running on the only worker: in-flight "+
					"work exceeded the pool size (I4)", admitted, engineRuns)
		}
		hostprobe.Skip(t, "the probe ran %v after the submit, past the job's end: it proves nothing on this host", admitted)
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

	baseline := livebytes.Quiesce(t)
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
	live, ok := livebytes.WaitUntil(func(live uint64) bool { return live <= baseline+payload })
	if !ok {
		t.Fatalf(
			"abandoned large results were not reclaimed: live bytes %d, baseline %d, "+
				"%d rounds of %d-byte results",
			live, baseline, rounds, payload,
		)
	}
	t.Logf("%d abandoned %d-byte results reclaimed; live %d vs baseline %d",
		rounds, payload, live, baseline)
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

	submitCtx, submitCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer submitCancel()

	// Panic 300 ms in, behind an abandoned ticket: the caller has given the
	// result up before the panic happens, so the only code that can latch the
	// poison is the completion path. A bare {1} panics instantly and is
	// collected by the caller, which tests the path that already worked.
	//
	// The ticket is abandoned with Discard, not a 20 ms deadline. Go hands the
	// deadline to Rust, and on a loaded host it could pass while the job was
	// still queued; a job cancelled before it starts returns Cancelled at
	// dequeue without running, so it never panicked and nothing was poisoned,
	// which is correct and not what this test is about. Discard does not cancel.
	ticket, err := h.Submit(submitCtx, nonCoopThen(300*time.Millisecond, 1))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := h.Discard(ticket); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	deadline := time.Now().Add(60 * time.Second)
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

	// 2.5 s, not the 700 ms it was: the bound below is engineRuns/2 measured
	// from Close, and the worker must already hold the job when Close lands.
	// Slack for a host that is slow to start the job or to schedule this
	// goroutine is engineRuns/2 minus the 50 ms nap, so it has to be wide for a
	// loaded host not to look like "Close returned without joining". A Close that
	// does not join returns in microseconds, far below either figure.
	const engineRuns = 2500 * time.Millisecond

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
		hostprobe.Skip(t, "no call was abandoned; the engine outran the deadline on this machine")
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

// Stress for the abandonment path introduced when waiters stopped parking on a
// completion they had already given up on.
//
// What is actually being protected here is not the semaphore for its own sake.
// The Rust pool is a fixed set of OS threads, so it bounds engine concurrency on
// its own; the Go permit bounds *queue depth*, and queue depth is what keeps
// `gusset_submit` from ever blocking. Rust's work channel holds `pool_size * 2`
// units, and a `sender.send()` that blocks does so inside a cgo call — which
// parks an M, and parking Ms under load is the thread explosion Gusset exists to
// prevent (I4/R11).
//
// So the interesting failure of "abandoned tickets return their permit too
// early" is not a lost result. It is: Go over-submits, Rust's channel fills,
// cgo blocks, and the process grows threads. That is what these tests measure.

// A sustained storm of abandoned non-cooperative jobs must not grow the thread
// pool and must not cost the handle any capacity.
//
// Every caller here walks away while its worker is still running, so the handle
// spends the whole test with abandoned tickets outstanding — the state that did
// not exist before waiters detached, and the state in which a permit accounting
// error is invisible until the process runs out of threads.
func TestChaos_NonCooperativeDeadlineStormKeepsThreadsBounded(t *testing.T) {
	// Waves of exactly pool size, not a flood.
	//
	// A flood is a weaker test than it looks: with hundreds of callers against
	// eight permits, almost every context expires while still queued on the Go
	// semaphore, so the caller returns from submit without ever reaching the
	// engine and nothing is abandoned at all. Each wave fills every permit and
	// every deadline lands mid-job, so each caller abandons exactly once.
	const poolSize = 8
	const waves = 50
	const callers = waves * poolSize
	const engineRuns = 120 * time.Millisecond
	const deadline = 40 * time.Millisecond

	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// Warm up so first-call thread creation is not counted as growth.
	warm, warmCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if _, err := h.Call(warm, []byte{0}); err != nil {
		warmCancel()
		t.Fatalf("warm-up Call failed: %v", err)
	}
	warmCancel()

	initialThreads := gusset.Threads()
	var peak atomic.Int64
	peak.Store(initialThreads)

	stopSampler := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-stopSampler:
				return
			default:
			}
			if n := gusset.Threads(); n > peak.Load() {
				peak.Store(n)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	var abandoned, finished atomic.Int64
	for w := 0; w < waves; w++ {
		var wg sync.WaitGroup
		for i := 0; i < poolSize; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), deadline)
				defer cancel()
				switch _, err := h.Call(ctx, nonCoop(engineRuns)); {
				case err == nil:
					finished.Add(1)
				case errors.Is(err, context.DeadlineExceeded):
					abandoned.Add(1)
				default:
					t.Errorf("unexpected error under the storm: %v", err)
				}
			}()
		}
		wg.Wait()
	}
	close(stopSampler)
	sampler.Wait()

	t.Logf("callers=%d abandoned=%d finished=%d threads: initial=%d peak=%d",
		callers, abandoned.Load(), finished.Load(), initialThreads, peak.Load())

	if abandoned.Load() < callers/2 {
		t.Fatalf("only %d of %d callers abandoned; the storm did not exercise the path",
			abandoned.Load(), callers)
	}

	bound := int64(poolSize + runtime.GOMAXPROCS(0) + 16)
	if peak.Load() > initialThreads+bound {
		t.Fatalf(
			"thread count grew from %d to %d under an abandonment storm (bound %d): "+
				"permits were returned before the work stopped, so Go over-submitted "+
				"and gusset_submit blocked in cgo (I4/R11)",
			initialThreads, peak.Load(), bound,
		)
	}

	// Every permit must eventually come back: the handle must still admit a full
	// pool of callers once the abandoned work has drained.
	var capWG sync.WaitGroup
	capErr := make(chan error, poolSize)
	for i := 0; i < poolSize; i++ {
		capWG.Add(1)
		go func() {
			defer capWG.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := h.Call(ctx, []byte{0}); err != nil {
				capErr <- err
			}
		}()
	}
	capWG.Wait()
	close(capErr)
	for err := range capErr {
		t.Fatalf("handle lost capacity after the abandonment storm: %v", err)
	}
}

// Abandonment interleaved with everything else: completions, cancels, buffers,
// zero-copy egress and poisoning, all against one handle.
//
// Run this under -race. It is looking for a torn interaction between the
// abandoned set, the pending map, the completed map and takeIDs — four maps
// under one mutex, written from both the caller goroutines and drainPipe.
func TestChaos_AbandonmentInterleavedWithEveryOtherOperation(t *testing.T) {
	const poolSize = 6
	const workers = 24
	const perWorker = 60

	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	var wg sync.WaitGroup
	var ops atomic.Int64
	deadline := time.Now().Add(60 * time.Second)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if time.Now().After(deadline) {
					return
				}
				ops.Add(1)
				switch (w + i) % 6 {
				case 0: // complete normally
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					_, err := h.Call(ctx, []byte{0, byte(i)})
					cancel()
					if err != nil && !isTolerated(err) {
						t.Errorf("w%d echo: %v", w, err)
					}

				case 1: // abandon a non-cooperative job
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
					_, err := h.Call(ctx, nonCoop(60*time.Millisecond))
					cancel()
					if err != nil && !errors.Is(err, context.DeadlineExceeded) && !isTolerated(err) {
						t.Errorf("w%d abandon: %v", w, err)
					}

				case 2: // abandon, then try to re-wait the spent ticket
					sctx, scancel := context.WithTimeout(context.Background(), 20*time.Second)
					ticket, err := h.Submit(sctx, nonCoop(60*time.Millisecond))
					scancel()
					if err != nil {
						if !isTolerated(err) {
							t.Errorf("w%d submit: %v", w, err)
						}
						continue
					}
					wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
					_, err = h.Wait(wctx, ticket)
					wcancel()
					if err == nil {
						continue // raced to completion, nothing was abandoned
					}
					if !errors.Is(err, context.DeadlineExceeded) && !isTolerated(err) {
						t.Errorf("w%d wait: %v", w, err)
						continue
					}
					rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
					_, err = h.Wait(rctx, ticket)
					rcancel()
					if err == nil {
						t.Errorf("w%d: a spent ticket returned a result", w)
					}

				case 3: // zero-copy round trip through CallBuffer
					const n = 8 * 1024
					in, err := h.NewBuffer(n)
					if err != nil {
						if !isTolerated(err) {
							t.Errorf("w%d NewBuffer: %v", w, err)
						}
						continue
					}
					s := in.Bytes()
					if s == nil {
						_ = in.Free()
						continue
					}
					s[0] = 0
					for k := 1; k < n; k++ {
						s[k] = byte(k + w)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					out, err := h.CallBuffer(ctx, in)
					cancel()
					_ = in.Free()
					if err != nil {
						if !isTolerated(err) {
							t.Errorf("w%d CallBuffer: %v", w, err)
						}
						continue
					}
					got := out.Bytes()
					if got != nil && len(got) != n {
						t.Errorf("w%d CallBuffer: echoed %d bytes, want %d", w, len(got), n)
					}
					runtime.KeepAlive(out)
					_ = out.Free()

				case 4: // abandon a buffer-backed job mid-flight
					const n = 8 * 1024
					in, err := h.NewBuffer(n)
					if err != nil {
						if !isTolerated(err) {
							t.Errorf("w%d NewBuffer: %v", w, err)
						}
						continue
					}
					if s := in.Bytes(); s != nil {
						copy(s, nonCoopThen(60*time.Millisecond, 0))
					}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
					out, err := h.CallBuffer(ctx, in)
					cancel()
					_ = in.Free()
					if err == nil {
						_ = out.Free()
						continue
					}
					if !errors.Is(err, context.DeadlineExceeded) && !isTolerated(err) {
						t.Errorf("w%d abandon buffer: %v", w, err)
					}

				case 5: // explicit cancel mid-flight
					ctx, cancel := context.WithCancel(context.Background())
					go func() {
						time.Sleep(5 * time.Millisecond)
						cancel()
					}()
					_, err := h.Call(ctx, nonCoop(60*time.Millisecond))
					cancel()
					if err != nil && !errors.Is(err, context.Canceled) && !isTolerated(err) {
						t.Errorf("w%d cancel: %v", w, err)
					}
				}
			}
		}(w)
	}
	wg.Wait()
	t.Logf("completed %d mixed operations across %d goroutines", ops.Load(), workers)

	// The handle must still be usable and must still have every permit.
	var capWG sync.WaitGroup
	for i := 0; i < poolSize; i++ {
		capWG.Add(1)
		go func() {
			defer capWG.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := h.Call(ctx, []byte{0}); err != nil {
				t.Errorf("handle lost capacity after the chaos run: %v", err)
			}
		}()
	}
	capWG.Wait()
}

// isTolerated reports errors that are legitimate outcomes of a chaos run rather
// than defects: a handle another goroutine poisoned, or one being closed.
func isTolerated(err error) bool {
	return errors.Is(err, gusset.ErrPoisoned) ||
		errors.Is(err, gusset.ErrPanic) ||
		errors.Is(err, context.Canceled)
}

// CallBuffer is the zero-copy round trip Call cannot express, so its contract is
// pinned directly: the payload survives, the output is Rust-owned rather than a
// Go copy, and the same argument checks Submit applies still apply.
func TestCallBuffer_ZeroCopyRoundTripAndArgumentChecks(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A payload well over the 4 KiB ceiling that Call refuses outright.
	const n = 256 * 1024
	in, err := h.NewBuffer(n)
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}
	s := in.Bytes()
	if s == nil {
		t.Fatal("NewBuffer returned a buffer with no bytes")
	}
	s[0] = 0 // diagnostic echo
	want := make([]byte, n)
	want[0] = 0
	for i := 1; i < n; i++ {
		s[i] = byte(i * 7)
		want[i] = byte(i * 7)
	}

	out, err := h.CallBuffer(ctx, in)
	if err != nil {
		t.Fatalf("CallBuffer failed: %v", err)
	}
	got := out.Bytes()
	if !bytes.Equal(got, want) {
		t.Fatalf("CallBuffer round trip corrupted the payload (%d bytes in, %d out)", n, len(got))
	}
	// The output is Rust-owned, so it carries a live buffer id rather than
	// having been copied onto the Go heap.
	if out.ID() == 0 {
		t.Fatal("CallBuffer returned a Go-copied buffer; the egress was not zero-copy")
	}
	runtime.KeepAlive(out)
	if err := out.Free(); err != nil {
		t.Fatalf("Free of the output buffer failed: %v", err)
	}
	if err := in.Free(); err != nil {
		t.Fatalf("Free of the input buffer failed: %v", err)
	}

	// Argument checks, matching Submit.
	if _, err := h.CallBuffer(ctx, nil); err == nil {
		t.Error("CallBuffer(nil) must be refused")
	}
	//lint:ignore SA1012 a nil context is the input under test
	if _, err := h.CallBuffer(nil, in); err == nil {
		t.Error("CallBuffer with a nil context must be refused")
	}
	if _, err := h.CallBuffer(ctx, in); err == nil {
		t.Error("CallBuffer with an already-freed buffer must be refused")
	}

	var nilHandle *gusset.Handle
	if _, err := nilHandle.CallBuffer(ctx, in); err == nil {
		t.Error("CallBuffer on a nil handle must be refused")
	}

	// A buffer from another handle must be refused rather than resolved against
	// this handle's id space, where the id names someone else's memory.
	other, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open of second handle failed: %v", err)
	}
	defer other.Close()
	foreign, err := other.NewBuffer(1024)
	if err != nil {
		t.Fatalf("NewBuffer on second handle failed: %v", err)
	}
	defer foreign.Free()
	if _, err := h.CallBuffer(ctx, foreign); err == nil {
		t.Error("CallBuffer must refuse a buffer belonging to a different handle")
	}
}
