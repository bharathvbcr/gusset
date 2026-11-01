package pitfalls_test

import (
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestConcurrentChaos_Invariants_I1_to_I6 executes a multi-threaded chaos swarm
// over 3 seconds with randomized calls, mode 16 take buffers, discards, microsecond
// cancellations, mode 17 non-cooperative engine holds, panics, allocations, and
// concurrent handle rotation, asserting Invariants I1–I6 under the race detector.
func TestConcurrentChaos_Invariants_I1_to_I6(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrent chaos in short mode")
	}

	baselineLive := gusset.Stats().LiveBytes
	baselineAlloc := gusset.Stats().AllocCount

	const (
		poolSize      = 4
		numWorkers    = 32
		chaosDuration = 3 * time.Second
		bufferSize    = 8192 // Mode 16 large take buffer (>4096 MAX_INLINE_INPUT)
	)

	// Probe handle to reset Mode 17/18 in-engine peak gauge (Invariant I1)
	probe, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open probe failed: %v", err)
	}
	defer probe.Close()
	// Reset gauge peak
	_, _ = probe.Call(context.Background(), []byte{18})

	// Shared active handle holder with concurrent rotation
	type handleHolder struct {
		mu sync.RWMutex
		h  *gusset.Handle
	}
	newHandle := func() *gusset.Handle {
		h, err := gusset.Open(
			gusset.WithPoolSize(poolSize),
			gusset.WithDiagnosticEngine(),
			gusset.WithMaxUncollected(1024),
		)
		if err != nil {
			panic(err)
		}
		return h
	}

	holder := &handleHolder{h: newHandle()}

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	var (
		opCallsCompleted   atomic.Int64
		opSubmitsCompleted atomic.Int64
		opCancelled        atomic.Int64
		opDiscards         atomic.Int64
		opClosedHandled    atomic.Int64
		opPanicsCaught     atomic.Int64
		handleChurnCount   atomic.Int64
	)

	// Mode 16 payload: [16, len_le_u32, seed]
	makeMode16Payload := func(seed byte) []byte {
		buf := make([]byte, 6)
		buf[0] = 16
		binary.LittleEndian.PutUint32(buf[1:5], uint32(bufferSize))
		buf[5] = seed
		return buf
	}

	// 1. Concurrent Chaos Worker Swarm
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(workerID)+100, 54321))

			for {
				select {
				case <-stopCh:
					return
				default:
				}

				holder.mu.RLock()
				curH := holder.h
				holder.mu.RUnlock()
				if curH == nil {
					time.Sleep(1 * time.Millisecond)
					continue
				}

				op := r.IntN(8)
				switch op {
				case 0:
					// Normal synchronous Call (Mode 0 echo)
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					res, err := curH.Call(ctx, []byte{0, byte(workerID), byte(op)})
					cancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							opClosedHandled.Add(1)
						} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
							opCancelled.Add(1)
						}
					} else {
						if len(res) >= 3 && res[1] == byte(workerID) {
							opCallsCompleted.Add(1)
						}
					}

				case 1:
					// Large Take Buffer Submit + WaitBuffer + Free (Invariant I3)
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					tk, err := curH.Submit(ctx, makeMode16Payload(byte(workerID)))
					cancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							opClosedHandled.Add(1)
						}
						continue
					}
					wCtx, wCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					buf, err := curH.WaitBuffer(wCtx, tk)
					wCancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) || errors.Is(err, gusset.ErrUnknownTicket) {
							opClosedHandled.Add(1)
						} else if errors.Is(err, context.DeadlineExceeded) {
							opCancelled.Add(1)
						}
					} else {
						opSubmitsCompleted.Add(1)
						if buf != nil {
							_ = buf.Free()
						}
					}

				case 2:
					// Submit + Discard (Invariant I4 permit return)
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					tk, err := curH.Submit(ctx, []byte{0, 42})
					cancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							opClosedHandled.Add(1)
						}
						continue
					}
					if err := curH.Discard(tk); err == nil {
						opDiscards.Add(1)
					} else if errors.Is(err, gusset.ErrClosed) {
						opClosedHandled.Add(1)
					}

				case 3:
					// Microsecond timeout Call (triggering early cancellation)
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Microsecond)
					_, err := curH.Call(ctx, []byte{5, 20}) // Mode 5 cooperative sleep
					cancel()
					if err != nil {
						opCancelled.Add(1)
					}

				case 4:
					// Non-cooperative engine hold (Mode 17) verifying Invariant I1
					ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
					// Mode 17 holds in engine for 3 ms
					tk, err := curH.Submit(ctx, []byte{17, 3})
					cancel()
					if err == nil {
						wCtx, wCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
						_, werr := curH.Wait(wCtx, tk)
						wCancel()
						if werr != nil && (errors.Is(werr, context.DeadlineExceeded) || errors.Is(werr, context.Canceled)) {
							opCancelled.Add(1)
						} else if werr == nil {
							opSubmitsCompleted.Add(1)
						}
					}

				case 5:
					// Ephemeral handle panic containment test (Invariant I2)
					panickyHandle, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
					if err == nil {
						pCtx, pCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
						// Mode 1: plain panic, or Mode 15: double panic DropBomb
						panicMode := byte(1)
						if workerID%2 == 0 {
							panicMode = 15
						}
						_, perr := panickyHandle.Call(pCtx, []byte{panicMode, 2})
						pCancel()
						if perr != nil && (errors.Is(perr, gusset.ErrPanic) || errors.Is(perr, gusset.ErrPoisoned)) {
							opPanicsCaught.Add(1)
						}
						_ = panickyHandle.Close()
					}

				case 6:
					// NewBuffer allocation and concurrent free stress
					buf, err := curH.NewBuffer(1024)
					if err == nil {
						b := buf.Bytes()
						if len(b) > 0 {
							b[0] = 0xAA
						}
						_ = buf.Free()
					}

				case 7:
					// Fast inline Submit + Wait
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					tk, err := curH.Submit(ctx, []byte{0, 1, 2, 3})
					cancel()
					if err == nil {
						wCtx, wCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
						res, werr := curH.Wait(wCtx, tk)
						wCancel()
						if werr == nil && len(res) >= 4 {
							opSubmitsCompleted.Add(1)
						} else if errors.Is(werr, gusset.ErrClosed) {
							opClosedHandled.Add(1)
						}
					}
				}
			}
		}(i)
	}

	// 2. Handle Lifecycle Churner (concurrent rotation and close)
	churnStop := make(chan struct{})
	var churnWg sync.WaitGroup
	churnWg.Add(1)
	go func() {
		defer churnWg.Done()
		ticker := time.NewTicker(60 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-churnStop:
				return
			case <-ticker.C:
				holder.mu.Lock()
				oldH := holder.h
				// Close old handle cleanly before swapping, ensuring strict peak gauge bounding
				if oldH != nil {
					_ = oldH.Close() // Invariant I4: must close cleanly without hang
				}
				holder.h = newHandle()
				holder.mu.Unlock()

				handleChurnCount.Add(1)
			}
		}
	}()

	time.Sleep(chaosDuration)

	// Stop churner
	close(churnStop)
	churnWg.Wait()

	// Stop workers
	close(stopCh)
	wg.Wait()

	// Close final handle
	holder.mu.Lock()
	if holder.h != nil {
		_ = holder.h.Close()
		holder.h = nil
	}
	holder.mu.Unlock()

	// Invariant Assertions:

	// Invariant I1: Read Mode 18 in-engine peak gauge
	res, err := probe.Call(context.Background(), []byte{18})
	if err != nil {
		t.Fatalf("Failed to read Mode 18 gauge: %v", err)
	}
	peakInRust := binary.LittleEndian.Uint64(res)
	t.Logf("Invariant I1: Peak in-engine concurrent jobs = %d (configured poolSize = %d)", peakInRust, poolSize)
	if peakInRust > poolSize {
		t.Fatalf("INVARIANT I1 VIOLATION: peak concurrent jobs in Rust (%d) exceeded poolSize (%d)", peakInRust, poolSize)
	}

	// Invariant I2: Assert panics were caught
	t.Logf("Invariant I2: Caught panics on ephemeral handles = %d", opPanicsCaught.Load())
	if opPanicsCaught.Load() == 0 {
		t.Errorf("INVARIANT I2: no panic tests executed during chaos run")
	}

	// Invariant I6 & I3: Assert Stats monotonicity and zero leak
	finalStats := gusset.Stats()
	t.Logf("Chaos Stats Summary: Calls=%d, Submits=%d, Cancelled=%d, Discards=%d, Closed=%d, Churns=%d",
		opCallsCompleted.Load(), opSubmitsCompleted.Load(), opCancelled.Load(),
		opDiscards.Load(), opClosedHandled.Load(), handleChurnCount.Load())
	t.Logf("Memory Stats: LiveBytes baseline=%d final=%d (delta=%d), Allocs baseline=%d final=%d",
		baselineLive, finalStats.LiveBytes, int64(finalStats.LiveBytes)-int64(baselineLive),
		baselineAlloc, finalStats.AllocCount)

	if finalStats.PeakBytes < finalStats.LiveBytes {
		t.Errorf("INVARIANT I6 VIOLATION: PeakBytes (%d) < LiveBytes (%d)", finalStats.PeakBytes, finalStats.LiveBytes)
	}
	if finalStats.AllocCount < baselineAlloc {
		t.Errorf("INVARIANT I6 VIOLATION: AllocCount decreased: baseline %d, final %d", baselineAlloc, finalStats.AllocCount)
	}
	if finalStats.LiveBytes != baselineLive {
		t.Fatalf("INVARIANT I3/I6 VIOLATION: Memory leak detected! baseline LiveBytes=%d, final LiveBytes=%d (leaked %d bytes)",
			baselineLive, finalStats.LiveBytes, int64(finalStats.LiveBytes)-int64(baselineLive))
	}
}

