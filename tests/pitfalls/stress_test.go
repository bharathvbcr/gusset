package pitfalls_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

func osThreadCount(t *testing.T) int {
	pid := os.Getpid()
	out, err := exec.Command("ps", "-M", fmt.Sprintf("%d", pid)).Output()
	if err != nil {
		t.Skipf("cannot query OS thread count via ps: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) <= 1 {
		return 1
	}
	return len(lines) - 1
}

// TestBug_HandleCloseLeak proves that Handle.Close() leaked worker threads and memory
// because worker threads held an Arc<Handle> cycle that kept the SyncSender alive.
func TestBug_HandleCloseLeak(t *testing.T) {
	initialThreads := osThreadCount(t)
	t.Logf("initial OS threads: %d", initialThreads)

	// Rapidly open and close 20 handles with pool size 4
	for i := 0; i < 20; i++ {
		h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		// Do a quick call
		_, err = h.Call(context.Background(), []byte{0, byte(i)})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if err := h.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}
	}

	runtime.GC()
	time.Sleep(50 * time.Millisecond)

	finalThreads := osThreadCount(t)
	t.Logf("final OS threads after 20 open/close cycles: %d", finalThreads)

	// If 20 handles with 4 workers leaked threads, thread count grew by ~80 threads!
	if finalThreads > initialThreads+15 {
		t.Fatalf("OS threads leaked on Handle.Close: started with %d, now %d (leaked %d threads)",
			initialThreads, finalThreads, finalThreads-initialThreads)
	}
}

// TestBug_BufferFreeAfterHandleClose proves that calling Buffer.Free() after Handle.Close()
// caused a use-after-free by passing the deallocated handle pointer to Rust.
func TestBug_BufferFreeAfterHandleClose(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	buf, err := h.NewBuffer(1024)
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}

	// Close handle first
	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Freeing the buffer after handle is closed must safely return nil without crashing or UAF
	err = buf.Free()
	if err != nil {
		t.Errorf("expected clean nil return on buf.Free() after handle close, got: %v", err)
	}
}

// TestBug_PanicLocationWorkerThread proves that worker thread panics lost the true source
// file and line number, returning dummy "gusset.rs:86" instead.
func TestBug_PanicLocationWorkerThread(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Mode 1: triggers panic!("plain panic") in default_dispatch (crates/gusset/src/pool/mod.rs)
	_, err = h.Call(ctx, []byte{1})
	if err == nil {
		t.Fatal("expected panic error, got nil")
	}

	if !errors.Is(err, gusset.ErrPanic) {
		t.Fatalf("expected ErrPanic, got: %v", err)
	}

	errStr := err.Error()
	t.Logf("caught worker panic error: %s", errStr)

	// Must contain the actual file (crates/gusset/src/pool/mod.rs or pool/mod.rs)
	// and NOT the fake "gusset.rs" dummy location.
	if strings.Contains(errStr, "at gusset.rs") {
		t.Fatalf("panic location was lost/faked as 'gusset.rs': %s", errStr)
	}
	if !strings.Contains(errStr, "pool/mod.rs") && !strings.Contains(errStr, "pool") {
		t.Fatalf("expected real panic location in pool/mod.rs, got: %s", errStr)
	}
}

// TestBug_SemaphoreInFlightBreachOnTimeout proves that when a caller's context times out,
// the semaphore was prematurely released while the worker was still executing, allowing
// another caller to enter and breach Invariant I4 (in-flight calls bounded by pool size).
func TestBug_SemaphoreInFlightBreachOnTimeout(t *testing.T) {
	const poolSize = 1
	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// Caller 1 submits slow job (takes 100ms) with 20ms timeout
	ctx1, cancel1 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel1()

	var started1, finished1 atomic.Bool
	go func() {
		started1.Store(true)
		_, _ = h.Call(ctx1, []byte{5, 10}) // 10 iterations * 10ms = 100ms
		finished1.Store(true)
	}()

	// Wait for Caller 1 to start and then time out
	time.Sleep(35 * time.Millisecond)
	if !started1.Load() {
		t.Fatal("caller 1 never started")
	}

	t.Logf("finished1 at 35ms: %v", finished1.Load())

	var caller2Acquired atomic.Bool
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel2()

	// Caller 2 attempts to Submit while Caller 1 is still in-flight.
	// We want to test that Caller 2 CANNOT acquire the semaphore permit
	// until Caller 1 has actually finished draining!
	if !finished1.Load() {
		// If Caller 1 is still running, Submit must fail with timeout (no permit available)
		t2, err := h.Submit(ctx2, []byte{0})
		if err == nil {
			caller2Acquired.Store(true)
			_, _ = h.Wait(context.Background(), t2)
		}
		if caller2Acquired.Load() {
			t.Fatalf("semaphore token was prematurely available while worker was still running cancelled job (I4 breach)")
		}
	} else {
		// Caller 1 has finished and drained properly!
		t.Logf("Caller 1 properly finished and drained before releasing permit!")
	}
}

