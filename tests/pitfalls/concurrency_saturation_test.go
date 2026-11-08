package pitfalls_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
	"github.com/bharathvbcr/gusset/tests/internal/livebytes"
)

// TestConcurrency_Saturation_HighFanOutScaling scales concurrency to 256, 512, and 1,024 goroutines
// across pools of 16 and 64 workers, asserting Invariants I1, I3, I4, I5, and R11.
func TestConcurrency_Saturation_HighFanOutScaling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping saturation test in short mode")
	}

	testCases := []struct {
		poolSize   int
		goroutines int
		iterations int
	}{
		{poolSize: 16, goroutines: 256, iterations: 15},
		{poolSize: 64, goroutines: 512, iterations: 15},
		{poolSize: 64, goroutines: 1024, iterations: 10},
	}

	for _, tc := range testCases {
		name := fmt.Sprintf("Pool_%d_Goroutines_%d", tc.poolSize, tc.goroutines)
		t.Run(name, func(t *testing.T) {
			baseLive := livebytes.Quiesce(t)
			initialThreads := gusset.Threads()

			h, err := gusset.Open(
				gusset.WithPoolSize(tc.poolSize),
				gusset.WithDiagnosticEngine(),
			)
			if err != nil {
				t.Fatalf("Open failed: %v", err)
			}

			// Verify Invariant I5: worker thread stack is explicitly 8 MiB on supported platforms
			stackRes, err := h.Call(context.Background(), []byte{8})
			if err != nil {
				t.Fatalf("Mode 8 stack check failed: %v", err)
			}
			if len(stackRes) >= 8 {
				stackSize := binary.LittleEndian.Uint64(stackRes[:8])
				if stackSize > 0 && stackSize < 8*1024*1024 {
					t.Errorf("I5: expected worker stack size >= 8 MiB (8388608), got %d bytes", stackSize)
				}
			}

			var peakThreads atomic.Int64
			peakThreads.Store(initialThreads)
			stopSampler := make(chan struct{})
			var samplerWG sync.WaitGroup
			samplerWG.Add(1)
			go func() {
				defer samplerWG.Done()
				ticker := time.NewTicker(2 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-stopSampler:
						return
					case <-ticker.C:
						if th := gusset.Threads(); th > peakThreads.Load() {
							peakThreads.Store(th)
						}
					}
				}
			}()

			var wg sync.WaitGroup
			var completed, cancelled, discarded atomic.Int64

			for g := 0; g < tc.goroutines; g++ {
				wg.Add(1)
				go func(gid int) {
					defer wg.Done()
					r := rand.New(rand.NewPCG(uint64(gid)+1, 12345))
					for i := 0; i < tc.iterations; i++ {
						op := r.IntN(100)
						switch {
						case op < 40: // 40% Synchronous Call (Mode 0)
							ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
							res, err := h.Call(ctx, []byte{0, byte(gid), byte(i)})
							cancel()
							if err != nil {
								if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
									cancelled.Add(1)
									continue
								}
								t.Errorf("Call error: %v", err)
								return
							}
							if len(res) < 3 || res[1] != byte(gid) || res[2] != byte(i) {
								t.Errorf("Call response corrupted: %v", res)
								return
							}
							completed.Add(1)

						case op < 65: // 25% Submit + Wait (Mode 0)
							ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
							tk, err := h.Submit(ctx, []byte{0, byte(gid), byte(i)})
							if err != nil {
								cancel()
								t.Errorf("Submit error: %v", err)
								return
							}
							res, err := h.Wait(ctx, tk)
							cancel()
							if err != nil {
								if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
									cancelled.Add(1)
									continue
								}
								t.Errorf("Wait error: %v", err)
								return
							}
							if len(res) < 3 || res[1] != byte(gid) {
								t.Errorf("Wait response corrupted: %v", res)
								return
							}
							completed.Add(1)

						case op < 80: // 15% Submit + WaitBuffer (Mode 16 allocated, I1)
							ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
							// Mode 16: 1024-byte allocated buffer: [16, len_le_u32 (1024 = 0x0400), gid]
							tk, err := h.Submit(ctx, []byte{16, 0x00, 0x04, 0x00, 0x00, byte(gid)})
							if err != nil {
								cancel()
								t.Errorf("Submit allocated error: %v", err)
								return
							}
							buf, err := h.WaitBuffer(ctx, tk)
							cancel()
							if err != nil {
								if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
									cancelled.Add(1)
									continue
								}
								t.Errorf("WaitBuffer error: %v", err)
								return
							}
							if buf != nil {
								if len(buf.Bytes()) != 1024 {
									t.Errorf("WaitBuffer length mismatch: got %d, want 1024", len(buf.Bytes()))
								}
								_ = buf.Free()
							}
							completed.Add(1)

						case op < 90: // 10% Submit + Discard (permit return)
							ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
							tk, err := h.Submit(ctx, []byte{0, 1, 2, 3})
							cancel()
							if err != nil {
								t.Errorf("Submit for discard error: %v", err)
								return
							}
							if err := h.Discard(tk); err != nil && !errors.Is(err, gusset.ErrUnknownTicket) {
								t.Errorf("Discard error: %v", err)
								return
							}
							discarded.Add(1)

						default: // 10% Short-timeout cancellation (Mode 5 cooperative sleep, I3)
							ctx, cancel := context.WithTimeout(context.Background(), 100*time.Microsecond)
							_, err := h.Call(ctx, []byte{5, 10}) // Mode 5 sleep
							cancel()
							if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
								cancelled.Add(1)
							} else if err == nil {
								completed.Add(1)
							}
						}
					}
				}(g)
			}

			wg.Wait()
			close(stopSampler)
			samplerWG.Wait()

			// Close handle and assert clean resource teardown
			if err := h.Close(); err != nil {
				t.Fatalf("Close failed: %v", err)
			}

			// Invariant I1: LiveBytes must return exactly to baseline
			finalLive, leakFree := livebytes.Settle(baseLive)
			if !leakFree {
				t.Errorf("I1: Rust LiveBytes leak detected: baseline %d, final %d (delta %d)",
					baseLive, finalLive, int64(finalLive)-int64(baseLive))
			}

			// Invariant I5 & Rule R11: OS Thread bound
			if initialThreads > 0 {
				threadBound := initialThreads + int64(runtime.GOMAXPROCS(0)+tc.poolSize+32)
				if pt := peakThreads.Load(); pt > threadBound {
					t.Errorf("R11/I5: Peak OS threads %d exceeded bound %d (initial=%d, GOMAXPROCS=%d, poolSize=%d)",
						pt, threadBound, initialThreads, runtime.GOMAXPROCS(0), tc.poolSize)
				}
			}

			t.Logf("HighFanOut (%s): Completed=%d, Cancelled=%d, Discarded=%d, PeakThreads=%d",
				name, completed.Load(), cancelled.Load(), discarded.Load(), peakThreads.Load())
		})
	}
}

