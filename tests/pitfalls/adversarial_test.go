package pitfalls_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// mustReturnWithin fails the test if fn has not returned by d.
//
// Every hang-shaped defect in this file would otherwise block the whole test
// binary until the -timeout kills it, losing which call hung and why.
func mustReturnWithin(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v (hang)", what, d)
	}
}

// openFDCount reports how many descriptors this process holds.
func openFDCount(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("lsof", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Skipf("cannot count open fds via lsof: %v", err)
	}
	return len(strings.Split(strings.TrimSpace(string(out)), "\n")) - 1
}

// TestAdversarial_WaitOnUnknownTicketRespectsContext.
//
// Wait registered the ticket in the pending map and then, on ctx.Done, waited
// unconditionally for a completion that was never coming. A caller who passed a
// ticket that was never submitted — a typo, a stale id, a ticket from a different
// handle — parked a goroutine forever, and its context deadline did nothing.
func TestAdversarial_WaitOnUnknownTicketRespectsContext(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	mustReturnWithin(t, 5*time.Second, "Wait on an unknown ticket", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		if _, err := h.Wait(ctx, 999999); err == nil {
			t.Error("Wait on a ticket that was never submitted must fail, not succeed")
		}
	})
}

// TestAdversarial_DoubleWaitOnSameTicketDoesNotOrphan.
//
// Wait assigned s.pending[ticket] unconditionally. A second Wait on the same ticket
// replaced the first waiter's channel, so the completion was delivered to the second
// and the first blocked forever with nothing left holding a reference to it.
func TestAdversarial_DoubleWaitOnSameTicketDoesNotOrphan(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A slow job, so both waiters are registered before it completes.
	ticket, err := h.Submit(ctx, []byte{5, 5}) // 5 iterations x 10ms
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	var wg sync.WaitGroup
	var errs [2]error
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wctx, wcancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer wcancel()
			_, errs[i] = h.Wait(wctx, ticket)
		}(i)
		time.Sleep(10 * time.Millisecond) // stagger so the second arrives after the first
	}

	mustReturnWithin(t, 8*time.Second, "two concurrent Waits on one ticket", func() {
		wg.Wait()
	})

	// Exactly one waiter owns the ticket; the other must be told so rather than
	// silently parked on a channel nothing will ever send to.
	ok := 0
	for i, e := range errs {
		if e == nil {
			ok++
		} else {
			t.Logf("waiter %d: %v", i, e)
		}
	}
	if ok > 1 {
		t.Fatalf("a completion was delivered to %d waiters; it can only be moved out once", ok)
	}
}