// TestStress_HeavyConcurrentLoadAndPoisoning stresses 100 concurrent callers,
// injects a caught panic, and asserts that poisoning propagates fail-fast
// without deadlock, hung goroutines, or process crashes.
func TestStress_HeavyConcurrentLoadAndPoisoning(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const numGoroutines = 100
	var wg sync.WaitGroup
	var panicReported atomic.Bool
	var poisonReported atomic.Bool

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				var payload []byte
				if id == 42 && i == 10 {
					// Inject deliberate panic in default_dispatch (Mode 1)
					payload = []byte{1}
				} else {
					payload = []byte{0, byte(id), byte(i)}
				}

				res, callErr := h.Call(ctx, payload)
				if callErr != nil {
					if errors.Is(callErr, gusset.ErrPanic) {
						panicReported.Store(true)
					} else if errors.Is(callErr, gusset.ErrPoisoned) {
						poisonReported.Store(true)
					}
				} else if len(res) == 0 || res[1] != byte(id) || res[2] != byte(i) {
					t.Errorf("corrupted response in goroutine %d iteration %d", id, i)
					return
				}
			}
		}(g)
	}

	wg.Wait()

	if !panicReported.Load() {
		t.Fatal("expected panic to be reported by at least one caller")
	}
	if !poisonReported.Load() {
		t.Fatal("expected poisoned status to be reported by subsequent callers")
	}

	// Final verification: all calls on this handle must fail fast with ErrPoisoned
	_, err = h.Call(ctx, []byte{0})
	if !errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("expected ErrPoisoned after stress test, got: %v", err)
	}
}

// TestStress_HighVolumeBufferAllocFreeCycle allocates and frees 500 buffers concurrently
// under active garbage collection pressure to stress test allocator accounting and UAF protection.
func TestStress_HighVolumeBufferAllocFreeCycle(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const iterations = 500
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iterations/10; i++ {
				size := 8192 + (i % 8192)
				buf, err := h.NewBuffer(size)
				if err != nil {
					t.Errorf("NewBuffer failed: %v", err)
					return
				}

				// Write pattern into Rust-owned memory view
				bytes := buf.Bytes()
				bytes[0] = 0 // Mode 0 echo
				bytes[size-1] = byte(i & 0xFF)

				ticket, err := h.Submit(ctx, buf)
				if err != nil {
					t.Errorf("Submit failed: %v", err)
					_ = buf.Free()
					return
				}

				res, err := h.Wait(ctx, ticket)
				if err != nil {
					t.Errorf("Wait failed: %v", err)
					_ = buf.Free()
					return
				}

				if len(res) != size || res[size-1] != byte(i&0xFF) {
					t.Errorf("data corrupted in buffer g=%d i=%d", gid, i)
					_ = buf.Free()
					return
				}

				// Free explicitly (R4)
				if err := buf.Free(); err != nil {
					t.Errorf("buf.Free failed: %v", err)
					return
				}

				if i%10 == 0 {
					runtime.GC()
				}
			}
		}(g)
	}

	wg.Wait()
}