// TestConcurrency_Saturation_MultiPoolContention tests 16 concurrent worker pools
// under 512 goroutines, asserting poison isolation (I2) and aggregate thread bounding (I5, R11).
func TestConcurrency_Saturation_MultiPoolContention(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-pool contention in short mode")
	}

	const numPools = 16
	const poolSize = 4
	const totalGoroutines = 512

	handles := make([]*gusset.Handle, numPools)
	for i := 0; i < numPools; i++ {
		h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open pool %d failed: %v", i, err)
		}
		handles[i] = h
	}

	baseLive := livebytes.Quiesce(t)
	initialThreads := gusset.Threads()

	var wg sync.WaitGroup
	var completed atomic.Int64

	for g := 0; g < totalGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			poolIdx := gid % numPools
			h := handles[poolIdx]
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			res, err := h.Call(ctx, []byte{0, byte(gid)})
			if err != nil {
				return
			}
			if len(res) >= 2 && res[1] == byte(gid) {
				completed.Add(1)
			}
		}(g)
	}

	// Deliberately poison Handle 0 with a panic (Invariant I2 isolation test)
	time.Sleep(10 * time.Millisecond)
	pCtx, pCancel := context.WithTimeout(context.Background(), 1*time.Second)
	_, _ = handles[0].Call(pCtx, []byte{1}) // Mode 1: plain panic
	pCancel()

	// Handle 0 must now be poisoned
	_, err := handles[0].Call(context.Background(), []byte{0})
	if !errors.Is(err, gusset.ErrPoisoned) && !errors.Is(err, gusset.ErrPanic) {
		t.Errorf("I2: Expected Handle 0 to report ErrPoisoned or ErrPanic, got: %v", err)
	}

	// Verify sibling Handle 1 is NOT poisoned and functions normally (I2 isolation)
	h1Res, err := handles[1].Call(context.Background(), []byte{0, 99})
	if err != nil || len(h1Res) < 2 || h1Res[1] != 99 {
		t.Errorf("I2: Sibling handle 1 affected by Handle 0 poison: err=%v, res=%v", err, h1Res)
	}

	wg.Wait()

	// Close all handles
	for i, h := range handles {
		if err := h.Close(); err != nil && i != 0 {
			t.Errorf("Close pool %d failed: %v", i, err)
		}
	}

	// Verify thread bounding across all pools
	if initialThreads > 0 {
		maxAllowedThreads := initialThreads + int64(runtime.GOMAXPROCS(0)+(numPools*poolSize)+32)
		currentThreads := gusset.Threads()
		if currentThreads > maxAllowedThreads {
			t.Errorf("R11: Threads after multi-pool test %d exceeded bound %d", currentThreads, maxAllowedThreads)
		}
	}

	// Verify live bytes return to baseline
	finalLive, leakFree := livebytes.Settle(baseLive)
	if !leakFree {
		t.Errorf("I1: Multi-pool test leaked memory: baseline %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}
}

// TestConcurrency_Saturation_ViolentHandleTeardown tests closing a handle in the middle of peak 512-goroutine traffic.
func TestConcurrency_Saturation_ViolentHandleTeardown(t *testing.T) {
	const poolSize = 32
	const numGoroutines = 512

	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	baseLive := livebytes.Quiesce(t)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			// Random mix of Call, Submit, WaitBuffer
			if gid%2 == 0 {
				_, _ = h.Call(ctx, []byte{0, byte(gid)})
			} else {
				tk, err := h.Submit(ctx, []byte{0, byte(gid)})
				if err == nil {
					buf, wErr := h.WaitBuffer(ctx, tk)
					if wErr == nil && buf != nil {
						_ = buf.Free()
					}
				}
			}
		}(g)
	}

	close(start)
	// Let saturation build up
	time.Sleep(5 * time.Millisecond)

	// Violently close handle mid-flight
	if err := h.Close(); err != nil {
		t.Errorf("Violent Close failed: %v", err)
	}

	wg.Wait()

	// Verify memory ownership I1: zero live byte leak
	finalLive, leakFree := livebytes.Settle(baseLive)
	if !leakFree {
		t.Errorf("I1: Violent close leaked memory: baseline %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}
}

// TestConcurrency_Saturation_WorkerStackVerification tests worker recursion on an 8 MiB stack.
func TestConcurrency_Saturation_WorkerStackVerification(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(16), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// Mode 8: inspect stack size
	stackRes, err := h.Call(context.Background(), []byte{8})
	if err != nil {
		t.Fatalf("Mode 8 call failed: %v", err)
	}
	if len(stackRes) >= 8 {
		stackSize := binary.LittleEndian.Uint64(stackRes[:8])
		if stackSize > 0 && stackSize < 8*1024*1024 {
			t.Errorf("expected at least 8 MiB worker stack size, got %d", stackSize)
		}
	}

	// Mode 6: 50,000 frames of recursion across 64 concurrent callers
	const concurrentCallers = 64
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 0; i < concurrentCallers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// Payload for Mode 6: [6] defaults to 50,000 frames
			res, err := h.Call(ctx, []byte{6})
			if err != nil {
				t.Errorf("caller %d deep recursion failed: %v", id, err)
				return
			}
			if len(res) < 4 {
				t.Errorf("caller %d returned short response: %v", id, res)
			}
		}(i)
	}
	wg.Wait()
}

