package pitfalls_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
	"github.com/bharathvbcr/gusset/internal/ffi"
)

// Pitfall 1: ABI Drift Check (Invariant I6, R12)
func TestPitfall_ABILayoutMatch(t *testing.T) {
	layout := ffi.GetAbiLayout()
	if layout.Version != gusset.ExpectedAbiVersion {
		t.Fatalf("ABI version mismatch: expected %d, got %d", gusset.ExpectedAbiVersion, layout.Version)
	}

	expectedSizes := [ffi.AbiTypeCount]uint32{
		gusset.ExpectedHeaderSize, gusset.ExpectedStatusSize,
		gusset.ExpectedLayoutSize, gusset.ExpectedStatsSize,
	}
	if layout.Sizes != expectedSizes {
		t.Fatalf("ABI sizes mismatch: expected %v, got %v", expectedSizes, layout.Sizes)
	}

	expectedAligns := [ffi.AbiTypeCount]uint32{
		gusset.ExpectedHeaderAlign, gusset.ExpectedStatusAlign,
		gusset.ExpectedLayoutAlign, gusset.ExpectedStatsAlign,
	}
	if layout.Aligns != expectedAligns {
		t.Fatalf("ABI aligns mismatch: expected %v, got %v", expectedAligns, layout.Aligns)
	}

	// AllocStats crosses the boundary through gusset_alloc_stats, which writes it
	// straight into Go memory. ABI version 1 did not cover it, so a size or
	// alignment disagreement corrupted the Go heap with nothing to catch it.
	localSizes, localAligns := ffi.LocalLayout()
	for i := 0; i < ffi.AbiTypeCount; i++ {
		if localSizes[i] != layout.Sizes[i] || localAligns[i] != layout.Aligns[i] {
			t.Fatalf("ABI drift for %s: Rust size %d align %d, cgo size %d align %d",
				ffi.AbiTypeNames[i], layout.Sizes[i], layout.Aligns[i],
				localSizes[i], localAligns[i])
		}
	}
}

// Pitfall 2: Handle Poisoning (Invariant I2, R10)
func TestPitfall_HandlePoisoning(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx := context.Background()

	// Cause panic to trigger poisoning
	_, err = h.Call(ctx, []byte{1})
	if !errors.Is(err, gusset.ErrPanic) {
		t.Fatalf("expected ErrPanic, got: %v", err)
	}

	// Any subsequent call must fail immediately with ErrPoisoned
	for i := 0; i < 5; i++ {
		_, err = h.Call(ctx, []byte{0, 1, 2})
		if !errors.Is(err, gusset.ErrPoisoned) {
			t.Fatalf("call %d: expected ErrPoisoned, got: %v", i, err)
		}
	}
}

// Pitfall 3: Deadline Enforcement inside Rust (Invariant I3, R9)
func TestPitfall_DeadlineEnforcement(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// Short deadline of 50ms, while Mode 5 sleeps 10ms per iteration for 50 iterations (500ms)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = h.Call(ctx, []byte{5, 50})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected deadline/cancellation error, got nil")
	}

	// Must return promptly around the deadline, not hanging for the full 500ms
	if elapsed > 250*time.Millisecond {
		t.Fatalf("call took %v, deadline was not enforced promptly", elapsed)
	}
	t.Logf("deadline correctly interrupted execution after %v: %v", elapsed, err)
}

// Pitfall 4: Goroutine Migration Safety (Rule R7)
func TestPitfall_GoroutineMigration(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	var wg sync.WaitGroup
	ctx := context.Background()

	// Run concurrent goroutines calling the handle repeatedly
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				runtime.Gosched() // encourage goroutine migration
				payload := []byte{0, byte(id), byte(i)}
				res, err := h.Call(ctx, payload)
				if err != nil {
					t.Errorf("worker %d iteration %d failed: %v", id, i, err)
					return
				}
				if len(res) != len(payload) || res[1] != byte(id) || res[2] != byte(i) {
					t.Errorf("worker %d iteration %d corrupted response: %v", id, i, res)
					return
				}
			}
		}(g)
	}

	wg.Wait()
}