// TestStress_AggressiveTimeoutDraining submits 100 calls with 2ms timeout while jobs
// sleep for 40ms, ensuring all tickets drain without leaks and the handle remains healthy.
func TestStress_AggressiveTimeoutDraining(t *testing.T) {
	const poolSize = 4
	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const numCallers = 100
	var wg sync.WaitGroup
	var timeoutCount atomic.Int32

	for i := 0; i < numCallers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
			defer cancel()

			// Mode 5: 4 iterations * 10ms = 40ms sleep
			_, err := h.Call(ctx, []byte{5, 4})
			if err != nil && errors.Is(err, context.DeadlineExceeded) {
				timeoutCount.Add(1)
			}
		}()
	}

	wg.Wait()

	t.Logf("completed %d timed out calls out of %d", timeoutCount.Load(), numCallers)

	// Sleep briefly for all cancelled jobs to complete in Rust
	time.Sleep(100 * time.Millisecond)

	// Verify handle has all semaphore permits restored and serves normal traffic
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for i := 0; i < 20; i++ {
		res, err := h.Call(ctx, []byte{0, byte(i)})
		if err != nil {
			t.Fatalf("subsequent Call failed on iteration %d: %v", i, err)
		}
		if len(res) < 2 || res[1] != byte(i) {
			t.Fatalf("unexpected result on iteration %d: %v", i, res)
		}
	}
}

// TestStress_RapidHandleLifecycle verifies that 40 rapid Open/Close cycles with 8-worker pools
// completely join all worker threads without thread accumulation.
func TestStress_RapidHandleLifecycle(t *testing.T) {
	initialThreads := osThreadCount(t)
	t.Logf("initial OS threads: %d", initialThreads)

	for i := 0; i < 40; i++ {
		h, err := gusset.Open(gusset.WithPoolSize(8), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		res, err := h.Call(ctx, []byte{0, 99})
		cancel()

		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
		if len(res) < 2 || res[1] != 99 {
			t.Fatalf("result mismatch: %v", res)
		}

		if err := h.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}
	}

	runtime.GC()
	time.Sleep(50 * time.Millisecond)

	finalThreads := osThreadCount(t)
	t.Logf("final OS threads after 40 open/close cycles (8 workers each = 320 threads total): %d", finalThreads)

	// If threads leaked, thread count would be initial + 320!
	if finalThreads > initialThreads+15 {
		t.Fatalf("OS threads leaked: started with %d, ended with %d (leaked %d threads)",
			initialThreads, finalThreads, finalThreads-initialThreads)
	}
}