// TestPitfall_DoubleWaitDoesNotReleaseSemaphore is the I4 half of ErrTicketBusy.
//
// waitInternal deferred releaseSem on every return, including ErrTicketBusy. The
// second waiter therefore consumed the owner's semaphore permit while the job was
// still running, so a third Submit on a pool-size-1 handle could enter Rust and
// breach the in-flight bound. Ticket ownership (one waiter) is necessary but not
// sufficient: the permit still belongs to the owner until that waiter returns.
func TestPitfall_DoubleWaitDoesNotReleaseSemaphore(t *testing.T) {
	const poolSize = 1
	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// Mode 5: 25 iterations × 10ms ≈ 250ms, long enough for the busy waiter and
	// the probe Submit to run while the owner is still in flight.
	ticket, err := h.Submit(context.Background(), []byte{5, 25})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	ownerDone := make(chan error, 1)
	go func() {
		_, waitErr := h.Wait(context.Background(), ticket)
		ownerDone <- waitErr
	}()

	// The owner must be the registered waiter before the busy probe runs.
	time.Sleep(20 * time.Millisecond)

	_, err = h.Wait(context.Background(), ticket)
	if !errors.Is(err, gusset.ErrTicketBusy) {
		t.Fatalf("expected ErrTicketBusy, got %v", err)
	}

	probeCtx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	probeTicket, err := h.Submit(probeCtx, []byte{0})
	if err == nil {
		_, _ = h.Wait(context.Background(), probeTicket)
		t.Fatal("second Wait released the semaphore while the first job was still in flight (I4)")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the probe Submit to wait on the held permit and time out, got %v", err)
	}

	select {
	case waitErr := <-ownerDone:
		if waitErr != nil {
			t.Fatalf("owner Wait failed: %v", waitErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owner Wait did not return")
	}
}

// TestAdversarial_HandleChurnDoesNotLeakDescriptors.
//
// Each handle owns a completion-pipe write descriptor. Releasing it twice would
// close whatever unrelated file had since been given that number, and never
// releasing it exhausts the process descriptor table.
func TestAdversarial_HandleChurnDoesNotLeakDescriptors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Warm up so one-off allocations are not counted as a leak.
	for i := 0; i < 5; i++ {
		h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		_, _ = h.Call(ctx, []byte{0, 1})
		_ = h.Close()
	}
	runtime.GC()

	before := openFDCount(t)

	const cycles = 150
	for i := 0; i < cycles; i++ {
		h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open failed at cycle %d: %v", i, err)
		}
		if _, err := h.Call(ctx, []byte{0, byte(i)}); err != nil {
			t.Fatalf("Call failed at cycle %d: %v", i, err)
		}
		buf, err := h.NewBuffer(1024)
		if err != nil {
			t.Fatalf("NewBuffer failed at cycle %d: %v", i, err)
		}
		if err := buf.Free(); err != nil {
			t.Fatalf("Free failed at cycle %d: %v", i, err)
		}
		if err := h.Close(); err != nil {
			t.Fatalf("Close failed at cycle %d: %v", i, err)
		}
	}

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	after := openFDCount(t)

	t.Logf("open fds before %d cycles: %d, after: %d", cycles, before, after)
	// Each leaked cycle costs two descriptors, so 150 cycles would be +300.
	if after > before+20 {
		t.Fatalf("descriptor leak across %d open/close cycles: %d -> %d", cycles, before, after)
	}
}

// TestAdversarial_PoisonStormUnderConcurrency drives panics and healthy work through
// one handle from many goroutines at once.
//
// Poisoning is a one-way latch reached from a worker thread while other callers are
// mid-flight. Nothing here may deadlock, double-release a semaphore permit, or take
// the process down; every call must end in a result or a clean error.
func TestAdversarial_PoisonStormUnderConcurrency(t *testing.T) {
	const goroutines = 128

	h, err := gusset.Open(gusset.WithPoolSize(8), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	var wg sync.WaitGroup
	var ok, panicked, poisoned, other atomic.Int64

	// A start barrier, so the panics land while many callers are genuinely in
	// flight. Without it the first panic latches the poison before most goroutines
	// have even been scheduled, and the run degenerates into 120 instant refusals
	// that never touch the concurrent path this test exists to cover.
	start := make(chan struct{})

	mustReturnWithin(t, 60*time.Second, "poison storm", func() {
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()

				var payload []byte
				switch id % 5 {
				case 0:
					// A slow panic: the job sleeps before panicking, so healthy work
					// is still running on other workers when the poison latches.
					payload = []byte{1}
				case 1:
					payload = []byte{2} // panic with an embedded NUL byte
				case 2:
					payload = []byte{3} // non-string panic payload
				default:
					payload = []byte{0, byte(id)} // healthy echo
				}

				<-start
				_, err := h.Call(ctx, payload)
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, gusset.ErrPanic):
					panicked.Add(1)
				case errors.Is(err, gusset.ErrPoisoned):
					poisoned.Add(1)
				default:
					other.Add(1)
				}
			}(g)
		}
		// Give every goroutine time to reach the barrier before releasing them.
		time.Sleep(50 * time.Millisecond)
		close(start)
		wg.Wait()
	})

	total := ok.Load() + panicked.Load() + poisoned.Load() + other.Load()
	t.Logf("ok=%d panic=%d poisoned=%d other=%d (total %d)",
		ok.Load(), panicked.Load(), poisoned.Load(), other.Load(), total)

	if total != goroutines {
		t.Fatalf("every call must terminate: accounted for %d of %d", total, goroutines)
	}
	if panicked.Load() == 0 {
		t.Fatal("expected at least one caught panic to have been reported")
	}

	// Poisoning latches: once poisoned, the handle stays refused.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.Call(ctx, []byte{0, 1}); !errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("after a caught panic the handle must stay poisoned, got: %v", err)
	}
}

