package e2e_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestTier5_Adversarial_AsyncCancellation_RingSaturationStorm stresses the interop boundary
// by driving heavy concurrent submissions into a small worker pool / ring buffer while 50%
// of operations are cancelled asynchronously at microsecond intervals.
func TestTier5_Adversarial_AsyncCancellation_RingSaturationStorm(t *testing.T) {
	const poolSize = 4
	h, err := gusset.Open(
		gusset.WithPoolSize(poolSize),
		gusset.WithDiagnosticEngine(),
		gusset.WithMaxUncollected(8),
	)
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	const numGoroutines = 24
	const iterationsPerGoroutine = 30
	var completedCount atomic.Int64
	var cancelledCount atomic.Int64
	var throttledCount atomic.Int64

	var wg sync.WaitGroup
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(gid*7919 + 1)))

			for i := 0; i < iterationsPerGoroutine; i++ {
				var ctx context.Context
				var cancel context.CancelFunc

				// 50% of jobs have an extremely tight timeout to race with completion delivery
				if rng.Float32() < 0.50 {
					timeout := time.Duration(rng.Intn(1500)+10) * time.Microsecond
					ctx, cancel = context.WithTimeout(context.Background(), timeout)
				} else {
					ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
				}

				// Alternate between mode 0 echo, mode 5 sleep-loop, and mode 9 delay prefix
				var payload []byte
				switch i % 3 {
				case 0:
					payload = []byte{0, byte(gid), byte(i)}
				case 1:
					// Mode 5: sleeps up to 10ms with ctx.check()
					payload = []byte{5, 1}
				case 2:
					// Mode 9: non-cooperative delay prefix (10ms) followed by echo
					payload = []byte{9, 1, 0, byte(gid), byte(i)}
				}

				ticket, err := h.Submit(ctx, payload)
				if err != nil {
					cancel()
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						cancelledCount.Add(1)
						continue
					}
					if errors.Is(err, gusset.ErrTooManyUncollected) {
						throttledCount.Add(1)
						time.Sleep(time.Millisecond)
						continue
					}
					t.Errorf("goroutine %d: unexpected Submit error: %v", gid, err)
					return
				}

				// If odd iteration, wait for result; if even, discard
				if (gid+i)%2 == 1 {
					res, err := h.Wait(ctx, ticket)
					cancel()
					if err != nil {
						if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
							cancelledCount.Add(1)
						} else {
							t.Errorf("goroutine %d: Wait error: %v", gid, err)
						}
					} else {
						completedCount.Add(1)
						if len(res) == 0 && payload[0] != 5 {
							t.Errorf("goroutine %d: unexpected empty result for payload %v", gid, payload)
						}
					}
				} else {
					err := h.Discard(ticket)
					cancel()
					if err != nil && !errors.Is(err, gusset.ErrUnknownTicket) {
						t.Errorf("goroutine %d: unexpected Discard error: %v", gid, err)
					}
				}
			}
		}(g)
	}
	wg.Wait()

	t.Logf("Ring saturation storm complete: completed=%d, cancelled=%d, throttled=%d",
		completedCount.Load(), cancelledCount.Load(), throttledCount.Load())

	// Settle: wait for any abandoned mode 9 jobs to exit and return permits
	time.Sleep(100 * time.Millisecond)

	// The storm must have exercised both outcomes it claims to mix; a storm in
	// which nothing completed or nothing was cancelled proves nothing about the
	// permits those paths return.
	if completedCount.Load() == 0 {
		t.Fatalf("no storm operation completed: the completion path was never exercised")
	}
	if cancelledCount.Load() == 0 {
		t.Fatalf("no storm operation was cancelled: the cancel and abandon paths were never exercised")
	}

	// Verify Invariant I4 (Bounded concurrency and permit conservation):
	// every one of the pool's permits must be back. Sequential calls cannot show
	// that, since one permit serves them all, so hold poolSize at once.
	assertAllPermitsHeldAtOnce(t, h, poolSize)
}