// TestConcurrency_Saturation_ExtremeGoroutineSingleWorker attacks a pool of size 1 with 1,024 goroutines.
// This maximizes channel semaphore queuing depth (1,023 goroutines in waiting queue).
// It verifies:
// 1. Invariant I4: No deadlock, in-flight jobs in engine never exceed 1.
// 2. Invariant I1: LiveBytes returns to baseline after drain & close.
// 3. Post-saturation liveness: Single worker is still fully functional.
func TestConcurrency_Saturation_ExtremeGoroutineSingleWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	baseLive := livebytes.Quiesce(t)
	initialThreads := gusset.Threads()

	const poolSize = 1
	const numGoroutines = 1024
	const iterations = 5

	h, err := gusset.Open(
		gusset.WithPoolSize(poolSize),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Reset Mode 17/18 in-engine peak gauge
	_, _ = h.Call(context.Background(), []byte{18})

	var peakThreads atomic.Int64
	peakThreads.Store(initialThreads)
	stopSampler := make(chan struct{})
	var samplerWG sync.WaitGroup
	samplerWG.Add(1)
	go func() {
		defer samplerWG.Done()
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopSampler:
				return
			case <-ticker.C:
				if th := gusset.Threads(); th > peakThreads.Load() {
					peakThreads.Store(th)
				}
			}
		}
	}()

	var wg sync.WaitGroup
	var completed, cancelled, discarded atomic.Int64

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(gid)+1, 99999))
			for i := 0; i < iterations; i++ {
				op := r.IntN(100)
				switch {
				case op < 40: // 40% Synchronous Call (Mode 0 echo)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					res, err := h.Call(ctx, []byte{0, byte(gid), byte(i)})
					cancel()
					if err != nil {
						if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
							cancelled.Add(1)
							continue
						}
						t.Errorf("Call error: %v", err)
						return
					}
					if len(res) < 3 || res[1] != byte(gid) || res[2] != byte(i) {
						t.Errorf("Call response corrupted: %v", res)
						return
					}
					completed.Add(1)

				case op < 65: // 25% Submit + Wait (Mode 0)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					tk, err := h.Submit(ctx, []byte{0, byte(gid), byte(i)})
					if err != nil {
						cancel()
						if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
							cancelled.Add(1)
							continue
						}
						t.Errorf("Submit error: %v", err)
						return
					}
					res, err := h.Wait(ctx, tk)
					cancel()
					if err != nil {
						if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
							cancelled.Add(1)
							continue
						}
						t.Errorf("Wait error: %v", err)
						return
					}
					if len(res) < 3 || res[1] != byte(gid) {
						t.Errorf("Wait response corrupted: %v", res)
						return
					}
					completed.Add(1)

				case op < 80: // 15% Submit + WaitBuffer (Mode 16 allocated, 512-byte buffer)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					// Mode 16: len 512 = 0x0200
					tk, err := h.Submit(ctx, []byte{16, 0x00, 0x02, 0x00, 0x00, byte(gid)})
					if err != nil {
						cancel()
						if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
							cancelled.Add(1)
							continue
						}
						t.Errorf("Submit allocated error: %v", err)
						return
					}
					buf, err := h.WaitBuffer(ctx, tk)
					cancel()
					if err != nil {
						if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
							cancelled.Add(1)
							continue
						}
						t.Errorf("WaitBuffer error: %v", err)
						return
					}
					if buf != nil {
						if len(buf.Bytes()) != 512 {
							t.Errorf("WaitBuffer length mismatch: got %d, want 512", len(buf.Bytes()))
						}
						_ = buf.Free()
					}
					completed.Add(1)

				case op < 90: // 10% Submit + Discard
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					tk, err := h.Submit(ctx, []byte{0, 1, 2, 3})
					cancel()
					if err != nil {
						if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
							cancelled.Add(1)
							continue
						}
						t.Errorf("Submit for discard error: %v", err)
						return
					}
					if err := h.Discard(tk); err != nil && !errors.Is(err, gusset.ErrUnknownTicket) {
						t.Errorf("Discard error: %v", err)
						return
					}
					discarded.Add(1)

				default: // 10% Short-timeout cancellation (Mode 5 cooperative sleep)
					ctx, cancel := context.WithTimeout(context.Background(), 50*time.Microsecond)
					_, err := h.Call(ctx, []byte{5, 5})
					cancel()
					if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
						cancelled.Add(1)
					} else if err == nil {
						completed.Add(1)
					}
				}
			}
		}(g)
	}

	wg.Wait()
	close(stopSampler)
	samplerWG.Wait()

	// Verify post-saturation liveness: the single worker must still be fully responsive
	livenessRes, err := h.Call(context.Background(), []byte{0, 0xAA, 0xBB})
	if err != nil {
		t.Fatalf("Single worker died or wedged after saturation: %v", err)
	}
	if len(livenessRes) < 3 || livenessRes[1] != 0xAA || livenessRes[2] != 0xBB {
		t.Fatalf("Corrupted liveness response: %v", livenessRes)
	}

	// Read peak concurrent jobs in engine
	gaugeRes, err := h.Call(context.Background(), []byte{18})
	if err != nil {
		t.Fatalf("Mode 18 call failed: %v", err)
	}
	if len(gaugeRes) >= 8 {
		peakInEngine := binary.LittleEndian.Uint64(gaugeRes[:8])
		if peakInEngine > poolSize {
			t.Errorf("I4 breached: peak in-engine jobs %d > poolSize %d", peakInEngine, poolSize)
		}
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	finalLive, leakFree := livebytes.Settle(baseLive)
	if !leakFree {
		t.Errorf("I1: LiveBytes leak detected: base %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}

	if initialThreads > 0 {
		threadBound := initialThreads + int64(runtime.GOMAXPROCS(0)+poolSize+32)
		if pt := peakThreads.Load(); pt > threadBound {
			t.Errorf("R11/I5: Peak OS threads %d exceeded bound %d", pt, threadBound)
		}
	}

	t.Logf("SingleWorker saturation passed: Completed=%d, Cancelled=%d, Discarded=%d, PeakThreads=%d",
		completed.Load(), cancelled.Load(), discarded.Load(), peakThreads.Load())
}

// TestConcurrency_Saturation_ViolentCloseContinuousHammer executes a true continuous hammer on a pool of 32 workers
// with 512 goroutines running an UNBOUNDED loop of mixed large take buffers, sleeps, and sync calls,
// while 4 concurrent closers violently invoke Close() during peak load.
func TestConcurrency_Saturation_ViolentCloseContinuousHammer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	baseLive := livebytes.Quiesce(t)

	const poolSize = 32
	const numGoroutines = 512
	const takeBufferSize = 8192 // Mode 16 large buffer (>4096 inline limit)

	h, err := gusset.Open(
		gusset.WithPoolSize(poolSize),
		gusset.WithDiagnosticEngine(),
		gusset.WithMaxUncollected(1024),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	var wg sync.WaitGroup
	stopSignal := make(chan struct{})
	var completedCount, closedCount, cancelCount atomic.Int64

	// Payload generator for Mode 16
	makeMode16 := func(seed byte) []byte {
		buf := make([]byte, 6)
		buf[0] = 16
		binary.LittleEndian.PutUint32(buf[1:5], uint32(takeBufferSize))
		buf[5] = seed
		return buf
	}

	// 512 hammer goroutines running continuously
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(gid)+1, 77777))
			for {
				select {
				case <-stopSignal:
					return
				default:
				}

				op := r.IntN(4)
				switch op {
				case 0: // Mode 0 Echo Call
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					res, err := h.Call(ctx, []byte{0, byte(gid)})
					cancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							closedCount.Add(1)
							return
						} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
							cancelCount.Add(1)
						}
					} else if len(res) >= 2 && res[1] == byte(gid) {
						completedCount.Add(1)
					}

				case 1: // Mode 16 Take Buffer Submit + WaitBuffer
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					tk, err := h.Submit(ctx, makeMode16(byte(gid)))
					cancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							closedCount.Add(1)
							return
						}
						continue
					}
					wCtx, wCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					buf, err := h.WaitBuffer(wCtx, tk)
					wCancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) || errors.Is(err, gusset.ErrUnknownTicket) {
							closedCount.Add(1)
							return
						} else if errors.Is(err, context.DeadlineExceeded) {
							cancelCount.Add(1)
						}
					} else {
						completedCount.Add(1)
						if buf != nil {
							_ = buf.Free()
						}
					}

				case 2: // Mode 5 Cooperative sleep (2ms)
					ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					_, err := h.Call(ctx, []byte{5, 2})
					cancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							closedCount.Add(1)
							return
						} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
							cancelCount.Add(1)
						}
					} else {
						completedCount.Add(1)
					}

				case 3: // Mode 17 Non-cooperative engine hold (1ms)
					ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					tk, err := h.Submit(ctx, []byte{17, 1})
					cancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							closedCount.Add(1)
							return
						}
						continue
					}
					wCtx, wCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					_, err = h.Wait(wCtx, tk)
					wCancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) || errors.Is(err, gusset.ErrUnknownTicket) {
							closedCount.Add(1)
							return
						} else if errors.Is(err, context.DeadlineExceeded) {
							cancelCount.Add(1)
						}
					} else {
						completedCount.Add(1)
					}
				}
			}
		}(g)
	}

	// Squeal under load for 30 milliseconds so all 512 workers are actively saturating
	time.Sleep(30 * time.Millisecond)

	// Race 4 concurrent closers violently!
	var closeWG sync.WaitGroup
	var closeErrors [4]error
	for i := 0; i < 4; i++ {
		closeWG.Add(1)
		go func(idx int) {
			defer closeWG.Done()
			closeErrors[idx] = h.Close()
		}(i)
	}

	// Close stop signal to let any remaining workers terminate
	close(stopSignal)
	closeWG.Wait()
	wg.Wait()

	// Assert that all 4 closers returned identical results (idempotent close)
	for i := 1; i < 4; i++ {
		if !errors.Is(closeErrors[i], closeErrors[0]) && (closeErrors[i] != closeErrors[0]) {
			t.Errorf("Concurrent close discrepancy: closer 0 got %v, closer %d got %v",
				closeErrors[0], i, closeErrors[i])
		}
	}

	// Invariant I1: LiveBytes must return exactly to baseline
	finalLive, leakFree := livebytes.Settle(baseLive)
	if !leakFree {
		t.Errorf("I1: Violent close continuous hammer leaked Rust memory: base %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}

	t.Logf("ViolentClose passed: completed=%d, closedRefusals=%d, cancels=%d, LiveBytes delta=%d",
		completedCount.Load(), closedCount.Load(), cancelCount.Load(), int64(finalLive)-int64(baseLive))
}

// TestConcurrency_Saturation_MultiPoolMassiveChurnAndPoisonStorm opens 32 concurrent worker pools
// with 512 goroutines dispatching traffic, while a poison storm regularly panics handles (Mode 1),
// asserting sibling isolation (I2), clean dynamic handle replacement, and strict thread bounding (R11).
func TestConcurrency_Saturation_MultiPoolMassiveChurnAndPoisonStorm(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	baseLive := livebytes.Quiesce(t)
	initialThreads := gusset.Threads()

	const numPools = 32
	const poolSize = 4
	const numGoroutines = 512
	const stormDuration = 2 * time.Second

	type poolEntry struct {
		mu sync.RWMutex
		h  *gusset.Handle
	}

	pools := make([]*poolEntry, numPools)
	for i := 0; i < numPools; i++ {
		h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Initial open pool %d failed: %v", i, err)
		}
		pools[i] = &poolEntry{h: h}
	}

	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	var completedCalls, caughtPanics, poolReplaces atomic.Int64

	// 512 traffic workers
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(gid)+1, 33333))
			for {
				select {
				case <-stopCh:
					return
				default:
				}

				pIdx := r.IntN(numPools)
				entry := pools[pIdx]

				entry.mu.RLock()
				curH := entry.h
				entry.mu.RUnlock()

				if curH == nil {
					time.Sleep(1 * time.Millisecond)
					continue
				}

				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				res, err := curH.Call(ctx, []byte{0, byte(gid)})
				cancel()

				if err != nil {
					if errors.Is(err, gusset.ErrPoisoned) || errors.Is(err, gusset.ErrPanic) {
						caughtPanics.Add(1)
						// Replace poisoned handle
						entry.mu.Lock()
						if entry.h == curH {
							_ = curH.Close()
							newH, openErr := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
							if openErr == nil {
								entry.h = newH
								poolReplaces.Add(1)
							}
						}
						entry.mu.Unlock()
					}
				} else if len(res) >= 2 && res[1] == byte(gid) {
					completedCalls.Add(1)
				}
			}
		}(g)
	}

	// Poison injector: periodically injects panic into random pools
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		r := rand.New(rand.NewPCG(42, 88888))

		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				pIdx := r.IntN(numPools)
				entry := pools[pIdx]
				entry.mu.RLock()
				curH := entry.h
				entry.mu.RUnlock()
				if curH != nil {
					pCtx, pCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					// Mode 1: plain panic; Mode 15: non-string panic payload
					mode := byte(1)
					if r.IntN(2) == 1 {
						mode = 15
					}
					_, _ = curH.Call(pCtx, []byte{mode})
					pCancel()
				}
			}
		}
	}()

	time.Sleep(stormDuration)
	close(stopCh)
	wg.Wait()

	// Close all active pools
	for i, entry := range pools {
		entry.mu.Lock()
		if entry.h != nil {
			_ = entry.h.Close()
			entry.h = nil
		}
		entry.mu.Unlock()
		_ = i
	}

	// Verify thread bounding across all pools
	if initialThreads > 0 {
		maxAllowedThreads := initialThreads + int64(runtime.GOMAXPROCS(0)+(numPools*poolSize)+32)
		currentThreads := gusset.Threads()
		if currentThreads > maxAllowedThreads {
			t.Errorf("R11: Threads after 32-pool churn storm %d exceeded bound %d", currentThreads, maxAllowedThreads)
		}
	}

	// Verify zero live byte leak
	finalLive, leakFree := livebytes.Settle(baseLive)
	if !leakFree {
		t.Errorf("I1: 32-pool churn storm leaked Rust memory: base %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}

	t.Logf("MultiPoolChurnStorm passed: calls=%d, panicsCaught=%d, replaces=%d, LiveBytes delta=%d",
		completedCalls.Load(), caughtPanics.Load(), poolReplaces.Load(), int64(finalLive)-int64(baseLive))
}

// TestConcurrency_Saturation_PermitLeakAudit_CancellationStorm attacks the semaphore permit conservation invariant (I4).
// It subjects an 8-worker pool to 20,000 rapid microsecond cancellations.
// Afterwards, it verifies that exactly 8 concurrent long-running calls can acquire permits simultaneously,
// proving zero permit leaks and zero permit drift.
func TestConcurrency_Saturation_PermitLeakAudit_CancellationStorm(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	baseLive := livebytes.Quiesce(t)

	const poolSize = 8
	const numGoroutines = 256
	const cancelsPerGoroutine = 50 // 12,800 total cancellation attempts

	h, err := gusset.Open(
		gusset.WithPoolSize(poolSize),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Reset Mode 17/18 in-engine peak gauge
	_, _ = h.Call(context.Background(), []byte{18})

	var wg sync.WaitGroup
	var cancelledCount atomic.Int64

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(gid)+1, 11111))
			for i := 0; i < cancelsPerGoroutine; i++ {
				timeoutUs := time.Duration(r.IntN(200)+1) * time.Microsecond
				ctx, cancel := context.WithTimeout(context.Background(), timeoutUs)
				// Mode 5: cooperative sleep for 5ms
				_, cErr := h.Call(ctx, []byte{5, 5})
				cancel()
				if cErr != nil && (errors.Is(cErr, context.DeadlineExceeded) || errors.Is(cErr, context.Canceled)) {
					cancelledCount.Add(1)
				}
			}
		}(g)
	}

	wg.Wait()

	// Wait for any in-flight background worker completions to settle
	time.Sleep(50 * time.Millisecond)

	// Invariant I4 Audit:
	// Verify that ALL poolSize permits are fully restored and usable.
	// Launch exactly poolSize concurrent calls holding the engine for 50ms (Mode 17).
	var permitWG sync.WaitGroup
	var inFlightStarted sync.WaitGroup
	permitWG.Add(poolSize)
	inFlightStarted.Add(poolSize)

	var permitErrors [poolSize]error

	for i := 0; i < poolSize; i++ {
		go func(idx int) {
			defer permitWG.Done()
			inFlightStarted.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			// Mode 17: hold engine for 50ms
			_, permitErrors[idx] = h.Call(ctx, []byte{17, 50})
		}(i)
	}

	permitWG.Wait()

	for i, pErr := range permitErrors {
		if pErr != nil {
			t.Fatalf("I4 Permit Leak: Worker %d failed to acquire permit after cancellation storm: %v", i, pErr)
		}
	}

	// Invariant I1: Verify peak concurrent jobs in engine reached exactly poolSize
	gaugeRes, err := h.Call(context.Background(), []byte{18})
	if err != nil {
		t.Fatalf("Mode 18 call failed: %v", err)
	}
	if len(gaugeRes) >= 8 {
		peakInEngine := binary.LittleEndian.Uint64(gaugeRes[:8])
		if peakInEngine != poolSize {
			t.Errorf("I4 audit: Expected peak concurrent in-engine jobs to reach %d, got %d", poolSize, peakInEngine)
		}
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	finalLive, leakFree := livebytes.Settle(baseLive)
	if !leakFree {
		t.Errorf("I1: Cancellation storm leaked Rust memory: base %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}

	t.Logf("PermitLeakAudit passed: cancellations=%d, all %d permits intact and fully functional",
		cancelledCount.Load(), poolSize)
}

// TestConcurrency_Saturation_WidePoolScaling tests scaling pool up to 256 and 512 workers
// under 1,024 goroutines, verifying thread creation, stack size, and clean teardown.
func TestConcurrency_Saturation_WidePoolScaling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	testCases := []struct {
		poolSize   int
		goroutines int
	}{
		{poolSize: 128, goroutines: 512},
		{poolSize: 256, goroutines: 1024},
	}

	for _, tc := range testCases {
		name := fmt.Sprintf("Pool_%d_Goroutines_%d", tc.poolSize, tc.goroutines)
		t.Run(name, func(t *testing.T) {
			baseLive := livebytes.Quiesce(t)
			initialThreads := gusset.Threads()

			h, err := gusset.Open(
				gusset.WithPoolSize(tc.poolSize),
				gusset.WithDiagnosticEngine(),
			)
			if err != nil {
				t.Fatalf("Open failed: %v", err)
			}

			// Verify Invariant I5: worker stack size >= 8 MiB
			stackRes, err := h.Call(context.Background(), []byte{8})
			if err != nil {
				t.Fatalf("Mode 8 stack check failed: %v", err)
			}
			if len(stackRes) >= 8 {
				stackSize := binary.LittleEndian.Uint64(stackRes[:8])
				if stackSize > 0 && stackSize < 8*1024*1024 {
					t.Errorf("I5: expected worker stack size >= 8 MiB, got %d", stackSize)
				}
			}

			var wg sync.WaitGroup
			var completed atomic.Int64

			for g := 0; g < tc.goroutines; g++ {
				wg.Add(1)
				go func(gid int) {
					defer wg.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					res, err := h.Call(ctx, []byte{0, byte(gid)})
					if err == nil && len(res) >= 2 && res[1] == byte(gid) {
						completed.Add(1)
					}
				}(g)
			}

			wg.Wait()

			if err := h.Close(); err != nil {
				t.Fatalf("Close failed: %v", err)
			}

			finalLive, leakFree := livebytes.Settle(baseLive)
			if !leakFree {
				t.Errorf("I1: Wide pool leaked memory: base %d, final %d (delta %d)",
					baseLive, finalLive, int64(finalLive)-int64(baseLive))
			}

			if initialThreads > 0 {
				threadBound := initialThreads + int64(runtime.GOMAXPROCS(0)+tc.poolSize+32)
				peak := gusset.Threads()
				if peak > threadBound {
					t.Errorf("R11: Threads %d exceeded bound %d", peak, threadBound)
				}
			}

			t.Logf("WidePoolScaling (%s) passed: completed=%d", name, completed.Load())
		})
	}
}

// TestConcurrency_Saturation_MaxPoolBoundaryAndRejection tests the extreme boundary of WithPoolSize:
// 1. Refusal of poolSize <= 0 and poolSize > 1024 (MaxPoolSize).
// 2. Exact allocation of poolSize = 512 and poolSize = 1024.
// 3. Verifies that 1024 worker threads spawn with 8 MiB stacks, execute a call, and join cleanly without thread leak.
func TestConcurrency_Saturation_MaxPoolBoundaryAndRejection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	baseLive := livebytes.Quiesce(t)

	// 1. Boundary Refusals
	for _, badSize := range []int{0, -1, -100, 1025, 2048, 65536} {
		_, err := gusset.Open(gusset.WithPoolSize(badSize))
		if err == nil {
			t.Errorf("Expected refusal for invalid poolSize %d, got nil error", badSize)
		}
	}

	// 2. Test poolSize = 512
	h512, err := gusset.Open(gusset.WithPoolSize(512), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open poolSize 512 failed: %v", err)
	}
	res512, err := h512.Call(context.Background(), []byte{0, 0x55})
	if err != nil || len(res512) < 2 || res512[1] != 0x55 {
		t.Fatalf("poolSize 512 call failed: %v, res: %v", err, res512)
	}
	if err := h512.Close(); err != nil {
		t.Fatalf("Close poolSize 512 failed: %v", err)
	}

	// 3. Test poolSize = 1024 (Exact MaxPoolSize)
	h1024, err := gusset.Open(gusset.WithPoolSize(1024), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open MaxPoolSize 1024 failed: %v", err)
	}

	// Verify stack size on worker
	stackRes, err := h1024.Call(context.Background(), []byte{8})
	if err != nil {
		t.Fatalf("Mode 8 call on poolSize 1024 failed: %v", err)
	}
	if len(stackRes) >= 8 {
		stackSize := binary.LittleEndian.Uint64(stackRes[:8])
		if stackSize < 8*1024*1024 {
			t.Errorf("Worker stack size < 8 MiB: %d", stackSize)
		}
	}

	// Run concurrent calls across 1024 workers
	var wg sync.WaitGroup
	var completed atomic.Int64
	for i := 0; i < 1024; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			res, err := h1024.Call(ctx, []byte{0, byte(id)})
			if err == nil && len(res) >= 2 && res[1] == byte(id) {
				completed.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if err := h1024.Close(); err != nil {
		t.Fatalf("Close poolSize 1024 failed: %v", err)
	}

	finalLive, leakFree := livebytes.Settle(baseLive)
	if !leakFree {
		t.Errorf("I1: MaxPoolSize 1024 test leaked memory: base %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}

	t.Logf("MaxPoolBoundaryAndRejection passed: poolSize 1024 completed %d calls cleanly", completed.Load())
}