// TestAdversarial_CloseRacesEveryOperation runs Close concurrently with every public
// operation, repeatedly.
//
// Close frees the Rust handle, so any operation still holding that pointer is a
// use-after-free. Operations must either complete or return a clean error.
func TestAdversarial_CloseRacesEveryOperation(t *testing.T) {
	const trials = 30
	var completedTrials atomic.Int64

	mustReturnWithin(t, 180*time.Second, "close-vs-everything race", func() {
		for trial := 0; trial < trials; trial++ {
			h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
			if err != nil {
				t.Errorf("trial %d: Open failed: %v", trial, err)
				return
			}

			var wg sync.WaitGroup
			start := make(chan struct{})

			worker := func(fn func()) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					fn()
				}()
			}

			for i := 0; i < 6; i++ {
				worker(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					_, _ = h.Call(ctx, []byte{0, 7})
				})
				worker(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					if tk, err := h.Submit(ctx, []byte{0, 8}); err == nil {
						wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
						defer wcancel()
						_, _ = h.Wait(wctx, tk)
					}
				})
				worker(func() {
					if b, err := h.NewBuffer(4096); err == nil {
						_ = b.Bytes()
						_ = b.Free()
					}
				})
			}

			// Close joins the race from its own goroutine, after the others have
			// genuinely started work. Closing at t=0 mostly tests "operations on an
			// already-closed handle", which is the easy half; the interesting window
			// is Close landing while calls are in flight in Rust.
			worker(func() {
				time.Sleep(time.Duration(5+(trial%20)) * time.Millisecond)
				_ = h.Close()
			})

			close(start)
			wg.Wait()
			completedTrials.Add(1)

			// Idempotent: a second Close must be a clean no-op, not a double free.
			if err := h.Close(); err != nil {
				t.Errorf("trial %d: second Close returned %v", trial, err)
				return
			}
		}
	})

	if completedTrials.Load() != trials {
		t.Fatalf("only %d of %d race trials ran to completion", completedTrials.Load(), trials)
	}
	t.Logf("%d close-vs-everything trials completed", completedTrials.Load())
}

// TestAdversarial_DeadlineStormDoesNotLeakPermits.
//
// Every caller whose context expires must still drain its ticket before releasing
// its semaphore permit (I4). A permit released early admits an extra caller and
// breaks the bound; a permit never released starves the handle.
func TestAdversarial_DeadlineStormDoesNotLeakPermits(t *testing.T) {
	const poolSize = 4
	const waves = 30

	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// Run in waves of exactly pool-size callers rather than flooding the handle.
	//
	// A flood is a weaker test than it looks: with 300 callers against 4 permits,
	// almost every context expires while still queued on the Go semaphore, so the
	// caller returns before it ever submits and the cancel-and-drain path — the part
	// that can actually leak a permit — is never reached.
	//
	// Each wave fills every permit, and every caller's deadline lands in the middle
	// of its job, so each one exercises cancel-then-drain exactly once.
	var timedOut, completed atomic.Int64

	mustReturnWithin(t, 180*time.Second, "deadline storm", func() {
		for w := 0; w < waves; w++ {
			var wg sync.WaitGroup
			for i := 0; i < poolSize; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					// Job runs 10 x 10ms = 100ms; the deadline lands at 40ms.
					ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
					defer cancel()
					if _, err := h.Call(ctx, []byte{5, 10}); err != nil {
						timedOut.Add(1)
					} else {
						completed.Add(1)
					}
				}()
			}
			wg.Wait()
		}
	})

	t.Logf("waves=%d timed_out=%d completed=%d", waves, timedOut.Load(), completed.Load())

	// The point of the test is the cancel path, so assert it was actually taken.
	if timedOut.Load() < int64(waves*poolSize)/2 {
		t.Fatalf("expected most callers to time out mid-job and exercise the drain path, only %d did",
			timedOut.Load())
	}

	// If any permit leaked, the handle can no longer admit pool-size callers.
	mustReturnWithin(t, 30*time.Second, "post-storm capacity check", func() {
		var wg sync.WaitGroup
		for i := 0; i < poolSize; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if _, err := h.Call(ctx, []byte{0, 1}); err != nil {
					t.Errorf("handle lost capacity after the deadline storm: %v", err)
				}
			}()
		}
		wg.Wait()
	})
}

