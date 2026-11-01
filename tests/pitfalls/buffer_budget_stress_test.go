package pitfalls_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/bharathvbcr/gusset"
)

// isAligned64 tests whether the underlying backing array of a slice is 64-byte aligned.
func isAligned64(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	return uintptr(unsafe.Pointer(unsafe.SliceData(b)))%64 == 0
}

// TestBufferBudget_ConcurrentSaturationAndZeroDrift tests 32 goroutines concurrently
// allocating against a 512 KiB budget over 500 iterations, validating saturation rejection
// (ErrBufferBudget), exact budget boundary clamp, and zero drift in live bytes.
func TestBufferBudget_ConcurrentSaturationAndZeroDrift(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping buffer budget saturation in short mode")
	}

	baseLive := gusset.Stats().LiveBytes
	const budget = 512 * 1024
	h, err := gusset.Open(
		gusset.WithBufferBudget(budget),
		gusset.WithPoolSize(4),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const numGoroutines = 32
	const iterations = 500
	var budgetRefusals atomic.Int64
	var allocsCompleted atomic.Int64
	var wg sync.WaitGroup

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(gid)+10, 9999))
			var held []*gusset.Buffer

			for i := 0; i < iterations; i++ {
				sz := 1024 + r.IntN(63*1024) // 1 KiB to 64 KiB
				buf, err := h.NewBuffer(sz)
				if err != nil {
					if errors.Is(err, gusset.ErrBufferBudget) {
						budgetRefusals.Add(1)
						// Free oldest buffer if held to allow concurrency cycle
						if len(held) > 0 {
							_ = held[0].Free()
							held = held[1:]
						}
						continue
					}
					t.Errorf("unexpected NewBuffer error: %v", err)
					return
				}

				allocsCompleted.Add(1)
				slice := buf.Bytes()
				if len(slice) > 0 {
					slice[0] = byte(gid)
				}
				held = append(held, buf)

				// Hold up to 3 buffers concurrently per worker
				if len(held) > 3 {
					_ = held[0].Free()
					held = held[1:]
				}
			}

			// Clean up all remaining held buffers
			for _, buf := range held {
				_ = buf.Free()
			}
		}(g)
	}

	wg.Wait()

	if budgetRefusals.Load() == 0 {
		t.Errorf("expected budget refusals under concurrent saturation, got 0")
	}

	// Exact Budget Clamp and Zero Drift Verification:
	// 1. After freeing all buffers, exact budget allocation must succeed
	exactBuf, err := h.NewBuffer(budget)
	if err != nil {
		t.Fatalf("exact budget NewBuffer(%d) failed after complete free: %v", budget, err)
	}
	_ = exactBuf.Free()

	// 2. Allocating budget + 1 must fail with ErrBufferBudget
	overBuf, err := h.NewBuffer(budget + 1)
	if !errors.Is(err, gusset.ErrBufferBudget) {
		if overBuf != nil {
			_ = overBuf.Free()
		}
		t.Fatalf("expected NewBuffer(%d) to fail with ErrBufferBudget, got: %v", budget+1, err)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
		t.Fatalf("LiveBytes leak detected: baseline %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}

	t.Logf("ConcurrentSaturation passed: completed=%d, refusals=%d, LiveBytes delta=0",
		allocsCompleted.Load(), budgetRefusals.Load())
}

// TestBufferBudget_ExtremeBoundaryPayloadSweep sweeps boundary buffer sizes (0, negative,
// >1GiB, odd sizes, alignment edges, and 16 MiB multi-megabyte payloads), validating
// 64-byte alignment, 0x00 zero-initialization, read/write integrity, and post-Free zeroing.
func TestBufferBudget_ExtremeBoundaryPayloadSweep(t *testing.T) {
	baseLive := gusset.Stats().LiveBytes
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// 1. Refusal sizes: <= 0 and > MaxBufferBytes (1 GiB)
	badSizes := []int{0, -1, -64, (1 << 30) + 1}
	for _, sz := range badSizes {
		buf, err := h.NewBuffer(sz)
		if !errors.Is(err, gusset.ErrBadArg) {
			if buf != nil {
				_ = buf.Free()
			}
			t.Errorf("expected size %d to fail with ErrBadArg, got err=%v", sz, err)
		}
	}

	// 2. Valid boundary sizes: odd, alignment edges, and 16 MiB multi-megabyte payload
	validSizes := []int{
		1, 3, 7, 63, 64, 65,
		127, 128, 129,
		4095, 4096, 4097,
		65535, 65536, 65537,
		16 * 1024 * 1024, // 16 MiB multi-megabyte
	}

	for _, sz := range validSizes {
		buf, err := h.NewBuffer(sz)
		if err != nil {
			t.Fatalf("NewBuffer(%d) failed: %v", sz, err)
		}

		slice := buf.Bytes()
		if len(slice) != sz {
			t.Fatalf("size %d: slice length mismatch: got %d", sz, len(slice))
		}

		// 64-byte alignment check
		if !isAligned64(slice) {
			t.Fatalf("size %d: buffer pointer %p is not 64-byte aligned", sz, slice)
		}

		// Zero-fill verification:
		// Check all bytes for sizes <= 65536; sample for 16 MiB
		if sz <= 65536 {
			for i, b := range slice {
				if b != 0x00 {
					t.Fatalf("size %d: byte at index %d was not 0x00: got 0x%02X", sz, i, b)
				}
			}
		} else {
			// Sample first 4096 and last 4096
			for i := 0; i < 4096; i++ {
				if slice[i] != 0x00 {
					t.Fatalf("size %d: head byte %d was not 0x00: got 0x%02X", sz, i, slice[i])
				}
				if slice[sz-1-i] != 0x00 {
					t.Fatalf("size %d: tail byte %d was not 0x00: got 0x%02X", sz, sz-1-i, slice[sz-1-i])
				}
			}
		}

		// Write test pattern and verify read integrity
		slice[0] = 0x5A
		if sz > 1 {
			slice[sz-1] = 0xA5
		}
		if sz > 2 {
			slice[sz/2] = 0x3C
		}

		if slice[0] != 0x5A {
			t.Fatalf("size %d: pattern write slice[0] failed to read back", sz)
		}
		if sz > 1 && slice[sz-1] != 0xA5 {
			t.Fatalf("size %d: pattern write slice[sz-1] failed to read back", sz)
		}
		if sz > 2 && slice[sz/2] != 0x3C {
			t.Fatalf("size %d: mid pattern write failed to read back", sz)
		}

		// Free buffer
		if err := buf.Free(); err != nil {
			t.Fatalf("size %d: Free() failed: %v", sz, err)
		}

		if buf.Bytes() != nil {
			t.Fatalf("size %d: Bytes() was not nil after Free()", sz)
		}
		if buf.ID() != 0 {
			t.Fatalf("size %d: ID() was not 0 after Free()", sz)
		}
		if _, err := h.Submit(context.Background(), buf); err == nil {
			t.Fatalf("size %d: expected Submit with freed buffer to fail", sz)
		}
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

// TestBufferBudget_MultiMegabyteConcurrentSaturation tests allocating 16 MiB buffers
// against a 32 MiB budget across 16 concurrent goroutines, asserting that at most 2
// allocations succeed simultaneously, remaining receive ErrBufferBudget, and budget
// reclaims cleanly without drift.
func TestBufferBudget_MultiMegabyteConcurrentSaturation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-megabyte saturation in short mode")
	}

	baseLive := gusset.Stats().LiveBytes
	const budget = 32 * 1024 * 1024  // 32 MiB
	const bufSize = 16 * 1024 * 1024 // 16 MiB

	h, err := gusset.Open(
		gusset.WithBufferBudget(budget),
		gusset.WithPoolSize(4),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const callers = 16
	var currentlyHeld atomic.Int64
	var maxSimultaneous atomic.Int64
	var refusals atomic.Int64
	var successes atomic.Int64
	var wg sync.WaitGroup

	start := make(chan struct{})

	for c := 0; c < callers; c++ {
		wg.Add(1)
		go func(cid int) {
			defer wg.Done()
			<-start

			for trial := 0; trial < 10; trial++ {
				buf, err := h.NewBuffer(bufSize)
				if err != nil {
					if errors.Is(err, gusset.ErrBufferBudget) {
						refusals.Add(1)
						time.Sleep(2 * time.Millisecond)
						continue
					}
					t.Errorf("caller %d unexpected NewBuffer error: %v", cid, err)
					return
				}

				successes.Add(1)
				cur := currentlyHeld.Add(1)
				for {
					prev := maxSimultaneous.Load()
					if cur <= prev || maxSimultaneous.CompareAndSwap(prev, cur) {
						break
					}
				}

				if cur > 2 {
					t.Errorf("INVARIANT VIOLATION: %d simultaneous 16 MiB buffers held on 32 MiB budget", cur)
				}

				// Touch memory
				slice := buf.Bytes()
				if len(slice) > 0 {
					slice[0] = byte(cid)
					slice[len(slice)-1] = byte(trial)
				}

				time.Sleep(5 * time.Millisecond)
				currentlyHeld.Add(-1)
				_ = buf.Free()
			}
		}(c)
	}

	close(start)
	wg.Wait()

	t.Logf("MultiMegabyteSaturation: successes=%d, refusals=%d, peakHeld=%d",
		successes.Load(), refusals.Load(), maxSimultaneous.Load())

	if maxSimultaneous.Load() > 2 {
		t.Fatalf("peak simultaneous 16 MiB buffers %d exceeded budget limit of 2", maxSimultaneous.Load())
	}
	if refusals.Load() == 0 {
		t.Errorf("expected ErrBufferBudget refusals under 16-caller saturation of 32 MiB budget")
	}

	// Boundary verification after drain
	exactBuf, err := h.NewBuffer(budget)
	if err != nil {
		t.Fatalf("exact 32 MiB NewBuffer failed after drain: %v", err)
	}
	_ = exactBuf.Free()

	overBuf, err := h.NewBuffer(budget + 1)
	if !errors.Is(err, gusset.ErrBufferBudget) {
		if overBuf != nil {
			_ = overBuf.Free()
		}
		t.Fatalf("expected NewBuffer(32 MiB + 1) to fail with ErrBufferBudget, got: %v", err)
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

// TestBufferBudget_ConcurrentDoubleFreeIdempotency tests 50 buffers with 8 concurrent
// goroutines calling Free() per buffer, asserting zero races, zero panics, and zero budget underflow.
func TestBufferBudget_ConcurrentDoubleFreeIdempotency(t *testing.T) {
	baseLive := gusset.Stats().LiveBytes
	const budget = 1024 * 1024 // 1 MiB
	h, err := gusset.Open(
		gusset.WithBufferBudget(budget),
		gusset.WithPoolSize(2),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const numBuffers = 50
	const closersPerBuffer = 8

	buffers := make([]*gusset.Buffer, numBuffers)
	for i := 0; i < numBuffers; i++ {
		sz := 1024 * (1 + (i % 8)) // 1 KiB to 8 KiB
		buf, err := h.NewBuffer(sz)
		if err != nil {
			t.Fatalf("NewBuffer %d failed: %v", i, err)
		}
		buffers[i] = buf
	}

	var wg sync.WaitGroup
	for _, buf := range buffers {
		for c := 0; c < closersPerBuffer; c++ {
			wg.Add(1)
			go func(b *gusset.Buffer) {
				defer wg.Done()
				err := b.Free()
				if err != nil {
					t.Errorf("Free returned error: %v", err)
				}
			}(buf)
		}
	}
	wg.Wait()

	// Verify budget is intact and not underflowed:
	exactBuf, err := h.NewBuffer(budget)
	if err != nil {
		t.Fatalf("exact budget NewBuffer(%d) failed: %v", budget, err)
	}
	_ = exactBuf.Free()

	overBuf, err := h.NewBuffer(budget + 1)
	if !errors.Is(err, gusset.ErrBufferBudget) {
		if overBuf != nil {
			_ = overBuf.Free()
		}
		t.Fatalf("expected NewBuffer(%d) to fail with ErrBufferBudget, got: %v", budget+1, err)
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

// TestBufferBudget_ConcurrentCloseReclamation tests buffers allocated and held while
// a concurrent Close() runs, asserting that Free() after Close succeeds without leak or panic.
func TestBufferBudget_ConcurrentCloseReclamation(t *testing.T) {
	baseLive := gusset.Stats().LiveBytes
	const budget = 1024 * 1024
	h, err := gusset.Open(
		gusset.WithBufferBudget(budget),
		gusset.WithPoolSize(4),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const callers = 16
	var wg sync.WaitGroup
	var heldMu sync.Mutex
	var survivingBuffers []*gusset.Buffer
	start := make(chan struct{})

	for c := 0; c < callers; c++ {
		wg.Add(1)
		go func(cid int) {
			defer wg.Done()
			<-start
			for i := 0; i < 20; i++ {
				buf, err := h.NewBuffer(4096)
				if err != nil {
					return
				}
				if i == 19 {
					// Keep last buffer across Close()
					heldMu.Lock()
					survivingBuffers = append(survivingBuffers, buf)
					heldMu.Unlock()
				} else {
					_ = buf.Free()
				}
			}
		}(c)
	}

	close(start)
	time.Sleep(5 * time.Millisecond)

	// Close handle concurrently while workers allocate/free
	_ = h.Close()
	wg.Wait()

	// Free all buffers that survived Close()
	for _, buf := range survivingBuffers {
		if err := buf.Free(); err != nil {
			t.Errorf("Free() after Close returned error: %v", err)
		}
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
		t.Fatalf("LiveBytes leak detected: baseline %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}
}

//go:noinline
func allocateAndForget(h *gusset.Handle, size int) error {
	buf, err := h.NewBuffer(size)
	if err != nil {
		return err
	}
	slice := buf.Bytes()
	if len(slice) > 0 {
		slice[0] = 0x77
	}
	// Intentionally abandoned without calling buf.Free()
	return nil
}

// TestBufferBudget_GCFinalizerReclaimStress allocates buffers to exhaust budget without Free(),
// verifying that runtime.GC() invokes releaseForgottenBuffer to reclaim budget.
func TestBufferBudget_GCFinalizerReclaimStress(t *testing.T) {
	baseLive := gusset.Stats().LiveBytes
	const budget = 64 * 1024 // 64 KiB
	h, err := gusset.Open(
		gusset.WithBufferBudget(budget),
		gusset.WithPoolSize(2),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// 1. Exhaust budget by abandoning buffer
	if err := allocateAndForget(h, budget); err != nil {
		t.Fatalf("allocateAndForget failed: %v", err)
	}

	// 2. Immediate next allocation must fail with ErrBufferBudget
	overBuf, err := h.NewBuffer(1024)
	if !errors.Is(err, gusset.ErrBufferBudget) {
		if overBuf != nil {
			_ = overBuf.Free()
		}
		t.Fatalf("expected ErrBufferBudget before GC, got: %v", err)
	}

	// 3. Poll runtime.GC() until releaseForgottenBuffer runs and reclaims budget
	deadline := time.Now().Add(5 * time.Second)
	var recovered *gusset.Buffer
	for time.Now().Before(deadline) {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)

		b, err := h.NewBuffer(budget)
		if err == nil {
			recovered = b
			break
		}
	}

	if recovered == nil {
		t.Fatalf("GC finalizer failed to reclaim forgotten buffer budget within 5 seconds")
	}
	_ = recovered.Free()

	// 4. Exact budget verification
	exactBuf, err := h.NewBuffer(budget)
	if err != nil {
		t.Fatalf("exact NewBuffer failed after GC reclaim: %v", err)
	}
	_ = exactBuf.Free()

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
		t.Fatalf("LiveBytes leak detected: baseline %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}
}
