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

// TestChallenge_M3_2_BufferBudget_ExtremeOversubscriptionAndDrift tests 128 concurrent
// goroutines allocating odd and prime sizes against a tight 128 KiB budget, verifying
// heavy oversubscription rejection, exact zero budget drift, and zero LiveBytes leak.
func TestChallenge_M3_2_BufferBudget_ExtremeOversubscriptionAndDrift(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	baseLive := gusset.Stats().LiveBytes
	const budget = 128 * 1024 // 128 KiB tight budget

	h, err := gusset.Open(
		gusset.WithBufferBudget(budget),
		gusset.WithPoolSize(4),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const callers = 128
	const iterations = 50

	var (
		refusals        atomic.Int64
		successes       atomic.Int64
		overBudgetCalls atomic.Int64
		wg              sync.WaitGroup
	)

	// Odd and prime sizes to stress alignment arithmetic and budget accounting
	primeSizes := []int{1, 3, 7, 13, 17, 31, 67, 127, 257, 1021, 2053, 4093, 8191, 16381, 32771}

	startCh := make(chan struct{})

	for c := 0; c < callers; c++ {
		wg.Add(1)
		go func(cid int) {
			defer wg.Done()
			<-startCh

			r := rand.New(rand.NewPCG(uint64(cid)+2026, 99999))
			var held []*gusset.Buffer

			for i := 0; i < iterations; i++ {
				// 10% chance of attempting allocation exceeding full budget (instant refusal)
				var sz int
				if r.IntN(10) == 0 {
					sz = budget + 1 + r.IntN(1024)
					overBudgetCalls.Add(1)
				} else {
					sz = primeSizes[r.IntN(len(primeSizes))]
				}

				buf, err := h.NewBuffer(sz)
				if err != nil {
					if errors.Is(err, gusset.ErrBufferBudget) {
						refusals.Add(1)
						// Free oldest buffer if held to churn budget
						if len(held) > 0 {
							_ = held[0].Free()
							held = held[1:]
						}
						continue
					}
					t.Errorf("caller %d unexpected NewBuffer(%d) error: %v", cid, sz, err)
					return
				}

				successes.Add(1)
				slice := buf.Bytes()
				if len(slice) != sz {
					t.Errorf("caller %d size mismatch: got %d, expected %d", cid, len(slice), sz)
				}
				if !isAligned64(slice) {
					t.Errorf("caller %d buffer unaligned for size %d", cid, sz)
				}
				if len(slice) > 0 {
					slice[0] = byte(cid)
					slice[len(slice)-1] = byte(i)
				}
				held = append(held, buf)

				// Keep at most 2 buffers concurrently per worker
				if len(held) > 2 {
					_ = held[0].Free()
					held = held[1:]
				}
			}

			// Clean up remaining held buffers
			for _, b := range held {
				_ = b.Free()
			}
		}(c)
	}

	close(startCh)
	wg.Wait()

	t.Logf("ExtremeOversubscription: successes=%d, refusals=%d, overBudgetAttempts=%d",
		successes.Load(), refusals.Load(), overBudgetCalls.Load())

	if refusals.Load() == 0 {
		t.Fatalf("expected substantial budget refusals under 128 callers, got 0")
	}

	// Canonical Zero Drift & Boundary Clamp Verification:
	// 1. After freeing all buffers, exact budget allocation must succeed
	exactBuf, err := h.NewBuffer(budget)
	if err != nil {
		t.Fatalf("exact budget allocation failed after churn: %v", err)
	}
	_ = exactBuf.Free()

	// 2. Budget + 1 must fail with ErrBufferBudget
	overBuf, err := h.NewBuffer(budget + 1)
	if !errors.Is(err, gusset.ErrBufferBudget) {
		if overBuf != nil {
			_ = overBuf.Free()
		}
		t.Fatalf("expected NewBuffer(budget+1) to fail with ErrBufferBudget, got: %v", err)
	}

	// 3. Exact budget must succeed again
	exactBuf2, err := h.NewBuffer(budget)
	if err != nil {
		t.Fatalf("second exact budget allocation failed: %v", err)
	}
	_ = exactBuf2.Free()

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
		t.Fatalf("LiveBytes leak: baseline %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}
}

// TestChallenge_M3_2_BufferBudget_ConcurrentFreeBytesRace tests 200 buffers under
// simultaneous Free(), Bytes(), and ID() calls from 32 goroutines per buffer, asserting
// strict idempotency, zero data races, no use-after-free, and exact budget reclamation.
func TestChallenge_M3_2_BufferBudget_ConcurrentFreeBytesRace(t *testing.T) {
	baseLive := gusset.Stats().LiveBytes
	const budget = 512 * 1024 // 512 KiB
	h, err := gusset.Open(
		gusset.WithBufferBudget(budget),
		gusset.WithPoolSize(2),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const numBuffers = 200
	const closersPerBuffer = 16
	const readersPerBuffer = 16

	buffers := make([]*gusset.Buffer, numBuffers)
	for i := 0; i < numBuffers; i++ {
		sz := 1024 + (i*13)%2048
		buf, err := h.NewBuffer(sz)
		if err != nil {
			t.Fatalf("NewBuffer %d failed: %v", i, err)
		}
		b := buf.Bytes()
		if len(b) > 0 {
			b[0] = 0x55
			b[len(b)-1] = 0xAA
		}
		buffers[i] = buf
	}

	var wg sync.WaitGroup
	startRace := make(chan struct{})

	for _, buf := range buffers {
		// Launch Free() racers
		for c := 0; c < closersPerBuffer; c++ {
			wg.Add(1)
			go func(b *gusset.Buffer) {
				defer wg.Done()
				<-startRace
				if err := b.Free(); err != nil {
					t.Errorf("Free returned error: %v", err)
				}
			}(buf)
		}

		// Launch Bytes() and ID() concurrent readers
		for r := 0; r < readersPerBuffer; r++ {
			wg.Add(1)
			go func(b *gusset.Buffer) {
				defer wg.Done()
				<-startRace
				slice := b.Bytes()
				id := b.ID()
				// If slice is non-nil, it must be valid and intact; if nil, buffer was freed
				if slice != nil {
					_ = slice[0]
				}
				_ = id
			}(buf)
		}
	}

	close(startRace)
	wg.Wait()

	// All buffers are now freed; check that all report nil Bytes() and 0 ID()
	for i, b := range buffers {
		if b.Bytes() != nil {
			t.Errorf("buffer %d Bytes() != nil after concurrent Free", i)
		}
		if b.ID() != 0 {
			t.Errorf("buffer %d ID() != 0 after concurrent Free", i)
		}
	}

	// Verify budget is fully intact:
	exactBuf, err := h.NewBuffer(budget)
	if err != nil {
		t.Fatalf("exact budget NewBuffer failed: %v", err)
	}
	_ = exactBuf.Free()

	overBuf, err := h.NewBuffer(budget + 1)
	if !errors.Is(err, gusset.ErrBufferBudget) {
		if overBuf != nil {
			_ = overBuf.Free()
		}
		t.Fatalf("expected ErrBufferBudget for budget+1, got: %v", err)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
		t.Fatalf("LiveBytes leak detected: baseline %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}
}

// TestChallenge_M3_2_BufferBudget_BoundaryConfigurations tests negative, zero (unlimited),
// and 1-byte minimal buffer budgets, asserting API contracts and refusal behavior.
func TestChallenge_M3_2_BufferBudget_BoundaryConfigurations(t *testing.T) {
	// 1. Negative budget must be refused by Open with ErrBadArg
	badBudgets := []int64{-1, -100, -1024, -1 << 30}
	for _, b := range badBudgets {
		h, err := gusset.Open(gusset.WithBufferBudget(b))
		if !errors.Is(err, gusset.ErrBadArg) {
			if h != nil {
				_ = h.Close()
			}
			t.Errorf("expected WithBufferBudget(%d) to fail with ErrBadArg, got err=%v", b, err)
		}
	}

	// 2. Budget = 0 means unlimited: can allocate large buffers without ErrBufferBudget
	hZero, err := gusset.Open(gusset.WithBufferBudget(0), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open with budget 0 failed: %v", err)
	}
	b1, err := hZero.NewBuffer(16 * 1024 * 1024) // 16 MiB
	if err != nil {
		t.Fatalf("NewBuffer on unlimited budget failed: %v", err)
	}
	_ = b1.Free()
	_ = hZero.Close()

	// 3. Minimal 1-byte budget: exactly 1 byte succeeds; 2 bytes fails
	hOne, err := gusset.Open(gusset.WithBufferBudget(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open with budget 1 failed: %v", err)
	}
	buf1, err := hOne.NewBuffer(1)
	if err != nil {
		t.Fatalf("NewBuffer(1) on 1-byte budget failed: %v", err)
	}
	// Second allocation must fail with ErrBufferBudget
	bufOver, err := hOne.NewBuffer(1)
	if !errors.Is(err, gusset.ErrBufferBudget) {
		if bufOver != nil {
			_ = bufOver.Free()
		}
		t.Fatalf("expected ErrBufferBudget for 2nd byte, got: %v", err)
	}
	_ = buf1.Free()

	// After freeing, 1 byte succeeds again
	buf1Again, err := hOne.NewBuffer(1)
	if err != nil {
		t.Fatalf("NewBuffer(1) failed after free: %v", err)
	}
	_ = buf1Again.Free()
	_ = hOne.Close()
}

// TestChallenge_M3_2_ConcurrentChaos_TightBulkhead_Mode18 verifies Invariant I1
// on a tight pool of 2 workers under high-concurrency chaos swarm with non-cooperative
// engine holds (Mode 17), take buffers (Mode 16), panics (Mode 1 & 15), and cancellations.
func TestChallenge_M3_2_ConcurrentChaos_TightBulkhead_Mode18(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	baselineLive := gusset.Stats().LiveBytes
	baselineAlloc := gusset.Stats().AllocCount

	const (
		poolSize      = 2 // Tight pool: Invariant I1 must bound peak in-engine concurrency to <= 2
		callers       = 32
		chaosDuration = 2 * time.Second
	)

	// Probe handle to reset and verify Mode 17/18 in-engine peak gauge
	probe, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open probe failed: %v", err)
	}
	defer probe.Close()

	// Reset gauge peak
	_, _ = probe.Call(context.Background(), []byte{18})

	h, err := gusset.Open(
		gusset.WithPoolSize(poolSize),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open poolSize=2 failed: %v", err)
	}

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	var (
		mode17Submits atomic.Int64
		mode16Submits atomic.Int64
		echoCalls     atomic.Int64
		cancelled     atomic.Int64
	)

	for c := 0; c < callers; c++ {
		wg.Add(1)
		go func(cid int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(cid)+777, 88888))

			for {
				select {
				case <-stopCh:
					return
				default:
				}

				op := r.IntN(5)
				switch op {
				case 0:
					// Mode 17: Non-cooperative engine hold of 4ms (Invariant I1 stress)
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					tk, err := h.Submit(ctx, []byte{17, 4})
					cancel()
					if err == nil {
						mode17Submits.Add(1)
						wCtx, wCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
						_, _ = h.Wait(wCtx, tk)
						wCancel()
					} else {
						cancelled.Add(1)
					}

				case 1:
					// Mode 16: Take buffer (8192 bytes)
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					payload := make([]byte, 6)
					payload[0] = 16
					binary.LittleEndian.PutUint32(payload[1:5], 8192)
					payload[5] = byte(cid)
					tk, err := h.Submit(ctx, payload)
					cancel()
					if err == nil {
						mode16Submits.Add(1)
						wCtx, wCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
						buf, werr := h.WaitBuffer(wCtx, tk)
						wCancel()
						if werr == nil && buf != nil {
							_ = buf.Free()
						}
					}

				case 2:
					// Normal synchronous echo
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					_, err := h.Call(ctx, []byte{0, byte(cid)})
					cancel()
					if err == nil {
						echoCalls.Add(1)
					}

				case 3:
					// Rapid microsecond cancellation
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Microsecond)
					_, _ = h.Call(ctx, []byte{5, 20})
					cancel()
					cancelled.Add(1)

				case 4:
					// Ephemeral handle panic containment test (Invariant I2)
					pHandle, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
					if err == nil {
						pCtx, pCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
						_, _ = pHandle.Call(pCtx, []byte{15, 3}) // Mode 15: DropBomb double panic
						pCancel()
						_ = pHandle.Close()
					}
				}
			}
		}(c)
	}

	time.Sleep(chaosDuration)
	close(stopCh)
	wg.Wait()

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Read Invariant I1 in-engine peak gauge
	res, err := probe.Call(context.Background(), []byte{18})
	if err != nil {
		t.Fatalf("Failed to read Mode 18 gauge: %v", err)
	}
	peakInRust := binary.LittleEndian.Uint64(res)
	t.Logf("TightBulkhead Mode 18 Peak: %d (configured poolSize=%d). Ops: Mode17=%d, Mode16=%d, Echo=%d, Cancelled=%d",
		peakInRust, poolSize, mode17Submits.Load(), mode16Submits.Load(), echoCalls.Load(), cancelled.Load())

	if peakInRust > poolSize {
		t.Fatalf("INVARIANT I1 VIOLATION: peak in-engine jobs (%d) exceeded poolSize (%d)", peakInRust, poolSize)
	}

	// Invariant I6 & I3 Assertions: Stats Monotonicity and Zero Leaks
	finalStats := gusset.Stats()
	if finalStats.PeakBytes < finalStats.LiveBytes {
		t.Errorf("INVARIANT I6: PeakBytes (%d) < LiveBytes (%d)", finalStats.PeakBytes, finalStats.LiveBytes)
	}
	if finalStats.AllocCount < baselineAlloc {
		t.Errorf("INVARIANT I6: AllocCount decreased: %d < %d", finalStats.AllocCount, baselineAlloc)
	}
	if finalStats.LiveBytes != baselineLive {
		t.Fatalf("INVARIANT I3/I6: LiveBytes leak detected! baseline=%d, final=%d (delta=%d)",
			baselineLive, finalStats.LiveBytes, int64(finalStats.LiveBytes)-int64(baselineLive))
	}
}

// TestChallenge_M3_2_ViolentClose_ActiveBufferReclamation tests closing a handle while
// 32 goroutines are allocating buffers, asserting that surviving buffers can be safely
// freed or garbage-collected without crashes or memory leaks.
func TestChallenge_M3_2_ViolentClose_ActiveBufferReclamation(t *testing.T) {
	baseLive := gusset.Stats().LiveBytes
	const budget = 1024 * 1024 // 1 MiB

	h, err := gusset.Open(
		gusset.WithBufferBudget(budget),
		gusset.WithPoolSize(4),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const callers = 32
	var wg sync.WaitGroup
	var survivingMu sync.Mutex
	var surviving []*gusset.Buffer

	startCh := make(chan struct{})

	for c := 0; c < callers; c++ {
		wg.Add(1)
		go func(cid int) {
			defer wg.Done()
			<-startCh

			for i := 0; i < 30; i++ {
				buf, err := h.NewBuffer(2048)
				if err != nil {
					return
				}
				slice := buf.Bytes()
				if len(slice) > 0 {
					slice[0] = byte(cid)
				}

				if i%5 == 0 {
					// Retain some buffers across Close()
					survivingMu.Lock()
					surviving = append(surviving, buf)
					survivingMu.Unlock()
				} else {
					_ = buf.Free()
				}
			}
		}(c)
	}

	close(startCh)
	time.Sleep(3 * time.Millisecond)

	// Violently Close handle while workers allocate
	_ = h.Close()
	wg.Wait()

	t.Logf("ViolentClose: %d buffers survived Close()", len(surviving))

	// Free all surviving buffers post-Close
	for _, b := range surviving {
		// Calling Bytes() on post-close buffer must return nil safely
		if b.Bytes() != nil {
			t.Errorf("post-close buffer returned non-nil Bytes()")
		}
		// Calling Free() on post-close buffer must succeed safely
		if err := b.Free(); err != nil {
			t.Errorf("post-close Free() returned error: %v", err)
		}
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
		t.Fatalf("LiveBytes leak after violent close: baseline %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}
}

// TestChallenge_M3_2_CallBuffer_WithBudgetAndConcurrentFree tests CallBuffer using
// caller-allocated buffers under WithBufferBudget, testing concurrent Free() races
// and boundary clamp verification.
func TestChallenge_M3_2_CallBuffer_WithBudgetAndConcurrentFree(t *testing.T) {
	baseLive := gusset.Stats().LiveBytes
	const budget = 256 * 1024 // 256 KiB
	h, err := gusset.Open(
		gusset.WithBufferBudget(budget),
		gusset.WithPoolSize(4),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const callers = 32
	var wg sync.WaitGroup

	for c := 0; c < callers; c++ {
		wg.Add(1)
		go func(cid int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				inBuf, err := h.NewBuffer(4096)
				if err != nil {
					return
				}
				slice := inBuf.Bytes()
				if len(slice) > 0 {
					slice[0] = 0 // Mode 0 Echo
					slice[1] = byte(cid)
				}

				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				outBuf, err := h.CallBuffer(ctx, inBuf)
				cancel()

				_ = inBuf.Free()

				if err == nil && outBuf != nil {
					outSlice := outBuf.Bytes()
					if len(outSlice) >= 2 && outSlice[1] != byte(cid) {
						t.Errorf("caller %d result mismatch: %v", cid, outSlice[:2])
					}
					_ = outBuf.Free()
				}
			}
		}(c)
	}

	wg.Wait()

	// Verify exact budget clamp post CallBuffer churn
	exactBuf, err := h.NewBuffer(budget)
	if err != nil {
		t.Fatalf("exact NewBuffer failed after CallBuffer churn: %v", err)
	}
	_ = exactBuf.Free()

	overBuf, err := h.NewBuffer(budget + 1)
	if !errors.Is(err, gusset.ErrBufferBudget) {
		if overBuf != nil {
			_ = overBuf.Free()
		}
		t.Fatalf("expected ErrBufferBudget for budget+1, got: %v", err)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
		t.Fatalf("LiveBytes leak: baseline %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}
}

// TestChallenge_M3_2_BufferBudget_MultiPoolIsolation tests 8 concurrent handles each
// with distinct buffer budgets, verifying that budget exhaustion and handle closures
// on one handle have zero side-effects on sibling handles.
func TestChallenge_M3_2_BufferBudget_MultiPoolIsolation(t *testing.T) {
	baseLive := gusset.Stats().LiveBytes
	const numHandles = 8
	handles := make([]*gusset.Handle, numHandles)
	budgets := make([]int64, numHandles)

	for i := 0; i < numHandles; i++ {
		budgets[i] = int64((i + 1) * 64 * 1024) // 64 KiB to 512 KiB
		h, err := gusset.Open(
			gusset.WithBufferBudget(budgets[i]),
			gusset.WithPoolSize(2),
			gusset.WithDiagnosticEngine(),
		)
		if err != nil {
			t.Fatalf("Open handle %d failed: %v", i, err)
		}
		handles[i] = h
	}

	var wg sync.WaitGroup
	// Concurrently saturate each handle with allocations
	for i := 0; i < numHandles; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			h := handles[idx]
			b := budgets[idx]

			// Exhaust budget
			buf, err := h.NewBuffer(int(b))
			if err != nil {
				t.Errorf("handle %d: NewBuffer(%d) failed: %v", idx, b, err)
				return
			}

			// Verify budget is strictly exhausted
			over, err := h.NewBuffer(1)
			if !errors.Is(err, gusset.ErrBufferBudget) {
				if over != nil {
					_ = over.Free()
				}
				t.Errorf("handle %d: expected ErrBufferBudget, got: %v", idx, err)
			}

			// Free and verify exact reclamation
			_ = buf.Free()

			exact, err := h.NewBuffer(int(b))
			if err != nil {
				t.Errorf("handle %d: exact NewBuffer failed: %v", idx, err)
				return
			}
			_ = exact.Free()
		}(i)
	}

	wg.Wait()

	// Clean up all handles
	for i, h := range handles {
		if err := h.Close(); err != nil {
			t.Errorf("handle %d Close failed: %v", i, err)
		}
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
		t.Fatalf("LiveBytes leak in MultiPoolIsolation: baseline %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}
}