const (
	// permitHold is the most diagnostic mode 17 can hold a job (its argument is
	// one byte of milliseconds). A sleep is never shorter than asked, so a job
	// submitted at T is certainly still in the engine at T+permitHold.
	permitHold = 250 * time.Millisecond

	// permitProbeBy is how long after the first holder's Submit the poolSize+1th
	// submit may finish and still count: later than this the host stalled the
	// test and the holders may have ended. It chooses between running the check
	// again and judging it, never a verdict.
	permitProbeBy = 200 * time.Millisecond

	permitAttempts = 5
)

// readEngineGauge reads and resets diagnostic mode 18, the peak number of mode 17
// jobs the engine ran at once since the last read.
func readEngineGauge(t *testing.T, h *gusset.Handle) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := h.Call(ctx, []byte{18})
	requireNoError(t, err, "reading the engine gauge (mode 18)")
	if len(out) != 8 {
		t.Fatalf("engine gauge returned %d bytes, want 8", len(out))
	}
	return binary.LittleEndian.Uint64(out)
}

// assertAllPermitsHeldAtOnce proves h has all poolSize permits: poolSize
// non-cooperative jobs are admitted together and the engine's own gauge peaks
// at poolSize, while one more submit is neither admitted nor answered.
func assertAllPermitsHeldAtOnce(t *testing.T, h *gusset.Handle, poolSize int) {
	t.Helper()
	for attempt := 1; attempt <= permitAttempts; attempt++ {
		readEngineGauge(t, h) // reset the peak left by the storm or an earlier attempt

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		submitted := time.Now()
		tickets := make([]uint64, poolSize)
		for i := range tickets {
			tk, err := h.Submit(ctx, []byte{17, byte(permitHold / time.Millisecond)})
			if err != nil {
				cancel()
				t.Fatalf("I4: only %d of %d pool permits could be taken after the storm: Submit %d: %v", i, poolSize, i, err)
			}
			tickets[i] = tk
		}

		// poolSize jobs hold every permit. One more must wait for a permit.
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		probe, probeErr := h.Submit(probeCtx, []byte{0})
		probeCancel()
		sinceFirst := time.Since(submitted)
		if probeErr == nil {
			if _, err := h.Wait(ctx, probe); err != nil {
				cancel()
				t.Fatalf("collecting the extra job: %v", err)
			}
		}
		stalled := sinceFirst >= permitProbeBy

		for i, tk := range tickets {
			if _, err := h.Wait(ctx, tk); err != nil {
				cancel()
				t.Fatalf("holder %d: %v", i, err)
			}
		}
		cancel()

		if stalled {
			continue // the holders may have ended before the probe; run it again
		}
		if probeErr == nil {
			t.Fatalf("I4: a pool of %d admitted job %d while %d non-cooperative jobs were holding it", poolSize, poolSize+1, poolSize)
		}
		if !errors.Is(probeErr, context.DeadlineExceeded) {
			t.Fatalf("I4: the extra submit must wait for a permit, not fail with %v", probeErr)
		}
		if peak := readEngineGauge(t, h); peak != uint64(poolSize) {
			t.Fatalf("I4: the engine saw a peak of %d concurrent jobs, want exactly the pool size %d", peak, poolSize)
		}
		return
	}
	// Not a skip: a pool that is short of permits looks the same as a stalled
	// host (the holders cannot all be admitted before the first one ends), and a
	// check that could not run must not read as one that passed.
	t.Fatalf("I4: %d runs in a row could not admit %d holders and probe within %v of the first Submit: either the host stalled every run or the pool is short of permits",
		permitAttempts, poolSize, permitProbeBy)
}

// closeRaceOutcome is what one caller saw: the operation it ran and the error it
// got (nil for a completed operation whose result was checked).
type closeRaceOutcome struct {
	cid int
	op  string
	err error
}

