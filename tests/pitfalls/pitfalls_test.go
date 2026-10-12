package pitfalls_test

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// Pitfall: equal-width fields can trade places without changing a struct's size
// or alignment, which is all ABI version 2 checked. The named-offset table is
// what makes that swap visible. A check that sorted the offsets, or compared
// them as a multiset, would still accept it.
func TestPitfall_EqualWidthFieldSwapKeepsSizeAndBreaksNamedOffsets(t *testing.T) {
	rustOff, rustSz, n := ffi.RustFieldLayout()
	if int(n) != ffi.AbiFieldCount {
		t.Fatalf("Rust reported %d fields, Go compiled %d", n, ffi.AbiFieldCount)
	}
	localOff, localSz := ffi.LocalFieldLayout()
	if rustOff != localOff || rustSz != localSz {
		t.Fatalf("live field layouts disagree\n rust off %v sz %v\n cgo  off %v sz %v",
			rustOff, rustSz, localOff, localSz)
	}

	structSize := map[string]uint32{}
	sizes, _ := ffi.LocalLayout()
	for i, name := range ffi.AbiTypeNames {
		structSize[name] = sizes[i]
	}
	for i, name := range ffi.AbiFieldNames {
		ty, _, ok := strings.Cut(name, ".")
		if !ok {
			t.Fatalf("field name %q has no struct", name)
		}
		limit, known := structSize[ty]
		if !known {
			t.Fatalf("field %s is not on a checked struct", name)
		}
		if rustSz[i] == 0 {
			t.Fatalf("field %s has size 0; the check would pass vacuously", name)
		}
		if rustOff[i]+rustSz[i] > limit {
			t.Fatalf("field %s ends at %d, %s is %d bytes", name, rustOff[i]+rustSz[i], ty, limit)
		}
	}

	pairs := 0
	for i := 0; i < len(rustOff); i++ {
		for j := i + 1; j < len(rustOff); j++ {
			if rustSz[i] != rustSz[j] || rustOff[i] == rustOff[j] {
				continue
			}
			pairs++
			swapped := rustOff
			swapped[i], swapped[j] = swapped[j], swapped[i]
			if swapped == rustOff {
				t.Fatalf("swap of %s and %s did not move named offsets",
					ffi.AbiFieldNames[i], ffi.AbiFieldNames[j])
			}
			sortedA := append([]uint32(nil), rustOff[:]...)
			sortedB := append([]uint32(nil), swapped[:]...)
			slices.Sort(sortedA)
			slices.Sort(sortedB)
			if !slices.Equal(sortedA, sortedB) {
				t.Fatalf("equal-width swap of %s and %s changed the offset multiset",
					ffi.AbiFieldNames[i], ffi.AbiFieldNames[j])
			}
		}
	}
	if pairs == 0 {
		t.Fatal("no equal-width field pair; a size-only ABI check's blind spot is untested")
	}
	t.Logf("rejected %d equal-width field swaps that a size check would accept", pairs)
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
		// R10 is enforced twice: Rust refuses at the export, and Go latches
		// before entering Rust. Both answer ErrPoisoned, so the code alone
		// passes with either layer removed. Go's refusal is the one that says
		// the job panicked (the Rust one is the bare "handle is poisoned"),
		// and it is set before the caller sees ErrPanic.
		if !strings.Contains(err.Error(), "a job panicked; Close it and open a new handle") {
			t.Fatalf("call %d: refused by Rust, not by Go's poison latch: %v", i, err)
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
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("deadline must surface as a context error, got %v", err)
	}

	// Must return promptly around the deadline, not hanging for the full 500ms
	if elapsed > 250*time.Millisecond {
		t.Fatalf("call took %v, deadline was not enforced promptly", elapsed)
	}
	t.Logf("deadline correctly interrupted execution after %v: %v", elapsed, err)
}

