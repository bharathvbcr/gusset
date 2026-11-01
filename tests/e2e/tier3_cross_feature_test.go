package e2e_test

import (
	"context"
	"encoding/binary"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestTier3_ConcurrentBuffers_AsyncSubmit_Cancellation_Budget tests concurrent buffer allocation,
// asynchronous submission, random context cancellation, and buffer budget reclamation.
func TestTier3_ConcurrentBuffers_AsyncSubmit_Cancellation_Budget(t *testing.T) {
	const budget = 4 * 1024 * 1024 // 4 MiB budget
	const poolSize = 8

	h, err := gusset.Open(
		gusset.WithPoolSize(poolSize),
		gusset.WithDiagnosticEngine(),
		gusset.WithBufferBudget(budget),
	)
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	baseStats := gusset.Stats()

	const numWorkers = 16
	const opsPerWorker = 25
	var completedOps atomic.Int64
	var cancelledOps atomic.Int64
	var budgetExhaustedOps atomic.Int64

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID * 1000)))

			for i := 0; i < opsPerWorker; i++ {
				// Allocate buffer between 32 KiB and 256 KiB
				bufSize := (32 + rng.Intn(225)) * 1024
				buf, err := h.NewBuffer(bufSize)
				if err != nil {
					if errors.Is(err, gusset.ErrBufferBudget) {
						budgetExhaustedOps.Add(1)
						time.Sleep(2 * time.Millisecond)
						continue
					}
					t.Errorf("worker %d: unexpected NewBuffer error: %v", workerID, err)
					return
				}

				if !is64ByteAligned(buf.Bytes()) {
					t.Errorf("worker %d: buffer violates 64-byte alignment", workerID)
					buf.Free()
					return
				}

				// Populate payload: mode 0 echo
				data := buf.Bytes()
				data[0] = 0
				data[len(data)-1] = byte(workerID)

				// 25% of jobs have a short deadline to trigger cancellation
				var ctx context.Context
				var cancel context.CancelFunc
				if rng.Float32() < 0.25 {
					ctx, cancel = context.WithTimeout(context.Background(), 1*time.Millisecond)
				} else {
					ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
				}

				ticket, err := h.Submit(ctx, buf)
				if err != nil {
					cancel()
					buf.Free()
					if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
						cancelledOps.Add(1)
						continue
					}
					t.Errorf("worker %d: Submit failed: %v", workerID, err)
					return
				}

				resBuf, err := h.WaitBuffer(ctx, ticket)
				cancel()
				buf.Free()

				if err != nil {
					if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
						cancelledOps.Add(1)
					} else {
						t.Errorf("worker %d: WaitBuffer failed: %v", workerID, err)
					}
				} else {
					completedOps.Add(1)
					resData := resBuf.Bytes()
					if len(resData) != bufSize || resData[len(resData)-1] != byte(workerID) {
						t.Errorf("worker %d: data mismatch", workerID)
					}
					resBuf.Free()
				}
			}
		}(w)
	}
	wg.Wait()

	t.Logf("Tier 3 stress complete: completed=%d, cancelled=%d, budget_exhausted=%d",
		completedOps.Load(), cancelledOps.Load(), budgetExhaustedOps.Load())

	// Settle and verify memory returns to baseline
	time.Sleep(50 * time.Millisecond)
	finalStats := gusset.Stats()
	if finalStats.LiveBytes != baseStats.LiveBytes {
		t.Errorf("Stats().LiveBytes leaked memory: baseline=%d, final=%d",
			baseStats.LiveBytes, finalStats.LiveBytes)
	}
}