// closeRaceNamedError reports whether err is one of the answers an operation may
// give when the handle is closed under it: the handle is closed, a ticket is no
// longer there, or the caller's own timeout ended. Anything else is a bug.
//
// A bare context.Canceled is not an answer: every caller holds a context that is
// never cancelled before it returns, so a cancel it sees was Close's, and Close's
// contract is ErrClosed (A3-2). The deadline stays because each caller's own
// 500 ms timeout can end under host load.
func closeRaceNamedError(err error) bool {
	return errors.Is(err, gusset.ErrClosed) ||
		errors.Is(err, gusset.ErrUnknownTicket) ||
		errors.Is(err, context.DeadlineExceeded)
}

// TestTier5_Adversarial_HandleCloseRaces_InFlightTickets tests concurrent handle Close()
// while goroutines are actively submitting, waiting, allocating, and running slow Rust jobs.
//
// Every caller records what it saw. Each outcome must be a valid echo or a named
// error, both Closes must return nil or ErrClosed, and the buffer bytes the
// handle held must be back at the baseline once everything has returned.
func TestTier5_Adversarial_HandleCloseRaces_InFlightTickets(t *testing.T) {
	var echoes, namedErrors int
	for iteration := 0; iteration < 8; iteration++ {
		baselineLive := gusset.Stats().LiveBytes
		h, err := gusset.Open(
			gusset.WithPoolSize(4),
			gusset.WithDiagnosticEngine(),
			gusset.WithBufferBudget(1024*1024),
		)
		requireNoError(t, err, "gusset.Open")

		const numCallers = 16
		var wg sync.WaitGroup
		startGate := make(chan struct{})
		var mu sync.Mutex
		var outcomes []closeRaceOutcome
		record := func(cid int, op string, err error) {
			mu.Lock()
			outcomes = append(outcomes, closeRaceOutcome{cid, op, err})
			mu.Unlock()
		}

		for c := 0; c < numCallers; c++ {
			wg.Add(1)
			go func(cid int) {
				defer wg.Done()
				<-startGate

				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				defer cancel()

				switch cid % 4 {
				case 0:
					// Slow non-cooperative job (mode 9 sleeps 20ms, then echoes)
					res, err := h.Call(ctx, []byte{9, 2, 0, byte(cid)})
					if err == nil && !bytes.Equal(res, []byte{0, byte(cid)}) {
						err = fmt.Errorf("mode 9 echo returned %v, want %v", res, []byte{0, byte(cid)})
						record(cid, "call-slow-bad-echo", err)
						return
					}
					record(cid, "call-slow", err)
				case 1:
					// Fast async submit and wait
					ticket, err := h.Submit(ctx, []byte{0, byte(cid)})
					if err != nil {
						record(cid, "submit", err)
						return
					}
					res, err := h.Wait(ctx, ticket)
					if err == nil && !bytes.Equal(res, []byte{0, byte(cid)}) {
						record(cid, "wait-bad-echo", fmt.Errorf("echo returned %v, want %v", res, []byte{0, byte(cid)}))
						return
					}
					record(cid, "wait", err)
				case 2:
					// Buffer allocation roundtrip
					buf, err := h.NewBuffer(8192)
					if err != nil {
						record(cid, "newbuffer", err)
						return
					}
					defer func() { record(cid, "buffer-free", buf.Free()) }()
					if data := buf.Bytes(); data != nil {
						data[0] = 0
					}
					ticket, err := h.Submit(ctx, buf)
					if err != nil {
						record(cid, "submit-buffer", err)
						return
					}
					outBuf, err := h.WaitBuffer(ctx, ticket)
					if err != nil {
						record(cid, "waitbuffer", err)
						return
					}
					if got := outBuf.Bytes(); got != nil && (len(got) != 8192 || got[0] != 0) {
						record(cid, "waitbuffer-bad-echo", fmt.Errorf("buffer echo has len %d first byte %d, want 8192 and 0", len(got), got[0]))
					} else {
						record(cid, "waitbuffer", nil)
					}
					record(cid, "outbuf-free", outBuf.Free())
				case 3:
					// Async submit and immediate discard
					ticket, err := h.Submit(ctx, []byte{0, byte(cid)})
					if err != nil {
						record(cid, "submit", err)
						return
					}
					record(cid, "discard", h.Discard(ticket))
				}
			}(c)
		}

		// Let callers start running into Rust
		close(startGate)
		time.Sleep(time.Duration(5+iteration*2) * time.Millisecond)

		// Race Close() from multiple goroutines concurrently
		closeErrs := make([]error, 2)
		var closeWg sync.WaitGroup
		closeWg.Add(2)
		for i := range closeErrs {
			go func() {
				defer closeWg.Done()
				closeErrs[i] = h.Close()
			}()
		}
		closeWg.Wait()

		// Wait for all callers to return
		wg.Wait()

		for i, err := range closeErrs {
			if err != nil && !errors.Is(err, gusset.ErrClosed) {
				t.Errorf("iteration %d: concurrent Close #%d returned %v, want nil or ErrClosed", iteration, i, err)
			}
		}
		if len(outcomes) == 0 {
			t.Fatalf("iteration %d: no caller recorded an outcome", iteration)
		}
		for _, o := range outcomes {
			switch {
			case o.err == nil:
				echoes++
			case closeRaceNamedError(o.err):
				namedErrors++
			default:
				t.Errorf("iteration %d: caller %d (%s) got %v, want a valid result or ErrClosed, ErrUnknownTicket or a context error",
					iteration, o.cid, o.op, o.err)
			}
		}

		// Everything has returned and the handle is closed: the buffers it held
		// must be back, however Close interleaved with the callers.
		settleBy := time.Now().Add(5 * time.Second)
		for gusset.Stats().LiveBytes != baselineLive && time.Now().Before(settleBy) {
			time.Sleep(5 * time.Millisecond)
		}
		if live := gusset.Stats().LiveBytes; live != baselineLive {
			t.Errorf("iteration %d: LiveBytes settled at %d, want the baseline %d (delta %d)",
				iteration, live, baselineLive, int64(live)-int64(baselineLive))
		}

		// Assert handle is deterministically closed and post-close calls are rejected
		ctxClosed := context.Background()
		_, errCall := h.Call(ctxClosed, []byte{0})
		if !errors.Is(errCall, gusset.ErrClosed) {
			t.Fatalf("iteration %d: expected ErrClosed on post-close Call, got: %v", iteration, errCall)
		}

		_, errSub := h.Submit(ctxClosed, []byte{0})
		if !errors.Is(errSub, gusset.ErrClosed) {
			t.Fatalf("iteration %d: expected ErrClosed on post-close Submit, got: %v", iteration, errSub)
		}

		_, errBuf := h.NewBuffer(1024)
		if !errors.Is(errBuf, gusset.ErrClosed) {
			t.Fatalf("iteration %d: expected ErrClosed on post-close NewBuffer, got: %v", iteration, errBuf)
		}
	}
	t.Logf("close races: %d valid results, %d named errors across 8 iterations", echoes, namedErrors)
	if echoes == 0 {
		t.Fatalf("no operation completed before Close in any iteration; the race was never run against live work")
	}
}