// A Rust-side cancel is still a context cancellation. Error.Is used to match only
// other *Error values by code, so "cancelled: DeadlineExceeded" failed
// errors.Is(..., context.DeadlineExceeded) and GOGC=1's cancel-storm treated a
// correctly cancelled job as an unexpected failure.
func TestPitfall_FFICancelMapsToContextErrors(t *testing.T) {
	deadline := &gusset.Error{Code: gusset.ErrGeneric.Code, Msg: "cancelled: DeadlineExceeded"}
	if !errors.Is(deadline, context.DeadlineExceeded) {
		t.Fatalf("Rust deadline cancel must satisfy errors.Is(..., context.DeadlineExceeded); got Is=false for %v", deadline)
	}
	if !errors.Is(deadline, gusset.ErrGeneric) {
		t.Fatalf("deadline cancel must still match ErrGeneric; got %v", deadline)
	}
	explicit := &gusset.Error{Code: gusset.ErrGeneric.Code, Msg: "cancelled: Explicit"}
	if !errors.Is(explicit, context.Canceled) {
		t.Fatalf("Rust explicit cancel must satisfy errors.Is(..., context.Canceled); got Is=false for %v", explicit)
	}
	if errors.Is(explicit, context.DeadlineExceeded) {
		t.Fatal("explicit cancel must not also match DeadlineExceeded")
	}
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
//
// Two bounds, both on the PEAK rather than on a reading taken after the run:
//
//   - in-flight: the engine itself counts jobs executing at once (diagnostic
//     mode 17 raises an atomic on entry and lowers it on exit) and the highest
//     value it reached must not exceed the pool size. 500 callers on a pool of
//     4 is where an unbounded dispatch would show 500.
//   - threads: the scheduler's thread total, sampled every millisecond for the
//     whole run, stays within pool size + GOMAXPROCS + 8 of where it started,
//     the bound AGENTS.md states for R11.
//
// The thread count after the run says nothing about the run: Go never destroys
// an M, so it can only have grown, but a transient spike that settled would not
// have been seen by a single late reading either way.
func TestPitfall_ThreadCapBoundedSoak(t *testing.T) {
	const poolSize = 4
	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	initialThreads := gusset.Threads()
	t.Logf("initial scheduler thread count: %d", initialThreads)
	_ = peakInEngine(t, h) // reset the engine gauge

	var peakThreads atomic.Int64
	peakThreads.Store(initialThreads)
	stopSampling := make(chan struct{})
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		for {
			if n := gusset.Threads(); n > peakThreads.Load() {
				peakThreads.Store(n)
			}
			select {
			case <-stopSampling:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()

	const callers = 500
	var wg sync.WaitGroup
	ctx := context.Background()

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// A 2 ms non-cooperative hold, so jobs genuinely overlap.
			res, err := h.Call(ctx, holdInEngine(2))
			if err != nil {
				t.Errorf("caller %d failed: %v", id, err)
				return
			}
			if len(res) != 1 || res[0] != 17 {
				t.Errorf("unexpected result from caller %d: %v", id, res)
			}
		}(i)
	}

	wg.Wait()
	close(stopSampling)
	<-samplerDone

	// Invariant I4, seen from the engine.
	peak := peakInEngine(t, h)
	t.Logf("engine-side peak in-flight: %d (pool size %d)", peak, poolSize)
	if peak > poolSize {
		t.Fatalf("engine ran %d jobs at once on a pool of %d (I4)", peak, poolSize)
	}
	if peak < 2 {
		t.Fatalf("engine-side peak in-flight was %d: the soak never overlapped jobs, so the bound proved nothing", peak)
	}

	// Rule R11: threads must not explode under load.
	maxThreads := peakThreads.Load()
	t.Logf("peak scheduler thread count during the soak: %d", maxThreads)
	bound := int64(poolSize + runtime.GOMAXPROCS(0) + 8)
	if maxThreads > initialThreads+bound {
		t.Fatalf("thread count exploded: started with %d, peaked at %d, exceeded bound %d",
			initialThreads, maxThreads, bound)
	}
}
