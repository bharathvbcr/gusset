package pitfalls_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestChallenge_M2_TeardownTakeBufferLeakAttack executes Attack 1:
//  1. Open a handle with a wide worker pool (e.g. 16 workers).
//  2. Submit 200 large asynchronous jobs (>4096 bytes) returning Rust take buffers.
//  3. Let completions populate s.takeIDs in Go without calling Wait() or Discard().
//  4. Immediately invoke h.Close().
//  5. Measure gusset.Stats().LiveBytes and assert exact zero delta (LiveBytes == baselineLive),
//     proving teardown reordering completely eliminates buffer leaks.
func TestChallenge_M2_TeardownTakeBufferLeakAttack(t *testing.T) {
	baselineLive := gusset.Stats().LiveBytes

	const poolSize = 16
	const jobCount = 200
	const bufferSize = 8192 // > 4096 MAX_INLINE_INPUT, promotes to Rust take buffer

	h, err := gusset.Open(
		gusset.WithPoolSize(poolSize),
		gusset.WithDiagnosticEngine(),
		gusset.WithMaxUncollected(jobCount+50), // Ensure queue does not block
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	ctx := context.Background()
	// Mode 16 (diagnostic_allocated): input[1..5] is output length in u32 LE.
	// 8192 = 0x2000 => []byte{16, 0x00, 0x20, 0x00, 0x00, byte(i)}
	mode16Input := func(seed byte) []byte {
		return []byte{16, 0x00, 0x20, 0x00, 0x00, seed}
	}

	for i := 0; i < jobCount; i++ {
		tk, err := h.Submit(ctx, mode16Input(byte(i)))
		if err != nil {
			t.Fatalf("Submit %d failed: %v", i, err)
		}
		if tk == 0 {
			t.Fatalf("Submit %d returned zero ticket", i)
		}
	}

	// Allow the worker pool and drain loop to process all 200 jobs and buffer completions into takeIDs.
	// We expect live bytes to rise by at least jobCount * bufferSize (1,638,400 bytes).
	expectedGrowth := uint64(jobCount * bufferSize)
	deadline := time.Now().Add(5 * time.Second)
	var activeLive uint64
	for time.Now().Before(deadline) {
		activeLive = gusset.Stats().LiveBytes
		if activeLive >= baselineLive+expectedGrowth {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	if activeLive < baselineLive+expectedGrowth {
		t.Fatalf("expected LiveBytes to increase to at least %d (growth >= %d), got activeLive %d",
			baselineLive+expectedGrowth, expectedGrowth, activeLive)
	}

	// Immediately invoke h.Close() WITHOUT calling Wait() or Discard().
	// Teardown reordering in handleState.close() must clean up all 200 take buffers.
	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baselineLive {
		t.Fatalf("LEAK DETECTED: expected LiveBytes to return to baseline %d after Close(), got %d (delta %d)",
			baselineLive, finalLive, int64(finalLive)-int64(baselineLive))
	}
}

// TestChallenge_M2_TeardownTakeBufferLeakAttack_InFlightClose tests the variation where
// 200 large jobs are submitted and h.Close() is invoked immediately while jobs are actively in flight.
func TestChallenge_M2_TeardownTakeBufferLeakAttack_InFlightClose(t *testing.T) {
	baselineLive := gusset.Stats().LiveBytes

	const poolSize = 16
	const jobCount = 200

	h, err := gusset.Open(
		gusset.WithPoolSize(poolSize),
		gusset.WithDiagnosticEngine(),
		gusset.WithMaxUncollected(jobCount+50),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	ctx := context.Background()
	mode16Input := []byte{16, 0x00, 0x20, 0x00, 0x00, 0x55}

	for i := 0; i < jobCount; i++ {
		_, err := h.Submit(ctx, mode16Input)
		if err != nil && !errors.Is(err, gusset.ErrClosed) {
			t.Fatalf("Submit %d failed: %v", i, err)
		}
	}

	// Immediately close without waiting.
	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baselineLive {
		t.Fatalf("LEAK DETECTED: expected LiveBytes to return to baseline %d after in-flight Close(), got %d (delta %d)",
			baselineLive, finalLive, int64(finalLive)-int64(baselineLive))
	}
}

// TestChallenge_M2_HighConcurrencyRaceTest executes Attack 2:
// Runs 100 concurrent goroutines submitting and abandoning take buffers
// while concurrently closing and reopening handles under `go test -race`.
func TestChallenge_M2_HighConcurrencyRaceTest(t *testing.T) {
	baselineLive := gusset.Stats().LiveBytes

	// Handle holder that safely coordinates handle swapping
	type handleHolder struct {
		mu sync.RWMutex
		h  *gusset.Handle
	}

	createHandle := func() *gusset.Handle {
		h, err := gusset.Open(
			gusset.WithPoolSize(8),
			gusset.WithDiagnosticEngine(),
			gusset.WithMaxUncollected(1024),
		)
		if err != nil {
			panic(err)
		}
		return h
	}

	holder := &handleHolder{
		h: createHandle(),
	}

	const goroutines = 100
	const runDuration = 3 * time.Second

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	var submitSuccess atomic.Int64
	var submitClosed atomic.Int64
	var waitSuccess atomic.Int64
	var waitClosed atomic.Int64
	var discardCount atomic.Int64
	var abandonTimeoutCount atomic.Int64
	var handleSwapCount atomic.Int64

	// Mode 16 large buffer (8192 bytes > 4096)
	mode16Input := []byte{16, 0x00, 0x20, 0x00, 0x00, 0x42}

	// 1. Worker goroutines (100 goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for {
				select {
				case <-stopCh:
					return
				default:
				}

				// Fetch current handle under read lock
				holder.mu.RLock()
				curH := holder.h
				holder.mu.RUnlock()

				if curH == nil {
					time.Sleep(1 * time.Millisecond)
					continue
				}

				// Action branch based on goroutine id / iteration:
				// id % 4 == 0: Submit + WaitBuffer + Free
				// id % 4 == 1: Submit + Discard
				// id % 4 == 2: Submit + Abandon via Context timeout
				// id % 4 == 3: Submit and abandon completely (uncollected)
				action := (id + int(time.Now().UnixNano())) % 4

				switch action {
				case 0:
					// Submit + WaitBuffer + Free
					sCtx, sCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
					tk, err := curH.Submit(sCtx, mode16Input)
					sCancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							submitClosed.Add(1)
						}
						continue
					}
					submitSuccess.Add(1)

					// WaitBuffer with timeout
					wCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					buf, err := curH.WaitBuffer(wCtx, tk)
					cancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) || errors.Is(err, gusset.ErrUnknownTicket) {
							waitClosed.Add(1)
						}
					} else {
						waitSuccess.Add(1)
						_ = buf.Free()
					}

				case 1:
					// Submit + Discard
					sCtx, sCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
					tk, err := curH.Submit(sCtx, mode16Input)
					sCancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							submitClosed.Add(1)
						}
						continue
					}
					submitSuccess.Add(1)

					// Concurrently discard
					if err := curH.Discard(tk); err == nil {
						discardCount.Add(1)
					}

				case 2:
					// Submit + Abandon via expired context
					sCtx, sCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
					tk, err := curH.Submit(sCtx, mode16Input)
					sCancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							submitClosed.Add(1)
						}
						continue
					}
					submitSuccess.Add(1)

					// Wait with expired context to trigger abandonment
					expCtx, expCancel := context.WithTimeout(context.Background(), 1*time.Microsecond)
					time.Sleep(2 * time.Microsecond)
					_, err = curH.Wait(expCtx, tk)
					expCancel()
					if errors.Is(err, context.DeadlineExceeded) {
						abandonTimeoutCount.Add(1)
					}

				case 3:
					// Submit and abandon completely (fire-and-forget take buffer)
					sCtx, sCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
					_, err := curH.Submit(sCtx, mode16Input)
					sCancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							submitClosed.Add(1)
						}
						continue
					}
					submitSuccess.Add(1)
				}
			}
		}(i)
	}

	// 2. Churn goroutine: Concurrently closes and reopens handles
	churnStop := make(chan struct{})
	var churnWg sync.WaitGroup
	churnWg.Add(1)
	go func() {
		defer churnWg.Done()
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-churnStop:
				return
			case <-ticker.C:
				// Swap handle under write lock
				holder.mu.Lock()
				oldH := holder.h
				holder.h = createHandle()
				holder.mu.Unlock()

				handleSwapCount.Add(1)

				// Close old handle concurrently while 100 workers might still be referencing it
				if oldH != nil {
					_ = oldH.Close()
				}
			}
		}
	}()

	// Run for runDuration
	time.Sleep(runDuration)

	// Stop churner
	close(churnStop)
	churnWg.Wait()

	// Stop workers
	close(stopCh)
	wg.Wait()

	// Close the final remaining handle
	holder.mu.Lock()
	if holder.h != nil {
		_ = holder.h.Close()
		holder.h = nil
	}
	holder.mu.Unlock()

	// Assert stats
	finalLive := gusset.Stats().LiveBytes
	t.Logf("Concurrency Stats: Submits=%d (Closed=%d), WaitBuffer=%d (Closed=%d), Discards=%d, AbandonTimeouts=%d, HandleSwaps=%d",
		submitSuccess.Load(), submitClosed.Load(), waitSuccess.Load(), waitClosed.Load(),
		discardCount.Load(), abandonTimeoutCount.Load(), handleSwapCount.Load())

	if finalLive != baselineLive {
		t.Fatalf("LEAK DETECTED after high-concurrency race test: expected LiveBytes to return to baseline %d, got %d (delta %d)",
			baselineLive, finalLive, int64(finalLive)-int64(baselineLive))
	}
}