// TestTier5_Adversarial_BufferBudget_ExtremeBoundaryAndRaces validates exact boundary accounting,
// multi-threaded allocation races under budget pressure, and use-after-free poisoning protection.
func TestTier5_Adversarial_BufferBudget_ExtremeBoundaryAndRaces(t *testing.T) {
	const exactBudget = 64 * 1024 // 64 KiB
	h, err := gusset.Open(
		gusset.WithPoolSize(2),
		gusset.WithDiagnosticEngine(),
		gusset.WithBufferBudget(exactBudget),
	)
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	// 1. Exact boundary: 64 KiB succeeds
	bufExact, err := h.NewBuffer(exactBudget)
	requireNoError(t, err, "NewBuffer(64 KiB)")
	if !is64ByteAligned(bufExact.Bytes()) {
		t.Fatal("bufExact not 64-byte aligned")
	}

	// 2. Exact overflow by 1 byte must fail with ErrBufferBudget
	_, errOverflow := h.NewBuffer(1)
	if !errors.Is(errOverflow, gusset.ErrBufferBudget) {
		t.Fatalf("expected ErrBufferBudget on 1-byte overflow, got: %v", errOverflow)
	}

	// 3. Free bufExact and verify view and ID withdrawal
	sliceRef := bufExact.Bytes()
	if len(sliceRef) != exactBudget {
		t.Fatalf("expected slice length %d, got %d", exactBudget, len(sliceRef))
	}
	err = bufExact.Free()
	requireNoError(t, err, "bufExact.Free()")

	// After Free, Bytes() must return nil and ID() must return 0
	if bufExact.Bytes() != nil {
		t.Fatal("expected bufExact.Bytes() to be nil after Free()")
	}
	if bufExact.ID() != 0 {
		t.Fatalf("expected bufExact.ID() == 0 after Free(), got %d", bufExact.ID())
	}

	// 4. Double Free is an idempotent no-op
	err = bufExact.Free()
	requireNoError(t, err, "double Free()")

	// 5. Multi-threaded race allocating chunks of 16 KiB under 64 KiB budget
	const numRacers = 16
	const roundsPerRacer = 20
	var successfulAllocations atomic.Int64
	var budgetRejections atomic.Int64

	var wg sync.WaitGroup
	for r := 0; r < numRacers; r++ {
		wg.Add(1)
		go func(racerID int) {
			defer wg.Done()
			for round := 0; round < roundsPerRacer; round++ {
				buf, err := h.NewBuffer(16 * 1024)
				if err != nil {
					if errors.Is(err, gusset.ErrBufferBudget) {
						budgetRejections.Add(1)
						time.Sleep(500 * time.Microsecond)
						continue
					}
					t.Errorf("racer %d: unexpected NewBuffer error: %v", racerID, err)
					return
				}
				successfulAllocations.Add(1)

				data := buf.Bytes()
				if data != nil && len(data) == 16*1024 {
					data[0] = byte(racerID)
					data[len(data)-1] = byte(round)
				}

				// Free immediately or shortly
				_ = buf.Free()
			}
		}(r)
	}
	wg.Wait()

	t.Logf("Buffer budget race: successful=%d, rejected=%d",
		successfulAllocations.Load(), budgetRejections.Load())

	// 6. Verify zero budget drift: allocating full 64 KiB must now succeed
	bufRecovery, err := h.NewBuffer(exactBudget)
	requireNoError(t, err, "NewBuffer after race recovery")
	_ = bufRecovery.Free()
}