// Pitfall 5: Large Input and Buffer Zero-Copy (Rule R16)
func TestPitfall_LargeBufferAllocAndFree(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	size := 64 * 1024 // 64 KiB > 4 KiB limit
	buf, err := h.NewBuffer(size)
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}

	slice := buf.Bytes()
	if len(slice) != size {
		t.Fatalf("expected len %d, got %d", size, len(slice))
	}

	// Fill buffer with test pattern
	for i := range slice {
		slice[i] = byte(i % 251)
	}

	// Submit by buffer ID
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ticket, err := h.Submit(ctx, buf)
	if err != nil {
		t.Fatalf("Submit with Buffer failed: %v", err)
	}

	res, err := h.Wait(ctx, ticket)
	if err != nil {
		t.Fatalf("Wait failed: %v", err)
	}

	if len(res) != size {
		t.Fatalf("expected result len %d, got %d", size, len(res))
	}
	if res[100] != byte(100%251) {
		t.Fatalf("data mismatch at index 100")
	}

	// Explicit Free (R4)
	if err := buf.Free(); err != nil {
		t.Fatalf("buf.Free failed: %v", err)
	}
}

// Pitfall 6: Rust Allocator Accounting and Go Memory Limit (Rule R4)
func TestPitfall_AllocatorStatsAndLimit(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	initialStats := gusset.Stats()

	// Allocate a 1 MiB buffer
	const allocSize = 1024 * 1024
	buf, err := h.NewBuffer(allocSize)
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}

	postAllocStats := gusset.Stats()
	if postAllocStats.LiveBytes < initialStats.LiveBytes+allocSize {
		t.Fatalf("expected LiveBytes to increase by at least %d, initial: %d, post: %d",
			allocSize, initialStats.LiveBytes, postAllocStats.LiveBytes)
	}

	// Test AdviseMemoryLimit
	prevLimit := gusset.AdviseMemoryLimit(256 * 1024 * 1024)
	t.Logf("previous memory limit: %d bytes, current live Rust memory: %d bytes",
		prevLimit, postAllocStats.LiveBytes)

	// Free buffer
	_ = buf.Free()

	postFreeStats := gusset.Stats()
	if postFreeStats.LiveBytes > postAllocStats.LiveBytes-allocSize {
		t.Fatalf("expected LiveBytes to decrease after free, post-alloc: %d, post-free: %d",
			postAllocStats.LiveBytes, postFreeStats.LiveBytes)
	}
}

// Pitfall 7: Stack Size on Deep Recursion (Invariant I5, Rule R8)
func TestPitfall_DeepRecursionOnWorkerStack(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Mode 6 recurses 50,000 frames on the worker thread with 8 MiB stack.
	// On a 128 KiB musl default stack, this would trigger stack overflow SIGSEGV.
	depth := uint32(50_000)
	input := make([]byte, 5)
	input[0] = 6
	input[1] = byte(depth)
	input[2] = byte(depth >> 8)
	input[3] = byte(depth >> 16)
	input[4] = byte(depth >> 24)

	res, err := h.Call(ctx, input)
	if err != nil {
		t.Fatalf("Deep recursion call failed: %v", err)
	}

	if len(res) < 4 {
		t.Fatalf("expected 4 bytes return, got %d", len(res))
	}
	t.Logf("deep recursion completed successfully on 8 MiB worker stack")
}

// Pitfall 8: Concurrency Bound and Thread Cap Soak (Invariant I4, Rule R11)
func TestPitfall_ThreadCapBoundedSoak(t *testing.T) {
	const poolSize = 4
	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	initialThreads := gusset.Threads()
	t.Logf("initial scheduler thread count: %d", initialThreads)

	const callers = 500
	var wg sync.WaitGroup
	ctx := context.Background()

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload := []byte{0, byte(id & 0xFF)}
			res, err := h.Call(ctx, payload)
			if err != nil {
				t.Errorf("caller %d failed: %v", id, err)
				return
			}
			if len(res) == 0 || res[1] != byte(id&0xFF) {
				t.Errorf("corrupted echo from caller %d", id)
			}
		}(i)
	}

	wg.Wait()

	maxThreads := gusset.Threads()
	t.Logf("post-soak thread count: %d", maxThreads)

	// Invariant I4: threads must not explode under load
	bound := int64(poolSize + runtime.GOMAXPROCS(0) + 16)
	if maxThreads > initialThreads+bound {
		t.Fatalf("thread count exploded: started with %d, peaked at %d, exceeded bound %d",
			initialThreads, maxThreads, bound)
	}
}