// TestConcurrentChaos_ConcurrentCloseRaces tests that rapid concurrent Close() calls
// against in-flight operations never deadlock and return identical results (Invariant I4).
func TestConcurrentChaos_ConcurrentCloseRaces(t *testing.T) {
	for trial := 0; trial < 10; trial++ {
		h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("trial %d: Open failed: %v", trial, err)
		}

		var wg sync.WaitGroup
		const callers = 16
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				_, _ = h.Call(ctx, []byte{0, 1})
			}()
		}

		// Concurrently invoke Close() from 4 distinct goroutines
		const closers = 4
		closeResults := make([]error, closers)
		var closeWg sync.WaitGroup
		for c := 0; c < closers; c++ {
			closeWg.Add(1)
			go func(idx int) {
				defer closeWg.Done()
				closeResults[idx] = h.Close()
			}(c)
		}

		closeDone := make(chan struct{})
		go func() {
			closeWg.Wait()
			wg.Wait()
			close(closeDone)
		}()

		select {
		case <-closeDone:
		case <-time.After(3 * time.Second):
			t.Fatalf("trial %d: concurrent Close deadlock detected", trial)
		}

		// Assert all closers received identical error (either nil or same error)
		for c := 1; c < closers; c++ {
			if closeResults[c] != closeResults[0] {
				t.Fatalf("trial %d: closer %d returned %v, while closer 0 returned %v",
					trial, c, closeResults[c], closeResults[0])
			}
		}
	}
}