// TestTier5_Adversarial_PanicFirewall_DeepStressAndDropBombs verifies that all panic modes
// (including non-string, display panics, megabyte panics, and recursive drop bombs) are strictly
// isolated, correctly poison the invoking handle, and never contaminate sibling handles.
func TestTier5_Adversarial_PanicFirewall_DeepStressAndDropBombs(t *testing.T) {
	// Handle A: The sacrificial handle receiving panics
	hA, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "Open Handle A")
	defer hA.Close()

	// Handle B: The pristine sibling handle serving continuous heavy production traffic
	hB, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "Open Handle B")
	defer hB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start continuous background traffic on Handle B
	var siblingStop atomic.Bool
	var siblingOps atomic.Int64
	var siblingWg sync.WaitGroup

	for s := 0; s < 4; s++ {
		siblingWg.Add(1)
		go func(sid int) {
			defer siblingWg.Done()
			for !siblingStop.Load() {
				// Alternating between vector sum squares and echo
				data := make([]byte, 256)
				for i := range data {
					data[i] = byte(i ^ sid)
				}
				payload := append([]byte{10}, data...)
				res, err := hB.Call(ctx, payload)
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return
					}
					t.Errorf("sibling lane %d failed: %v", sid, err)
					return
				}
				got := binary.LittleEndian.Uint64(res)
				want := computeVectorSumSquaresOracle(data)
				if got != want {
					t.Errorf("sibling lane %d math mismatch: got %d, want %d", sid, got, want)
					return
				}
				siblingOps.Add(1)
			}
		}(s)
	}

	// Attack Handle A with diverse adversarial panic payloads
	panicScenarios := []struct {
		name    string
		payload []byte
	}{
		{"Mode 1 Plain Panic", []byte{1}},
		{"Mode 2 NUL Byte Panic", []byte{2}},
		{"Mode 3 Non-String Panic (i32)", []byte{3}},
		{"Mode 4 Display Panic", []byte{4}},
		{"Mode 12 Megabyte String Panic", []byte{12}},
		{"Mode 13 Primitive Panic (usize)", []byte{13, 3}},
		{"Mode 15 Nested Drop Bomb Panic (depth 4)", []byte{15, 4}},
		{"Mode 9 Delayed Drop Bomb (10ms + Mode 15)", []byte{9, 1, 15, 2}},
	}

	for _, sc := range panicScenarios {
		// New handle per scenario to isolate poisoning
		hChaos, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
		requireNoError(t, err, "Open chaos handle for "+sc.name)

		// 1. Healthy pre-check
		resEcho, err := hChaos.Call(ctx, []byte{0, 0x55})
		requireNoError(t, err, sc.name+" pre-check")
		assertBytesEqual(t, resEcho, []byte{0, 0x55}, sc.name+" pre-check echo")

		// 2. Trigger panic
		_, errPanic := hChaos.Call(ctx, sc.payload)
		if !errors.Is(errPanic, gusset.ErrPanic) {
			t.Errorf("%s: expected ErrPanic, got: %v", sc.name, errPanic)
		}

		// 3. Verify handle is poisoned
		_, errPoison := hChaos.Call(ctx, []byte{0, 0x66})
		if !errors.Is(errPoison, gusset.ErrPoisoned) {
			t.Errorf("%s: expected ErrPoisoned after panic, got: %v", sc.name, errPoison)
		}

		_ = hChaos.Close()
	}

	// Stop sibling workers and verify Handle B is completely unaffected
	siblingStop.Store(true)
	siblingWg.Wait()

	t.Logf("Panic containment verified: sibling completed %d heavy operations without error",
		siblingOps.Load())
	if siblingOps.Load() == 0 {
		t.Fatal("sibling lane did not execute any operations")
	}
}

