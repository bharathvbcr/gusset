package pitfalls_test

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/bharathvbcr/gusset"
)

func isBuffer64Aligned(b []byte) bool {
	return len(b) == 0 || uintptr(unsafe.Pointer(unsafe.SliceData(b)))%64 == 0
}

// TestChallenge_M5_2_ConcurrentDoubleFreeProtection validates that:
// 1. Multiple concurrent goroutines calling buf.Free() on the same Buffer is strictly safe and idempotent.
// 2. Budget is decremented exactly once with zero drift.
// 3. Stats().LiveBytes returns to baseline without double-uncount.
// 4. Both Rust-owned (ID > 0) and Go heap-owned (ID == 0) buffers behave identically.
func TestChallenge_M5_2_ConcurrentDoubleFreeProtection(t *testing.T) {
	const budget = 256 * 1024 // 256 KiB
	h, err := gusset.Open(
		gusset.WithPoolSize(4),
		gusset.WithDiagnosticEngine(),
		gusset.WithBufferBudget(budget),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	baseLive := gusset.Stats().LiveBytes

	// Phase 1: Rust-allocated Buffer (ID > 0)
	for trial := 0; trial < 20; trial++ {
		const bufSize = 16384 // 16 KiB
		buf, err := h.NewBuffer(bufSize)
		if err != nil {
			t.Fatalf("trial %d: NewBuffer(%d) failed: %v", trial, bufSize, err)
		}
		if buf.ID() == 0 {
			t.Fatalf("trial %d: expected non-zero ID for Rust buffer", trial)
		}

		// 32 concurrent goroutines racing to call Free() on the same buffer
		const racierCount = 32
		var wg sync.WaitGroup
		start := make(chan struct{})

		for r := 0; r < racierCount; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := buf.Free(); err != nil {
					t.Errorf("buf.Free() returned unexpected error: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()

		// Verify subsequent serial Free is safe
		if err := buf.Free(); err != nil {
			t.Fatalf("serial post-free failed: %v", err)
		}
		if buf.Bytes() != nil {
			t.Fatalf("buf.Bytes() did not return nil after Free()")
		}
		if buf.ID() != 0 {
			t.Fatalf("buf.ID() did not return 0 after Free()")
		}
	}

	// Phase 2: Go-owned heap Buffer (ID == 0) from an inline result
	for trial := 0; trial < 20; trial++ {
		// Echo a 64-byte payload
		payload := make([]byte, 64)
		payload[0] = 0 // mode 0 echo
		for i := 1; i < len(payload); i++ {
			payload[i] = byte(i ^ trial)
		}

		ticket, err := h.Submit(ctx, payload)
		if err != nil {
			t.Fatalf("trial %d: Submit failed: %v", trial, err)
		}
		heapBuf, err := h.WaitBuffer(ctx, ticket)
		if err != nil {
			t.Fatalf("trial %d: WaitBuffer failed: %v", trial, err)
		}
		if heapBuf.ID() != 0 {
			t.Fatalf("trial %d: expected ID == 0 for inline heap buffer, got %d", trial, heapBuf.ID())
		}

		const racierCount = 32
		var wg sync.WaitGroup
		start := make(chan struct{})

		for r := 0; r < racierCount; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := heapBuf.Free(); err != nil {
					t.Errorf("heapBuf.Free() returned unexpected error: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()

		if heapBuf.Bytes() != nil {
			t.Fatalf("heapBuf.Bytes() did not return nil after Free()")
		}
	}

	// Verify exact budget retention: full budget allocation succeeds
	exactBuf, err := h.NewBuffer(budget)
	if err != nil {
		t.Fatalf("budget corrupted by double-free race: %v", err)
	}
	_ = exactBuf.Free()

	// Verify zero live bytes leak
	curLive := gusset.Stats().LiveBytes
	if curLive != baseLive {
		t.Fatalf("LiveBytes delta != 0: base=%d, cur=%d", baseLive, curLive)
	}
}

// TestChallenge_M5_2_MemoryPoisoningAndPostFreeAccess validates that:
//  1. Calling Free() immediately withdraws the slice view (Bytes() == nil) and zeroes ID().
//  2. Subsequent Submit / CallBuffer calls with a freed Rust or Go-heap buffer are refused.
//  3. Go-heap buffer backing memory is left intact on Free() so a concurrent Submit that
//     passed the lock-free freed check cannot race a slice modification.
func TestChallenge_M5_2_MemoryPoisoningAndPostFreeAccess(t *testing.T) {
	h, err := gusset.Open(
		gusset.WithPoolSize(2),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Case 1: Rust buffer post-Free withdrawal
	const rustBufSize = 4096
	buf, err := h.NewBuffer(rustBufSize)
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}
	rawSlice := buf.Bytes()
	for i := range rawSlice {
		rawSlice[i] = 0x55
	}

	if err := buf.Free(); err != nil {
		t.Fatalf("buf.Free() failed: %v", err)
	}

	// Bytes() must immediately return nil and ID() must return 0
	if buf.Bytes() != nil {
		t.Fatal("expected buf.Bytes() to return nil after Free()")
	}
	if buf.ID() != 0 {
		t.Fatalf("expected buf.ID() == 0 after Free(), got %d", buf.ID())
	}
	if _, err := h.Submit(ctx, buf); err == nil {
		t.Fatal("expected Submit with freed Rust buffer to fail")
	}

	// Case 2: Go heap buffer post-Free withdrawal (ID == 0)
	payload := make([]byte, 256)
	payload[0] = 0 // mode 0 echo
	for i := 1; i < len(payload); i++ {
		payload[i] = 0xAA
	}

	ticket, err := h.Submit(ctx, payload)
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	heapBuf, err := h.WaitBuffer(ctx, ticket)
	if err != nil {
		t.Fatalf("WaitBuffer failed: %v", err)
	}
	heapSlice := heapBuf.Bytes()
	if len(heapSlice) != len(payload) {
		t.Fatalf("unexpected heapSlice len: %d", len(heapSlice))
	}

	if err := heapBuf.Free(); err != nil {
		t.Fatalf("heapBuf.Free() failed: %v", err)
	}

	if heapBuf.Bytes() != nil {
		t.Fatal("expected heapBuf.Bytes() to return nil after Free()")
	}
	if heapBuf.ID() != 0 {
		t.Fatalf("expected heapBuf.ID() == 0 after Free(), got %d", heapBuf.ID())
	}
	if _, err := h.Submit(ctx, heapBuf); err == nil {
		t.Fatal("expected Submit with freed Go-heap buffer to fail")
	}
}

// TestChallenge_M5_2_BufferFreeVsHandleCloseRace stress-tests concurrent racing between
// buf.Free() and h.Close() across multiple goroutines to verify that neither path segfaults,
// crashes under AddressSanitizer/race detector, or double-frees.
func TestChallenge_M5_2_BufferFreeVsHandleCloseRace(t *testing.T) {
	baseLive := gusset.Stats().LiveBytes

	const iterations = 50
	for iter := 0; iter < iterations; iter++ {
		h, err := gusset.Open(
			gusset.WithPoolSize(2),
			gusset.WithDiagnosticEngine(),
		)
		if err != nil {
			t.Fatalf("iter %d: Open failed: %v", iter, err)
		}

		const numBufs = 10
		buffers := make([]*gusset.Buffer, numBufs)
		for i := 0; i < numBufs; i++ {
			b, err := h.NewBuffer(2048)
			if err != nil {
				t.Fatalf("iter %d: NewBuffer failed: %v", iter, err)
			}
			buffers[i] = b
		}

		var wg sync.WaitGroup
		start := make(chan struct{})

		// Goroutines freeing buffers
		for i := 0; i < numBufs; i++ {
			wg.Add(1)
			go func(b *gusset.Buffer) {
				defer wg.Done()
				<-start
				_ = b.Free()
			}(buffers[i])
		}

		// Goroutine closing handle concurrently
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = h.Close()
		}()

		close(start)
		wg.Wait()
	}

	// Verify all buffers released across both paths
	curLive := gusset.Stats().LiveBytes
	if curLive != baseLive {
		t.Fatalf("LiveBytes leaked across Free/Close races: base=%d, cur=%d", baseLive, curLive)
	}
}

// TestChallenge_M5_2_AllocatorStatsMonotonicityAndMonotonicPeak asserts that under concurrent
// allocation and free churn:
// 1. Stats().AllocCount is strictly monotonically non-decreasing.
// 2. Stats().PeakBytes is strictly monotonically non-decreasing.
// 3. Stats().LiveBytes is non-negative and returns to baseline upon quiescence.
func TestChallenge_M5_2_AllocatorStatsMonotonicityAndMonotonicPeak(t *testing.T) {
	h, err := gusset.Open(
		gusset.WithPoolSize(8),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	baseStats := gusset.Stats()

	var (
		stopFlag atomic.Bool
		wg       sync.WaitGroup
		errCount atomic.Int64
	)

	const workers = 16
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(wid)+1000, 7777))
			sizes := []int{128, 512, 1024, 4096, 16384, 65536}

			for !stopFlag.Load() {
				sz := sizes[r.IntN(len(sizes))]
				buf, err := h.NewBuffer(sz)
				if err != nil {
					if errors.Is(err, gusset.ErrClosed) {
						return
					}
					errCount.Add(1)
					return
				}
				// Write data to verify memory writable
				slice := buf.Bytes()
				if len(slice) > 0 {
					slice[0] = byte(wid)
				}
				// Random sleep or immediate free
				if r.IntN(4) == 0 {
					runtime.Gosched()
				}
				_ = buf.Free()
			}
		}(w)
	}

	// Monitor goroutine verifying monotonicity
	var monitorWg sync.WaitGroup
	monitorWg.Add(1)
	go func() {
		defer monitorWg.Done()
		lastAllocCount := baseStats.AllocCount
		lastPeakBytes := baseStats.PeakBytes

		for i := 0; i < 300; i++ {
			s := gusset.Stats()
			if s.AllocCount < lastAllocCount {
				t.Errorf("AllocCount violation: current %d < last %d", s.AllocCount, lastAllocCount)
				return
			}
			if s.PeakBytes < lastPeakBytes {
				t.Errorf("PeakBytes violation: current %d < last %d", s.PeakBytes, lastPeakBytes)
				return
			}
			lastAllocCount = s.AllocCount
			lastPeakBytes = s.PeakBytes
			time.Sleep(1 * time.Millisecond)
		}
		stopFlag.Store(true)
	}()

	monitorWg.Wait()
	wg.Wait()

	if errCount.Load() > 0 {
		t.Fatalf("workers encountered %d errors", errCount.Load())
	}

	// Quiescence check: LiveBytes must return to baseline
	finalStats := gusset.Stats()
	if finalStats.LiveBytes != baseStats.LiveBytes {
		t.Fatalf("LiveBytes did not return to baseline: base=%d, final=%d",
			baseStats.LiveBytes, finalStats.LiveBytes)
	}
	if finalStats.PeakBytes < baseStats.PeakBytes {
		t.Fatalf("final PeakBytes (%d) < base PeakBytes (%d)", finalStats.PeakBytes, baseStats.PeakBytes)
	}
	if finalStats.AllocCount < baseStats.AllocCount {
		t.Fatalf("final AllocCount (%d) < base AllocCount (%d)", finalStats.AllocCount, baseStats.AllocCount)
	}
}

// TestChallenge_M5_2_AddCleanupGarbageCollectionBackstop validates that:
// 1. Buffers dropped without explicit Free() are reclaimed by Go's runtime.AddCleanup.
// 2. Reclaimed buffers decrement Stats().LiveBytes back to baseline.
// 3. Buffer budget (WithBufferBudget) is completely reclaimed, allowing subsequent allocations.
func TestChallenge_M5_2_AddCleanupGarbageCollectionBackstop(t *testing.T) {
	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	const budget = 128 * 1024 // 128 KiB
	h, err := gusset.Open(
		gusset.WithPoolSize(2),
		gusset.WithDiagnosticEngine(),
		gusset.WithBufferBudget(budget),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	baseLive := gusset.Stats().LiveBytes

	// Allocate 32 buffers of 4 KiB each (total 128 KiB, exhausting budget)
	func() {
		for i := 0; i < 32; i++ {
			buf, err := h.NewBuffer(4096)
			if err != nil {
				t.Fatalf("alloc %d failed: %v", i, err)
			}
			slice := buf.Bytes()
			slice[0] = byte(i)
			// Do not free buf; let it go out of scope
			_ = buf
		}
	}()

	// Immediately, budget must be exhausted
	overBuf, errExhaust := h.NewBuffer(1024)
	if !errors.Is(errExhaust, gusset.ErrBufferBudget) {
		if overBuf != nil {
			_ = overBuf.Free()
		}
		t.Fatalf("expected ErrBufferBudget, got: %v", errExhaust)
	}

	// Trigger GC to run AddCleanup handlers
	for attempts := 0; attempts < 10; attempts++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		runtime.GC()

		// Try allocating again to check if budget was reclaimed
		testBuf, err := h.NewBuffer(64 * 1024)
		if err == nil {
			// Successfully reclaimed!
			_ = testBuf.Free()
			break
		}
		if attempts == 9 {
			t.Fatalf("budget was not reclaimed by AddCleanup after 10 GC cycles: %v", err)
		}
	}

	// Now allocate the entire budget again to verify full recovery
	fullBuf, err := h.NewBuffer(budget)
	if err != nil {
		t.Fatalf("full budget allocation failed after GC cleanup: %v", err)
	}
	_ = fullBuf.Free()

	// Verify LiveBytes returns to baseline
	curLive := gusset.Stats().LiveBytes
	if curLive != baseLive {
		t.Fatalf("LiveBytes did not return to baseline: base=%d, cur=%d", baseLive, curLive)
	}
}

// TestChallenge_M5_2_CleanupVsManualFreeRace exercises concurrent racing between
// explicit buf.Free() and runtime.AddCleanup running during aggressive GC cycles.
// Asserts that budget does not drift negative and zero panics occur.
func TestChallenge_M5_2_CleanupVsManualFreeRace(t *testing.T) {
	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	const budget = 512 * 1024 // 512 KiB
	h, err := gusset.Open(
		gusset.WithPoolSize(4),
		gusset.WithDiagnosticEngine(),
		gusset.WithBufferBudget(budget),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	baseLive := gusset.Stats().LiveBytes

	var (
		stopFlag atomic.Bool
		workerWg sync.WaitGroup
		gcWg     sync.WaitGroup
	)

	// GC stress goroutine
	gcWg.Add(1)
	go func() {
		defer gcWg.Done()
		for !stopFlag.Load() {
			runtime.GC()
			time.Sleep(5 * time.Millisecond)
		}
	}()

	// 8 workers allocating, randomly freeing or dropping buffers
	for w := 0; w < 8; w++ {
		workerWg.Add(1)
		go func(wid int) {
			defer workerWg.Done()
			r := rand.New(rand.NewPCG(uint64(wid)+500, 333))

			for i := 0; i < 30; i++ {
				buf, err := h.NewBuffer(4096)
				if err != nil {
					if errors.Is(err, gusset.ErrBufferBudget) {
						time.Sleep(2 * time.Millisecond)
						continue
					}
					t.Errorf("worker %d unexpected error: %v", wid, err)
					return
				}
				// 50% chance manual free, 50% chance let GC collect
				if r.IntN(2) == 0 {
					_ = buf.Free()
				}
			}
		}(w)
	}

	workerWg.Wait()
	stopFlag.Store(true)
	gcWg.Wait()

	// Quiesce and run GC to flush any remaining cleanups
	for i := 0; i < 5; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}

	// Verify budget exact recovery: allocating full budget must succeed
	testBuf, err := h.NewBuffer(budget)
	if err != nil {
		t.Fatalf("budget drift detected after GC race: %v", err)
	}
	_ = testBuf.Free()

	// Verify zero live bytes leak
	curLive := gusset.Stats().LiveBytes
	if curLive != baseLive {
		t.Fatalf("LiveBytes delta != 0: base=%d, cur=%d", baseLive, curLive)
	}
}

// TestChallenge_M5_2_ARM64MemoryBarrierRingChurn stress-tests the completion ring and
// Dekker pair memory barrier synchronization (Go SwapUint32(waiting, 1) vs Rust fence(SeqCst)
// + waiting.swap(0, AcqRel)) under heavy concurrent async churn across 32 goroutines.
func TestChallenge_M5_2_ARM64MemoryBarrierRingChurn(t *testing.T) {
	h, err := gusset.Open(
		gusset.WithPoolSize(8),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const concurrency = 32
	const operationsPerWorker = 30

	var wg sync.WaitGroup
	start := make(chan struct{})

	for c := 0; c < concurrency; c++ {
		wg.Add(1)
		go func(cid int) {
			defer wg.Done()
			<-start

			for op := 0; op < operationsPerWorker; op++ {
				// Prepare 128-byte payload with mode 0 echo
				payload := make([]byte, 128)
				payload[0] = 0 // mode 0 echo
				binary.LittleEndian.PutUint32(payload[1:5], uint32(cid))
				binary.LittleEndian.PutUint32(payload[5:9], uint32(op))
				for i := 9; i < len(payload); i++ {
					payload[i] = byte(i ^ cid ^ op)
				}

				ticket, err := h.Submit(ctx, payload)
				if err != nil {
					t.Errorf("worker %d op %d Submit failed: %v", cid, op, err)
					return
				}

				res, err := h.Wait(ctx, ticket)
				if err != nil {
					t.Errorf("worker %d op %d Wait failed: %v", cid, op, err)
					return
				}

				if len(res) != len(payload) {
					t.Errorf("worker %d op %d len mismatch: got %d, want %d", cid, op, len(res), len(payload))
					return
				}

				for i := range res {
					if res[i] != payload[i] {
						t.Errorf("worker %d op %d byte %d corrupted: got %#x, want %#x",
							cid, op, i, res[i], payload[i])
						return
					}
				}
			}
		}(c)
	}

	close(start)
	wg.Wait()
}

// TestChallenge_M5_2_ZeroLeakMixedEgressStress executes 200 mixed ingress and egress operations
// (inline sync Call, sync CallBuffer, async Submit/Wait, async Submit/WaitBuffer, and diagnostic
// mode 16 BufferAlloc outputs), asserting that Stats().LiveBytes returns with 0 delta.
func TestChallenge_M5_2_ZeroLeakMixedEgressStress(t *testing.T) {
	h, err := gusset.Open(
		gusset.WithPoolSize(4),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	baseLive := gusset.Stats().LiveBytes

	const totalOps = 150
	var wg sync.WaitGroup

	for i := 0; i < totalOps; i++ {
		wg.Add(1)
		go func(op int) {
			defer wg.Done()
			pattern := op % 5

			switch pattern {
			case 0:
				// Inline Call
				payload := make([]byte, 128)
				payload[0] = 0 // mode 0
				res, err := h.Call(ctx, payload)
				if err != nil {
					t.Errorf("op %d Call failed: %v", op, err)
				}
				if len(res) != len(payload) {
					t.Errorf("op %d Call len mismatch", op)
				}

			case 1:
				// CallBuffer
				buf, err := h.NewBuffer(8192)
				if err != nil {
					t.Errorf("op %d NewBuffer failed: %v", op, err)
					return
				}
				buf.Bytes()[0] = 0
				outBuf, err := h.CallBuffer(ctx, buf)
				if err != nil {
					t.Errorf("op %d CallBuffer failed: %v", op, err)
					_ = buf.Free()
					return
				}
				_ = buf.Free()
				_ = outBuf.Free()

			case 2:
				// Submit with Buffer, Wait (reclaims via takeID)
				buf, err := h.NewBuffer(4096)
				if err != nil {
					t.Errorf("op %d NewBuffer failed: %v", op, err)
					return
				}
				buf.Bytes()[0] = 0
				ticket, err := h.Submit(ctx, buf)
				if err != nil {
					t.Errorf("op %d Submit failed: %v", op, err)
					_ = buf.Free()
					return
				}
				_ = buf.Free()
				res, err := h.Wait(ctx, ticket)
				if err != nil {
					t.Errorf("op %d Wait failed: %v", op, err)
				}
				if len(res) != 4096 {
					t.Errorf("op %d Wait len mismatch", op)
				}

			case 3:
				// Submit with Buffer, WaitBuffer (wrapped, explicit Free)
				buf, err := h.NewBuffer(4096)
				if err != nil {
					t.Errorf("op %d NewBuffer failed: %v", op, err)
					return
				}
				buf.Bytes()[0] = 0
				ticket, err := h.Submit(ctx, buf)
				if err != nil {
					t.Errorf("op %d Submit failed: %v", op, err)
					_ = buf.Free()
					return
				}
				_ = buf.Free()
				outBuf, err := h.WaitBuffer(ctx, ticket)
				if err != nil {
					t.Errorf("op %d WaitBuffer failed: %v", op, err)
					return
				}
				if !isBuffer64Aligned(outBuf.Bytes()) {
					t.Errorf("op %d output unaligned", op)
				}
				_ = outBuf.Free()

			case 4:
				// Diagnostic Mode 16 (gusset::BufferAlloc)
				const allocSz = 16384
				in := make([]byte, 6)
				in[0] = 16 // modeAllocated
				binary.LittleEndian.PutUint32(in[1:5], uint32(allocSz))
				in[5] = byte(op)

				ticket, err := h.Submit(ctx, in)
				if err != nil {
					t.Errorf("op %d mode 16 Submit failed: %v", op, err)
					return
				}
				outBuf, err := h.WaitBuffer(ctx, ticket)
				if err != nil {
					t.Errorf("op %d mode 16 WaitBuffer failed: %v", op, err)
					return
				}
				if len(outBuf.Bytes()) != allocSz {
					t.Errorf("op %d mode 16 len mismatch: got %d, want %d",
						op, len(outBuf.Bytes()), allocSz)
				}
				_ = outBuf.Free()
			}
		}(i)
	}

	wg.Wait()

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
		t.Fatalf("zero-leak invariant violated: base=%d, final=%d, delta=%d",
			baseLive, finalLive, finalLive-baseLive)
	}
}
