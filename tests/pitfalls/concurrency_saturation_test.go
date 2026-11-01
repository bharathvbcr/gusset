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
			baseLive := gusset.Stats().LiveBytes
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
			finalLive := gusset.Stats().LiveBytes
			if finalLive != baseLive {
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

	baseLive := gusset.Stats().LiveBytes
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
	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
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

	baseLive := gusset.Stats().LiveBytes
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
	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
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