// TestTier3_MultiHandle_ConcurrentChaos runs multiple handles concurrently, with one handle
// repeatedly catching panics, poisoning, and being cleanly replaced, while others serve traffic.
func TestTier3_MultiHandle_ConcurrentChaos(t *testing.T) {
	const numLanes = 3
	handles := make([]*gusset.Handle, numLanes)
	for i := range handles {
		h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
		requireNoError(t, err, "gusset.Open lane")
		defer h.Close()
		handles[i] = h
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	var wg sync.WaitGroup

	// Lane 0: Steady echo traffic
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			payload := []byte{0, byte(i), byte(i * 2)}
			res, err := handles[0].Call(ctx, payload)
			if err != nil {
				t.Errorf("Lane 0 failed: %v", err)
				return
			}
			assertBytesEqual(t, res, payload, "Lane 0 echo")
		}
	}()

	// Lane 1: Vector compute (mode 10)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			data := make([]byte, 100)
			for j := range data {
				data[j] = byte(j % 50)
			}
			payload := append([]byte{10}, data...)
			res, err := handles[1].Call(ctx, payload)
			if err != nil {
				t.Errorf("Lane 1 failed: %v", err)
				return
			}
			got := binary.LittleEndian.Uint64(res)
			want := computeVectorSumSquaresOracle(data)
			if got != want {
				t.Errorf("Lane 1 compute mismatch: got %d, want %d", got, want)
				return
			}
		}
	}()

	// Lane 2: Chaos handle — induces panic, verifies poisoning, closes, and recreates
	wg.Add(1)
	go func() {
		defer wg.Done()
		for cycle := 0; cycle < 5; cycle++ {
			ch, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
			if err != nil {
				t.Errorf("Chaos lane Open failed: %v", err)
				return
			}

			// Healthy call
			_, err = ch.Call(ctx, []byte{0, 1})
			if err != nil {
				t.Errorf("Chaos lane pre-panic call failed: %v", err)
				ch.Close()
				return
			}

			// Induce panic
			_, err = ch.Call(ctx, []byte{1})
			if !errors.Is(err, gusset.ErrPanic) {
				t.Errorf("Chaos lane expected ErrPanic, got: %v", err)
			}

			// Verify poisoned
			_, err = ch.Call(ctx, []byte{0, 2})
			if !errors.Is(err, gusset.ErrPoisoned) {
				t.Errorf("Chaos lane expected ErrPoisoned, got: %v", err)
			}

			// Clean close
			ch.Close()
		}
	}()

	wg.Wait()
}

// TestTier3_LargeBufferRoundtrip_HighConcurrency tests concurrent transfer of 1 MiB to 2 MiB
// buffers across multiple goroutines, verifying data fidelity and 64-byte alignment.
func TestTier3_LargeBufferRoundtrip_HighConcurrency(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const numCallers = 6
	const bufferSize = 1024 * 1024 // 1 MiB each

	var wg sync.WaitGroup
	for c := 0; c < numCallers; c++ {
		wg.Add(1)
		go func(callerID int) {
			defer wg.Done()
			buf, err := h.NewBuffer(bufferSize)
			if err != nil {
				t.Errorf("caller %d: NewBuffer failed: %v", callerID, err)
				return
			}
			defer buf.Free()

			bytes := buf.Bytes()
			if !is64ByteAligned(bytes) {
				t.Errorf("caller %d: buffer is not 64-byte aligned", callerID)
				return
			}

			// Patterned data with caller stamp
			seed := byte(callerID * 37)
			for i := 0; i < bufferSize; i++ {
				bytes[i] = byte(i) ^ seed
			}
			bytes[0] = 0 // mode 0: echo

			ticket, err := h.Submit(ctx, buf)
			if err != nil {
				t.Errorf("caller %d: Submit failed: %v", callerID, err)
				return
			}

			resBuf, err := h.WaitBuffer(ctx, ticket)
			if err != nil {
				t.Errorf("caller %d: WaitBuffer failed: %v", callerID, err)
				return
			}
			defer resBuf.Free()

			resBytes := resBuf.Bytes()
			if !is64ByteAligned(resBytes) {
				t.Errorf("caller %d: resBuf is not 64-byte aligned", callerID)
				return
			}

			assertBytesEqual(t, resBytes, bytes, "large buffer echo")
		}(c)
	}
	wg.Wait()
}

// TestTier3_BurstSubmission_And_AbandonmentStorm verifies semaphore permit conservation (Invariant I4)
// when an entire wave of asynchronous tickets is abandoned due to caller timeouts.
func TestTier3_BurstSubmission_And_AbandonmentStorm(t *testing.T) {
	const poolSize = 4
	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	// Submit 32 jobs with 1ms timeout where jobs sleep for 20ms (mode 5: 2 iterations of 10ms)
	const burstSize = 32
	var wg sync.WaitGroup

	for i := 0; i < burstSize; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
			defer cancel()

			payload := []byte{5, 2} // sleeps 20ms
			_, err := h.Call(ctx, payload)
			if err == nil {
				// Rare chance it finished before 2ms, but typically times out
				return
			}
			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				t.Errorf("expected DeadlineExceeded/Canceled, got: %v", err)
			}
		}()
	}
	wg.Wait()

	// Wait for workers in Rust pool to finish their 20ms sleep and return permits
	time.Sleep(100 * time.Millisecond)

	// Invariant I4 verification: pool permits must be fully restored.
	// Submit exactly poolSize jobs; if any permits leaked, this will stall or fail.
	ctxSettle, cancelSettle := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelSettle()

	for i := 0; i < poolSize; i++ {
		res, err := h.Call(ctxSettle, []byte{0, byte(i)})
		requireNoError(t, err, "settlement Call after abandonment storm")
		assertBytesEqual(t, res, []byte{0, byte(i)}, "settlement echo")
	}
}