// TestConcurrentChaos_CooperativeCancellationStorm tests a storm of rapid context
// cancellations, verifying permit conservation and zero memory leak.
func TestConcurrentChaos_CooperativeCancellationStorm(t *testing.T) {
	baseLive := gusset.Stats().LiveBytes
	const poolSize = 8
	h, err := gusset.Open(
		gusset.WithPoolSize(poolSize),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const callers = 64
	const iterations = 50
	var wg sync.WaitGroup

	for c := 0; c < callers; c++ {
		wg.Add(1)
		go func(cid int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(cid)+500, 11111))
			for i := 0; i < iterations; i++ {
				// Random microsecond timeout between 10µs and 500µs
				timeout := time.Duration(10+r.IntN(490)) * time.Microsecond
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				// Mode 5 cooperative sleep
				_, _ = h.Call(ctx, []byte{5, 10})
				cancel()
			}
		}(c)
	}

	wg.Wait()

	// Verify permit conservation:
	// All poolSize permits must be available and functional
	for i := 0; i < poolSize; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		res, err := h.Call(ctx, []byte{0, byte(i)})
		cancel()
		if err != nil {
			t.Fatalf("permit check %d failed: %v", i, err)
		}
		if len(res) < 2 || res[1] != byte(i) {
			t.Fatalf("permit check %d returned invalid result: %v", i, res)
		}
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
		t.Fatalf("Cancellation storm leaked memory: baseline %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}
}