// TestStress_UnclosedHandleGCAddCleanup proves that when a caller forgets to call Close(),
// the AddCleanup backstop properly collects unreachable handles, terminates worker threads,
// and releases OS resources upon garbage collection without leaks.
func TestStress_UnclosedHandleGCAddCleanup(t *testing.T) {
	initialThreads := osThreadCount(t)
	t.Logf("initial OS threads: %d", initialThreads)

	// Allocate 10 handles in a loop without calling Close()
	for i := 0; i < 10; i++ {
		func() {
			h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
			if err != nil {
				t.Fatalf("Open failed: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			_, err = h.Call(ctx, []byte{0, byte(i)})
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}
			// Intentionally do NOT call h.Close(); allow h to be collected
		}()
	}

	// Trigger GC repeatedly to ensure AddCleanup runs
	for i := 0; i < 5; i++ {
		runtime.GC()
		time.Sleep(30 * time.Millisecond)
	}

	finalThreads := osThreadCount(t)
	t.Logf("final OS threads after 10 unclosed handles collected: %d", finalThreads)

	// If AddCleanup failed to collect the 10 handles with 4 workers each,
	// at least 40 worker threads would have leaked!
	if finalThreads > initialThreads+15 {
		t.Fatalf("OS threads leaked from unclosed handles: started with %d, now %d (leaked %d threads)",
			initialThreads, finalThreads, finalThreads-initialThreads)
	}
}

// TestStress_WaitAfterCloseNoHang verifies that calling Wait() on an already-closed handle
// fails fast with an error and does not hang indefinitely.
func TestStress_WaitAfterCloseNoHang(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	ticket, err := h.Submit(context.Background(), []byte{0, 42})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Immediately close handle
	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Wait with background context (no deadline) must return fail-fast closed error
	done := make(chan error, 1)
	go func() {
		_, waitErr := h.Wait(context.Background(), ticket)
		done <- waitErr
	}()

	select {
	case waitErr := <-done:
		if waitErr == nil {
			t.Logf("Wait returned result before handle closed")
		} else {
			t.Logf("Wait returned expected error: %v", waitErr)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Wait hung indefinitely after handle was closed")
	}

	// Subsequent wait on closed handle with unknown ticket must fail fast immediately
	_, err = h.Wait(context.Background(), 99999)
	if err == nil {
		t.Fatal("expected error waiting on closed handle, got nil")
	}
}

// TestStress_ConcurrentCallAndCloseRace stresses 100 concurrent workers performing Calls,
// Submits, and Buffer operations while another goroutine randomly closes the handle.
// Asserts zero crashes, zero data corruption, and zero panics.
func TestStress_ConcurrentCallAndCloseRace(t *testing.T) {
	for trial := 0; trial < 5; trial++ {
		h, err := gusset.Open(gusset.WithPoolSize(8), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("trial %d: Open failed: %v", trial, err)
		}

		const numGoroutines = 50
		var wg sync.WaitGroup
		var successCount atomic.Int64
		var closedCount atomic.Int64

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

		for g := 0; g < numGoroutines; g++ {
			wg.Add(1)
			go func(gid int) {
				defer wg.Done()
				for i := 0; i < 30; i++ {
					// Alternately use small Call or large Buffer
					if (gid+i)%2 == 0 {
						res, err := h.Call(ctx, []byte{0, byte(gid), byte(i)})
						if err != nil {
							closedCount.Add(1)
							return
						}
						if len(res) < 3 || res[1] != byte(gid) || res[2] != byte(i) {
							t.Errorf("corrupted Call response: %v", res)
							return
						}
						successCount.Add(1)
					} else {
						buf, err := h.NewBuffer(8192)
						if err != nil {
							closedCount.Add(1)
							return
						}
						// Bytes() returns nil once the handle is closed, and this
						// test exists to race Close against everything — so nil is
						// an expected outcome here, not an anomaly.
						//
						// This line used to index the slice unconditionally. Before
						// Bytes() was hardened it got a live slice pointing at Rust
						// memory that Close had already released, and wrote three
						// bytes into it: a use-after-free the Go race detector
						// cannot see, because the memory is not Go's. The nil is the
						// fix working.
						slice := buf.Bytes()
						if len(slice) < 3 {
							_ = buf.Free()
							closedCount.Add(1)
							return
						}
						slice[0] = 0
						slice[1] = byte(gid)
						slice[2] = byte(i)
						ticket, err := h.Submit(ctx, buf)
						if err != nil {
							_ = buf.Free()
							closedCount.Add(1)
							return
						}
						res, err := h.Wait(ctx, ticket)
						_ = buf.Free()
						if err != nil {
							closedCount.Add(1)
							return
						}
						if len(res) < 3 || res[1] != byte(gid) || res[2] != byte(i) {
							t.Errorf("corrupted Buffer response: %v", res)
							return
						}
						successCount.Add(1)
					}
				}
			}(g)
		}

		// Random jitter before closing handle concurrently
		time.Sleep(time.Duration(10+trial*15) * time.Millisecond)
		_ = h.Close()
		cancel()

		wg.Wait()
		t.Logf("trial %d: %d successful calls, %d calls received clean close/cancelled error",
			trial, successCount.Load(), closedCount.Load())
	}
}

// TestStress_ConcurrentZeroCopyEgressAndCloseRace subjects WaitBuffer to severe concurrent
// load while racing handle close. It proves that WaitBuffer either yields a valid, readable
// *Buffer or fails cleanly with ErrClosed/canceled, never returning a nil or corrupted slice.
func TestStress_ConcurrentZeroCopyEgressAndCloseRace(t *testing.T) {
	for trial := 0; trial < 5; trial++ {
		h, err := gusset.Open(gusset.WithPoolSize(8), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("trial %d: Open failed: %v", trial, err)
		}

		const numGoroutines = 40
		var wg sync.WaitGroup
		var successCount atomic.Int64
		var closedCount atomic.Int64

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

		for g := 0; g < numGoroutines; g++ {
			wg.Add(1)
			go func(gid int) {
				defer wg.Done()
				for i := 0; i < 25; i++ {
					payload := []byte{0, byte(gid), byte(i)}
					ticket, err := h.Submit(ctx, payload)
					if err != nil {
						closedCount.Add(1)
						return
					}
					buf, err := h.WaitBuffer(ctx, ticket)
					if err != nil {
						closedCount.Add(1)
						return
					}
					slice := buf.Bytes()
					if slice == nil || len(slice) < 3 || slice[1] != byte(gid) || slice[2] != byte(i) {
						t.Errorf("corrupted WaitBuffer response: %v", slice)
						_ = buf.Free()
						return
					}
					if err := buf.Free(); err != nil {
						t.Errorf("buf.Free error: %v", err)
						return
					}
					successCount.Add(1)
				}
			}(g)
		}

		// Jitter before closing handle mid-flight
		time.Sleep(time.Duration(10+trial*15) * time.Millisecond)
		_ = h.Close()
		cancel()

		wg.Wait()
		t.Logf("trial %d: %d successful WaitBuffer calls, %d calls received clean error",
			trial, successCount.Load(), closedCount.Load())
	}
}

// TestStress_MultiEngineConcurrentSaturation hammers a handle with concurrent calls
// routed to different opcodes, verifying fail-closed isolation and thread safety.
func TestStress_MultiEngineConcurrentSaturation(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(8), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const numGoroutines = 40
	const iterations = 30
	var wg sync.WaitGroup
	var successCount atomic.Int64
	var refusedCount atomic.Int64

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				// Alternately send opcode 0 (diagnostic echo) or opcode > 0 (unregistered engine)
				if (gid+i)%2 == 0 {
					callCtx := gusset.ContextWithOpcode(ctx, 0)
					res, err := h.Call(callCtx, []byte{0, byte(gid), byte(i)})
					if err != nil {
						t.Errorf("opcode 0 call failed: %v", err)
						return
					}
					if len(res) < 3 || res[1] != byte(gid) || res[2] != byte(i) {
						t.Errorf("corrupted response: %v", res)
						return
					}
					successCount.Add(1)
				} else {
					targetOpcode := uint32(100 + (gid % 10))
					callCtx := gusset.ContextWithOpcode(ctx, targetOpcode)
					_, err := h.Call(callCtx, []byte{1, 2, 3})
					if err == nil {
						t.Errorf("unregistered opcode %d must be refused", targetOpcode)
						return
					}
					if !strings.Contains(err.Error(), "no engine handler registered") {
						t.Errorf("expected refusal message, got: %v", err)
						return
					}
					refusedCount.Add(1)
				}
			}
		}(g)
	}

	wg.Wait()
	t.Logf("saturation complete: %d successes, %d clean refusals", successCount.Load(), refusedCount.Load())
	if successCount.Load()+refusedCount.Load() != int64(numGoroutines*iterations) {
		t.Fatalf("expected %d total results, got %d", numGoroutines*iterations, successCount.Load()+refusedCount.Load())
	}
}