// TestAdversarial_BufferEdgeSizesAndDoubleFree covers the boundaries of the
// Rust-owned buffer path: the 4 KiB inline/buffer threshold, 64-byte alignment, and
// repeated frees.
func TestAdversarial_BufferEdgeSizesAndDoubleFree(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// Zero and negative sizes are refused rather than producing a zero-length
	// allocation that Rust would reject later, or a nil slice callers would write to.
	for _, n := range []int{0, -1, -4096} {
		if _, err := h.NewBuffer(n); err == nil {
			t.Fatalf("NewBuffer(%d) must fail", n)
		}
	}

	for _, n := range []int{1, 63, 64, 65, 4095, 4096, 4097, 1 << 20} {
		buf, err := h.NewBuffer(n)
		if err != nil {
			t.Fatalf("NewBuffer(%d) failed: %v", n, err)
		}
		b := buf.Bytes()
		if len(b) != n {
			t.Fatalf("NewBuffer(%d) returned %d bytes", n, len(b))
		}
		// Write the whole span: a short allocation shows up here as a fault.
		for i := range b {
			b[i] = byte(i)
		}

		// Free twice plus once more after a GC: only the first may do anything.
		if err := buf.Free(); err != nil {
			t.Fatalf("Free(%d) failed: %v", n, err)
		}
		if err := buf.Free(); err != nil {
			t.Fatalf("second Free(%d) must be a no-op, got: %v", n, err)
		}
		runtime.GC()
		if err := buf.Free(); err != nil {
			t.Fatalf("third Free(%d) after GC must be a no-op, got: %v", n, err)
		}
	}
}

// TestAdversarial_SubmitBufferUnderGCPressure.
//
// Submit takes a *Buffer for its id alone. Once that field is read nothing
// references the Buffer, so its AddCleanup backstop becomes eligible and could free
// the Rust memory while, or before, Rust resolves the id. GOGC-level pressure plus
// an explicit GC between submit and wait is what makes that window observable.
func TestAdversarial_SubmitBufferUnderGCPressure(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	mustReturnWithin(t, 90*time.Second, "buffer submit under GC pressure", func() {
		for i := 0; i < 200; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

			buf, err := h.NewBuffer(8192)
			if err != nil {
				cancel()
				t.Errorf("iteration %d: NewBuffer failed: %v", i, err)
				return
			}
			b := buf.Bytes()
			b[0] = 0 // diagnostic mode 0: echo
			for j := 1; j < len(b); j++ {
				b[j] = byte(j)
			}

			ticket, err := h.Submit(ctx, buf)
			if err != nil {
				cancel()
				t.Errorf("iteration %d: Submit failed: %v", i, err)
				return
			}

			// Maximum pressure exactly in the window.
			runtime.GC()

			out, err := h.Wait(ctx, ticket)
			if err != nil {
				cancel()
				t.Errorf("iteration %d: Wait failed: %v", i, err)
				return
			}
			if len(out) != 8192 {
				cancel()
				t.Errorf("iteration %d: expected 8192 bytes echoed, got %d", i, len(out))
				return
			}
			if out[100] != byte(100) {
				cancel()
				t.Errorf("iteration %d: buffer contents corrupted: out[100]=%d", i, out[100])
				return
			}

			_ = buf.Free()
			cancel()
		}
	})
}

