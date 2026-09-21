package pitfalls_test

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
)

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
	if _, err := h.CallBuffer(nil, in); err == nil { //nolint:staticcheck // nil ctx is the point
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