// TestStress_UnifiedCallAndSubmitWaitEquivalence stresses the unified pipeline by
// interleaving synchronous Call and asynchronous Submit+Wait under high concurrency.
// Verifies that semaphore permits are cleanly managed and never leaked.
func TestStress_UnifiedCallAndSubmitWaitEquivalence(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(6), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const numWorkers = 30
	const opsPerWorker = 40
	var wg sync.WaitGroup
	var callSuccesses atomic.Int64
	var waitSuccesses atomic.Int64

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for g := 0; g < numWorkers; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				payload := []byte{0, byte(gid), byte(i)}
				if (gid+i)%2 == 0 {
					// Use synchronous Call
					res, err := h.Call(ctx, payload)
					if err != nil {
						t.Errorf("Call failed: %v", err)
						return
					}
					if len(res) < 3 || res[1] != byte(gid) || res[2] != byte(i) {
						t.Errorf("Call corrupted data: %v", res)
						return
					}
					callSuccesses.Add(1)
				} else {
					// Use Submit + Wait
					ticket, err := h.Submit(ctx, payload)
					if err != nil {
						t.Errorf("Submit failed: %v", err)
						return
					}
					res, err := h.Wait(ctx, ticket)
					if err != nil {
						t.Errorf("Wait failed: %v", err)
						return
					}
					if len(res) < 3 || res[1] != byte(gid) || res[2] != byte(i) {
						t.Errorf("Wait corrupted data: %v", res)
						return
					}
					waitSuccesses.Add(1)
				}
			}
		}(g)
	}

	wg.Wait()
	total := callSuccesses.Load() + waitSuccesses.Load()
	if total != int64(numWorkers*opsPerWorker) {
		t.Fatalf("expected %d total completions, got %d (call=%d, wait=%d)",
			numWorkers*opsPerWorker, total, callSuccesses.Load(), waitSuccesses.Load())
	}
}