// TestTier5_Adversarial_MaxUncollected_BackpressureDeadlockDefense tests the uncollected
// results backpressure mechanism under saturation, cancellation, and discard storms.
func TestTier5_Adversarial_MaxUncollected_BackpressureDeadlockDefense(t *testing.T) {
	const maxUncollected = 2
	h, err := gusset.Open(
		gusset.WithPoolSize(2),
		gusset.WithDiagnosticEngine(),
		gusset.WithMaxUncollected(maxUncollected),
	)
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx := context.Background()

	// 1. Submit 2 uncollected tickets (fills maxUncollected capacity)
	ticket1, err := h.Submit(ctx, []byte{0, 1})
	requireNoError(t, err, "submit 1")
	ticket2, err := h.Submit(ctx, []byte{0, 2})
	requireNoError(t, err, "submit 2")

	// Wait for completions to land in handle completed map
	time.Sleep(20 * time.Millisecond)

	// 2. Third submission with tight deadline must be throttled with ErrTooManyUncollected
	ctxTimeout, cancelTimeout := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, errThrottled := h.Submit(ctxTimeout, []byte{0, 3})
	cancelTimeout()

	if !errors.Is(errThrottled, gusset.ErrTooManyUncollected) {
		t.Fatalf("expected ErrTooManyUncollected, got: %v", errThrottled)
	}

	// 3. Discard ticket1 to free up room
	err = h.Discard(ticket1)
	requireNoError(t, err, "h.Discard(ticket1)")

	// 4. Now third submission should succeed
	ticket3, err := h.Submit(ctx, []byte{0, 3})
	requireNoError(t, err, "submit 3 after discarding ticket1")

	// Clean up ticket2 and ticket3
	res2, err := h.Wait(ctx, ticket2)
	requireNoError(t, err, "h.Wait(ticket2)")
	assertBytesEqual(t, res2, []byte{0, 2}, "ticket2 echo")

	res3, err := h.Wait(ctx, ticket3)
	requireNoError(t, err, "h.Wait(ticket3)")
	assertBytesEqual(t, res3, []byte{0, 3}, "ticket3 echo")
}