// TestAdversarial_CancelIsSafeForEveryTicketState cancels tickets that never
// existed, tickets already completed, and the same ticket repeatedly.
func TestAdversarial_CancelIsSafeForEveryTicketState(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// A completed ticket, then repeated cancellation of it via expired contexts.
	ticket, err := h.Submit(ctx, []byte{0, 42})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	if _, err := h.Wait(ctx, ticket); err != nil {
		t.Fatalf("Wait failed: %v", err)
	}

	mustReturnWithin(t, 20*time.Second, "cancel of odd ticket states", func() {
		for i := 0; i < 50; i++ {
			dead, deadCancel := context.WithTimeout(context.Background(), time.Nanosecond)
			// Unknown ticket, already-taken ticket, and a huge id.
			_, _ = h.Wait(dead, ticket)
			_, _ = h.Wait(dead, ^uint64(0))
			deadCancel()
		}
	})

	// The handle is still usable afterwards.
	if _, err := h.Call(ctx, []byte{0, 7}); err != nil {
		t.Fatalf("handle unusable after cancel storm: %v", err)
	}
}

// TestAdversarial_EmptyAndOversizedInputs covers payload boundaries: nil, empty, the
// 4 KiB inline threshold, and a payload far past it.
func TestAdversarial_EmptyAndOversizedInputs(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// nil and empty are the same thing to the boundary and must not fault.
	for _, in := range [][]byte{nil, {}} {
		if _, err := h.Call(ctx, in); err != nil {
			t.Fatalf("Call with %v input failed: %v", in, err)
		}
	}

	// R16: []byte at or under 4 KiB is copied during submit. Above that the
	// slice is refused — the previous version of this test expected a 1 MiB
	// memcpy on the cgo thread, which is the long call I4/R16 forbid.
	for _, n := range []int{1, 4095, 4096} {
		in := make([]byte, n)
		in[0] = 0 // echo
		for i := 1; i < n; i++ {
			in[i] = byte(i)
		}
		out, err := h.Call(ctx, in)
		if err != nil {
			t.Fatalf("Call with %d-byte input failed: %v", n, err)
		}
		if len(out) != n {
			t.Fatalf("echo of %d bytes returned %d", n, len(out))
		}
		if n > 1000 && out[999] != in[999] {
			t.Fatalf("echo corrupted at offset 999 for %d-byte input", n)
		}
	}
	for _, n := range []int{4097, 1 << 16, 1 << 20} {
		in := make([]byte, n)
		in[0] = 0
		if _, err := h.Call(ctx, in); err == nil {
			t.Fatalf("Call with %d-byte []byte must be refused (R16); use NewBuffer", n)
		}
		buf, err := h.NewBuffer(n)
		if err != nil {
			t.Fatalf("NewBuffer(%d) failed: %v", n, err)
		}
		copy(buf.Bytes(), in)
		ticket, err := h.Submit(ctx, buf)
		if err != nil {
			_ = buf.Free()
			t.Fatalf("Submit Buffer(%d) failed: %v", n, err)
		}
		out, err := h.Wait(ctx, ticket)
		_ = buf.Free()
		if err != nil {
			t.Fatalf("Wait Buffer(%d) failed: %v", n, err)
		}
		if len(out) != n {
			t.Fatalf("echo of %d-byte Buffer returned %d", n, len(out))
		}
	}
}

// TestAdversarial_NoGoroutineLeakAfterChurn asserts that handles do not leave
// reader goroutines behind. Each handle starts one drainPipe goroutine that must
// exit on Close.
func TestAdversarial_NoGoroutineLeakAfterChurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Warm up first: the runtime starts helpers on first use.
	for i := 0; i < 5; i++ {
		h, _ := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
		_, _ = h.Call(ctx, []byte{0, 1})
		_ = h.Close()
	}
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()

	const cycles = 100
	for i := 0; i < cycles; i++ {
		h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open failed at cycle %d: %v", i, err)
		}
		if _, err := h.Call(ctx, []byte{0, byte(i)}); err != nil {
			t.Fatalf("Call failed at cycle %d: %v", i, err)
		}
		if err := h.Close(); err != nil {
			t.Fatalf("Close failed at cycle %d: %v", i, err)
		}
	}

	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()

	t.Logf("goroutines before %d cycles: %d, after: %d", cycles, before, after)
	if after > before+10 {
		t.Fatalf("goroutine leak across %d handle cycles: %d -> %d (one reader per handle leaked)",
			cycles, before, after)
	}
}