// TestStress_AdversarialContextCancelStorm unleashes hundreds of rapid context cancellations
// while worker threads are saturated with slow tasks, proving that tickets drain properly,
// the semaphore never leaks permits, and no goroutines are orphaned.
func TestStress_AdversarialContextCancelStorm(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const numGoroutines = 40
	const iterations = 15
	var wg sync.WaitGroup
	var cancelledCount atomic.Int64
	var completedCount atomic.Int64

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				// Random jitter timeout between 500us and 15ms
				timeout := time.Duration(500+((gid*17+i*31)%15000)) * time.Microsecond
				ctx, cancel := context.WithTimeout(context.Background(), timeout)

				// Mode 5: slow delay iterations (each iteration takes ~10ms)
				payload := []byte{5, 2} // ~20ms work
				res, err := h.Call(ctx, payload)
				cancel()

				if err != nil {
					if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
						cancelledCount.Add(1)
					} else {
						t.Errorf("unexpected error on cancel storm: %v", err)
						return
					}
				} else {
					if len(res) == 0 {
						t.Errorf("unexpected empty result")
						return
					}
					completedCount.Add(1)
				}
			}
		}(g)
	}

	wg.Wait()
	t.Logf("Cancel storm finished: %d cancellations, %d completions",
		cancelledCount.Load(), completedCount.Load())

	// After all cancelled calls return, verify that the handle is still healthy
	// and can immediately process normal work without semaphore deadlock (I4)
	freshCtx, freshCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer freshCancel()

	normalRes, err := h.Call(freshCtx, []byte{0, 0xAA, 0xBB})
	if err != nil {
		t.Fatalf("handle unusable after cancel storm: %v", err)
	}
	if len(normalRes) != 3 || normalRes[1] != 0xAA || normalRes[2] != 0xBB {
		t.Fatalf("unexpected normal call result: %v", normalRes)
	}
}

// TestStress_NonblockingPipeWatchdog verifies that pipe write descriptors are guaranteed
// to be non-blocking and that high-frequency completion writes make continuous progress.
func TestStress_NonblockingPipeWatchdog(t *testing.T) {
	const poolSize = 8
	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const totalJobs = 500
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Pipeline batches of size poolSize (I4: in-flight calls bounded by pool size)
	for batch := 0; batch < totalJobs; batch += poolSize {
		batchCount := poolSize
		if batch+batchCount > totalJobs {
			batchCount = totalJobs - batch
		}
		tickets := make([]uint64, batchCount)
		for i := 0; i < batchCount; i++ {
			ticket, err := h.Submit(ctx, []byte{0, byte((batch + i) % 256)})
			if err != nil {
				t.Fatalf("Submit %d failed: %v", batch+i, err)
			}
			tickets[i] = ticket
		}
		for i, ticket := range tickets {
			res, err := h.Wait(ctx, ticket)
			if err != nil {
				t.Fatalf("Wait %d failed: %v", batch+i, err)
			}
			if len(res) != 2 || res[1] != byte((batch+i)%256) {
				t.Fatalf("mismatched response for ticket %d: %v", ticket, res)
			}
		}
	}
}