// TestTier5_Adversarial_BufferSlice_UseAfterFreeDefenses validates that slices extracted
// via Bytes() cannot cause use-after-free or read corrupted memory once Free() or Close() occurs.
func TestTier5_Adversarial_BufferSlice_UseAfterFreeDefenses(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")

	ctx := context.Background()

	// 1. Rust-backed buffer lifecycle
	bufRust, err := h.NewBuffer(1024)
	requireNoError(t, err, "NewBuffer")
	if bufRust.ID() == 0 {
		t.Fatal("expected non-zero ID for Rust-allocated buffer")
	}

	sliceRust := bufRust.Bytes()
	if sliceRust == nil || len(sliceRust) != 1024 {
		t.Fatalf("invalid slice from bufRust: len=%d", len(sliceRust))
	}
	sliceRust[0] = 0xAA

	// Free and verify Bytes() immediately reports nil
	_ = bufRust.Free()
	if bufRust.Bytes() != nil {
		t.Fatal("expected bufRust.Bytes() == nil after Free()")
	}

	// 2. Go-heap result buffer (inline echo < 4 KiB)
	outBufGo, err := h.CallBuffer(ctx, func() *gusset.Buffer {
		b, _ := h.NewBuffer(64)
		copy(b.Bytes(), []byte{0, 1, 2, 3})
		return b
	}())
	requireNoError(t, err, "CallBuffer inline")
	defer outBufGo.Free()

	if outBufGo.ID() != 0 {
		t.Logf("inline CallBuffer result ID: %d", outBufGo.ID())
	}
	sliceGo := outBufGo.Bytes()
	if !is64ByteAligned(sliceGo) {
		t.Fatal("inline result buffer slice is not 64-byte aligned")
	}

	// 3. Handle Close frees all associated buffers
	bufRemaining, err := h.NewBuffer(2048)
	requireNoError(t, err, "NewBuffer before close")

	err = h.Close()
	requireNoError(t, err, "h.Close()")

	// After handle Close, Bytes() on un-freed buffer must safely return nil
	if bufRemaining.Bytes() != nil {
		t.Fatal("expected bufRemaining.Bytes() == nil after Handle.Close()")
	}

	// Subsequent Free() on buffer after handle close must be safe no-op
	err = bufRemaining.Free()
	requireNoError(t, err, "bufRemaining.Free() after handle close")
}

// TestTier5_Adversarial_ZeroAndOneByteEdgeCases tests extreme payload sizes including 0-byte, 1-byte,
// maximum inline payload (exact 4096 bytes), and 4097-byte boundary refusal.
func TestTier5_Adversarial_ZeroAndOneByteEdgeCases(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx := context.Background()

	// 1. Zero-byte payload
	res0, err := h.Call(ctx, []byte{})
	requireNoError(t, err, "Call 0-byte payload")
	if len(res0) != 0 {
		t.Fatalf("expected 0 bytes return, got %d", len(res0))
	}

	// 2. One-byte payload (mode 0 echo)
	res1, err := h.Call(ctx, []byte{0})
	requireNoError(t, err, "Call 1-byte payload")
	assertBytesEqual(t, res1, []byte{0}, "1-byte echo")

	// 3. Exact 4096-byte inline payload (the MAX_INLINE_INPUT boundary)
	exactInline := make([]byte, 4096)
	exactInline[0] = 0 // mode 0
	exactInline[4095] = 0xBE
	resExact, err := h.Call(ctx, exactInline)
	requireNoError(t, err, "Call exact 4096-byte payload")
	assertBytesEqual(t, resExact, exactInline, "4096-byte echo")

	// 4. Exact 4097-byte payload must be rejected as ErrInputTooLarge
	exceedInline := make([]byte, 4097)
	_, errExceed := h.Call(ctx, exceedInline)
	if !errors.Is(errExceed, gusset.ErrInputTooLarge) {
		t.Fatalf("expected ErrInputTooLarge on 4097-byte payload, got: %v", errExceed)
	}

	// 5. Zero-byte buffer wait
	bufZero, err := h.NewBuffer(64)
	requireNoError(t, err, "NewBuffer(64)")
	copy(bufZero.Bytes(), []byte{0})
	resBufZero, err := h.CallBuffer(ctx, bufZero)
	requireNoError(t, err, "CallBuffer")
	_ = bufZero.Free()
	_ = resBufZero.Free()
}