// TestAdversarial_ConcurrentHandlesAreIndependent.
//
// Poisoning is per handle. A panic on one must not refuse work on another, which is
// what a process-global latch would do.
func TestAdversarial_ConcurrentHandlesAreIndependent(t *testing.T) {
	const handles = 8

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	hs := make([]*gusset.Handle, handles)
	for i := range hs {
		h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open %d failed: %v", i, err)
		}
		hs[i] = h
		defer h.Close()
	}

	// Poison the even-numbered handles.
	for i := 0; i < handles; i += 2 {
		if _, err := hs[i].Call(ctx, []byte{1}); !errors.Is(err, gusset.ErrPanic) {
			t.Fatalf("handle %d: expected a caught panic, got %v", i, err)
		}
	}

	var wg sync.WaitGroup
	for i := range hs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := hs[i].Call(ctx, []byte{0, byte(i)})
			if i%2 == 0 {
				if !errors.Is(err, gusset.ErrPoisoned) {
					t.Errorf("handle %d was poisoned and must refuse work, got %v", i, err)
				}
				return
			}
			if err != nil {
				t.Errorf("handle %d was never poisoned and must still work, got %v", i, err)
			}
		}(i)
	}

	mustReturnWithin(t, 30*time.Second, "independent handle check", wg.Wait)
}

// TestAdversarial_SustainedThroughputStaysBounded is the long soak: sustained load
// across several handles while asserting the OS thread count stays bounded (I4/R11).
func TestAdversarial_SustainedThroughputStaysBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("soak skipped in -short mode")
	}

	const handles = 4
	const poolSize = 4
	const callers = 64
	const perCaller = 200

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	hs := make([]*gusset.Handle, handles)
	for i := range hs {
		h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open %d failed: %v", i, err)
		}
		hs[i] = h
		defer h.Close()
	}

	baseline := gusset.Threads()
	var completed atomic.Int64
	var failed atomic.Int64
	var peakThreads atomic.Int64
	peakThreads.Store(baseline)

	mustReturnWithin(t, 180*time.Second, "sustained throughput soak", func() {
		var wg sync.WaitGroup
		for c := 0; c < callers; c++ {
			wg.Add(1)
			go func(c int) {
				defer wg.Done()
				h := hs[c%handles]
				for j := 0; j < perCaller; j++ {
					payload := []byte{0, byte(c), byte(j)}
					if _, err := h.Call(ctx, payload); err != nil {
						failed.Add(1)
						return
					}
					completed.Add(1)
					if j%50 == 0 {
						if n := gusset.Threads(); n > peakThreads.Load() {
							peakThreads.Store(n)
						}
					}
				}
			}(c)
		}
		wg.Wait()
	})

	t.Logf("completed=%d failed=%d baseline_threads=%d peak_threads=%d",
		completed.Load(), failed.Load(), baseline, peakThreads.Load())

	if failed.Load() != 0 {
		t.Fatalf("%d calls failed under sustained load", failed.Load())
	}
	if completed.Load() != int64(callers*perCaller) {
		t.Fatalf("expected %d completions, got %d", callers*perCaller, completed.Load())
	}

	// I4/R11: callers park on the Go semaphore, not on OS threads. 64 concurrent
	// callers must not translate into 64 threads.
	limit := int64(handles*poolSize + runtime.GOMAXPROCS(0) + 16)
	if peakThreads.Load() > limit {
		t.Fatalf("thread count %d exceeded the bound %d: callers are blocking on OS threads (I4 breach)",
			peakThreads.Load(), limit)
	}
}