// TestTier5_Adversarial_ConcurrentGarbageCollectionStress allocates hundreds of buffers
// concurrently while intentionally dropping references without Free() and triggering runtime.GC()
// to stress the AddCleanup backstop and verify zero leaks or deadlocks.
func TestTier5_Adversarial_ConcurrentGarbageCollectionStress(t *testing.T) {
	const poolSize = 4
	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const numGoroutines = 12
	const buffersPerGoroutine = 50

	var wg sync.WaitGroup
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < buffersPerGoroutine; i++ {
				buf, err := h.NewBuffer(1024)
				if err != nil {
					t.Errorf("goroutine %d: NewBuffer error: %v", gid, err)
					return
				}
				data := buf.Bytes()
				if data != nil {
					data[0] = 0
					data[len(data)-1] = byte(gid + i)
				}

				// If even, free explicitly; if odd, drop reference and let GC collect
				if (gid+i)%2 == 0 {
					_ = buf.Free()
				} else {
					// Drop reference
					buf = nil
				}

				// Occasional roundtrip
				if i%10 == 0 {
					res, err := h.Call(ctx, []byte{0, byte(gid), byte(i)})
					if err == nil {
						assertBytesEqual(t, res, []byte{0, byte(gid), byte(i)}, "gc stress echo")
					}
				}
			}
		}(g)
	}

	// Concurrently trigger garbage collection
	gcDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-gcDone:
				return
			default:
				time.Sleep(10 * time.Millisecond)
				runtime.GC()
			}
		}
	}()

	wg.Wait()
	close(gcDone)

	// Final GC and settle
	runtime.GC()
	time.Sleep(50 * time.Millisecond)

	// Verify handle remains fully operational
	resFinal, err := h.Call(ctx, []byte{0, 0xEE})
	requireNoError(t, err, "final Call after GC stress")
	assertBytesEqual(t, resFinal, []byte{0, 0xEE}, "final GC stress echo")
}

// TestTier5_Adversarial_MassiveConcurrentWorkersAndOpcodeRouting stresses 32 concurrent goroutines
// sending a mix of valid vector compute opcodes and invalid opcodes to verify opcode isolation and non-poisoning.
func TestTier5_Adversarial_MassiveConcurrentWorkersAndOpcodeRouting(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(8), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const numWorkers = 24
	const opsPerWorker = 30
	var validOps atomic.Int64
	var invalidOps atomic.Int64

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				if (wid+i)%4 == 0 {
					// Invalid opcode: must return error without poisoning handle
					ctxInvalid := gusset.ContextWithOpcode(ctx, 99990+uint32(wid%10))
					_, err := h.Call(ctxInvalid, []byte{0, 1})
					if err == nil {
						t.Errorf("worker %d: expected error for invalid opcode", wid)
						return
					}
					invalidOps.Add(1)
				} else {
					// Valid diagnostic call: mode 10 vector sum square
					data := make([]byte, 128)
					for j := range data {
						data[j] = byte(j ^ wid)
					}
					payload := append([]byte{10}, data...)
					res, err := h.Call(ctx, payload)
					if err != nil {
						t.Errorf("worker %d: valid call failed: %v", wid, err)
						return
					}
					got := binary.LittleEndian.Uint64(res)
					want := computeVectorSumSquaresOracle(data)
					if got != want {
						t.Errorf("worker %d: math mismatch: got %d, want %d", wid, got, want)
						return
					}
					validOps.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()

	t.Logf("Opcode routing stress complete: validOps=%d, invalidOps=%d",
		validOps.Load(), invalidOps.Load())

	// Settle check
	res, err := h.Call(ctx, []byte{0, 0x42})
	requireNoError(t, err, "settle call")
	assertBytesEqual(t, res, []byte{0, 0x42}, "settle echo")
}
