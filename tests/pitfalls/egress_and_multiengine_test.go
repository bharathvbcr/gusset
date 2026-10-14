package pitfalls_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestZeroCopyEgress_WaitBufferReturnsBufferDirectly validates the R16 zero-copy egress path:
// WaitBuffer returns a Rust-owned *Buffer without copying memory into Go heap.
func TestZeroCopyEgress_WaitBufferReturnsBufferDirectly(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Submit an echo task (mode 0)
	input := []byte{0, 0xDE, 0xAD, 0xBE, 0xEF}
	ticket, err := h.Submit(ctx, input)
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	buf, err := h.WaitBuffer(ctx, ticket)
	if err != nil {
		t.Fatalf("WaitBuffer failed: %v", err)
	}
	defer func() {
		if err := buf.Free(); err != nil {
			t.Fatalf("buf.Free failed: %v", err)
		}
	}()

	slice := buf.Bytes()
	if len(slice) != len(input) {
		t.Fatalf("expected length %d, got %d", len(input), len(slice))
	}
	for i := range input {
		if slice[i] != input[i] {
			t.Fatalf("byte mismatch at %d: expected %x, got %x", i, input[i], slice[i])
		}
	}

	// Double Free is safe and idempotent
	if err := buf.Free(); err != nil {
		t.Fatalf("second Free returned error: %v", err)
	}
	if buf.Bytes() != nil {
		t.Fatal("Bytes() must return nil after Free()")
	}
}

// liveBytesNotLeaked is the leak predicate for the process-wide LiveBytes
// gauge. A drop under the quiesced baseline is an earlier test's AddCleanup,
// not a leak. A leak is a reading that stays above that baseline.
func liveBytesNotLeaked(baseline, live uint64) bool {
	return live <= baseline
}

// quiesceLiveBytes GCs until two consecutive LiveBytes readings match, and
// returns that reading. Snapshotting while an earlier test's cleanup is still
// running lets the counter fall under the snapshot; an equality wait then
// spins for 30s and reports a leak.
func quiesceLiveBytes(t *testing.T) uint64 {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	prev := gusset.Stats().LiveBytes
	for {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
		live := gusset.Stats().LiveBytes
		if live == prev {
			return live
		}
		if time.Now().After(deadline) {
			t.Fatalf("LiveBytes did not quiesce: last=%d prev=%d", live, prev)
		}
		prev = live
	}
}

// settleLiveBytes polls until liveBytesNotLeaked accepts the reading, with
// the same 10ms/30s settle as waitLiveBytes. collect forces a GC on each pass
// so an abandoned *Buffer's AddCleanup can run; the sample waitLiveBytes
// already took is from before that GC, and the next pass observes it.
func settleLiveBytes(baseline uint64, collect bool) (uint64, bool) {
	return waitLiveBytes(func(live uint64) bool {
		if collect {
			runtime.GC()
		}
		return liveBytesNotLeaked(baseline, live)
	})
}

// TestZeroCopyEgress_WaitBufferAddCleanupFreesOnGC verifies that if an adopter
// abandons a *Buffer returned by WaitBuffer without calling Free(), the
// runtime.AddCleanup backstop automatically reclaims it in Rust memory (R4).
func TestZeroCopyEgress_WaitBufferAddCleanupFreesOnGC(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	baseline := quiesceLiveBytes(t)

	// Create and abandon 10 buffers in an inner function
	abandonBuffers := func() {
		for i := 0; i < 10; i++ {
			ticket, err := h.Submit(ctx, []byte{0, 1, 2, 3})
			if err != nil {
				t.Fatalf("Submit %d failed: %v", i, err)
			}
			buf, err := h.WaitBuffer(ctx, ticket)
			if err != nil {
				t.Fatalf("WaitBuffer %d failed: %v", i, err)
			}
			_ = buf.Bytes()
			// intentionally do not call buf.Free()
		}
	}
	abandonBuffers()

	// All abandoned buffers must be reclaimed by AddCleanup. Each GC hands the
	// cleanup goroutine only the objects it found, and that goroutine is as
	// slow as the host, so collect until the counter is back at or under the
	// quiesced baseline. A buffer nothing reclaims stays above it and fails
	// when the deadline passes. A counter that drops further is an earlier
	// test's cleanup, not a leak (I4/R16).
	if live, ok := settleLiveBytes(baseline, true); !ok {
		t.Fatalf("unfreed buffers leaked memory: baseline=%d live=%d", baseline, live)
	}
}

// TestZeroCopyEgress_WaitBufferTimeoutCleansUpBuffer verifies that when a context
// deadline expires while waiting on a ticket, any buffer returned upon completion
// is automatically cleaned up and does not leak (I4, R16).
func TestZeroCopyEgress_WaitBufferTimeoutCleansUpBuffer(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	baseline := quiesceLiveBytes(t)

	// Mode 5: delay iterations (each iteration takes 10ms)
	// Mode 5 with 10 iterations takes ~100ms
	submitCtx := context.Background()
	ticket, err := h.Submit(submitCtx, []byte{5, 10})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Wait with a timeout that expires before the job completes
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err = h.WaitBuffer(waitCtx, ticket)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}

	// Wait for the slow task to finish and the drainer to reclaim its result.
	// Each of mode 5's ten 10 ms sleeps costs several times that on a loaded
	// host, so a fixed nap measured the host. Poll until LiveBytes is at or
	// under the quiesced baseline; a leaked buffer stays above it. A drop
	// under the baseline is an earlier test's cleanup, not a leak (I4/R16).
	if live, ok := settleLiveBytes(baseline, false); !ok {
		t.Fatalf("timed-out buffer leaked: baseline=%d, live=%d", baseline, live)
	}
}

// TestZeroCopyEgress_LiveBytesLeakIsAboveBaselineOnly pins the predicate the
// two leak waits use. LiveBytes is process-wide, so the check has to tell a
// buffer this test still holds from a counter that fell because some earlier
// test's cleanup ran after the snapshot.
func TestZeroCopyEgress_LiveBytesLeakIsAboveBaselineOnly(t *testing.T) {
	t.Run("injected readings", func(t *testing.T) {
		if !liveBytesNotLeaked(1000, 0) {
			t.Fatal("a reading under the snapshot is an earlier cleanup, not a leak")
		}
		if !liveBytesNotLeaked(1000, 1000) {
			t.Fatal("the snapshot itself is not a leak")
		}
		if liveBytesNotLeaked(1000, 1001) {
			t.Fatal("a reading above the snapshot is a buffer this test still holds")
		}
	})

	t.Run("held buffer stays above, free may drop under", func(t *testing.T) {
		h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer h.Close()

		baseline := quiesceLiveBytes(t)
		// Held on purpose: not Free'd and not left for AddCleanup. A 1 MiB
		// buffer cannot hide in allocator noise.
		buf, err := h.NewBuffer(1 << 20)
		if err != nil {
			t.Fatalf("NewBuffer failed: %v", err)
		}
		defer func() {
			if err := buf.Free(); err != nil {
				t.Errorf("Free failed: %v", err)
			}
		}()
		held := gusset.Stats().LiveBytes
		if liveBytesNotLeaked(baseline, held) {
			t.Fatalf("a buffer this test still holds must fail the leak check: baseline=%d live=%d", baseline, held)
		}

		if err := buf.Free(); err != nil {
			t.Fatalf("Free failed: %v", err)
		}
		// `held` includes this buffer. Free drops the gauge under that
		// snapshot; the same predicate must accept the drop, and must not
		// leave the buffer charged against the quiesced baseline.
		live, ok := settleLiveBytes(held, false)
		if !ok {
			t.Fatalf("Free did not bring LiveBytes back: snapshot=%d live=%d", held, live)
		}
		if live >= held {
			t.Fatalf("Free must drop LiveBytes under the snapshot taken while the buffer was held: snapshot=%d live=%d", held, live)
		}
		if !liveBytesNotLeaked(held, live) {
			t.Fatalf("a drop under the snapshot must pass: snapshot=%d live=%d", held, live)
		}
		live, ok = settleLiveBytes(baseline, false)
		if !ok {
			t.Fatalf("after Free, LiveBytes stayed above the quiesced baseline: baseline=%d live=%d", baseline, live)
		}
	})
}

// TestMultiEngine_WithOpcodeAndContextWithOpcode tests opcode configuration and routing.
func TestMultiEngine_WithOpcodeAndContextWithOpcode(t *testing.T) {
	// Handle configured with default opcode 100
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithOpcode(100))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Default opcode 100 has no registered engine -> must fail closed
	_, err = h.Call(ctx, []byte{1, 2, 3})
	if err == nil {
		t.Fatal("call with unregistered default opcode 100 must fail")
	}
	if !strings.Contains(err.Error(), "no engine handler registered") {
		t.Fatalf("expected 'no engine handler registered', got: %v", err)
	}

	// Override opcode per-call using ContextWithOpcode
	ctxOp200 := gusset.ContextWithOpcode(ctx, 200)
	_, err = h.Call(ctxOp200, []byte{1, 2, 3})
	if err == nil {
		t.Fatal("call with unregistered opcode 200 must fail")
	}
	if !strings.Contains(err.Error(), "no engine handler registered") {
		t.Fatalf("expected 'no engine handler registered', got: %v", err)
	}
}

// TestMultiEngine_DiagnosticEnginePrecedence verifies that opcode dispatch
// takes precedence over the diagnostic engine fallback.
func TestMultiEngine_DiagnosticEnginePrecedence(t *testing.T) {
	// Handle with both WithOpcode and WithDiagnosticEngine
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithOpcode(999), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Opcode 999 is unregistered: it must fail closed and NOT execute the diagnostic echo (mode 0)
	_, err = h.Call(ctx, []byte{0, 1, 2, 3})
	if err == nil {
		t.Fatal("unregistered opcode must fail closed even when diagnostic engine is enabled")
	}
	if !strings.Contains(err.Error(), "no engine handler registered") {
		t.Fatalf("expected 'no engine handler registered', got: %v", err)
	}

	// Context with opcode 0 falls back to default dispatch (which uses diagnostic engine if enabled)
	ctxOp0 := gusset.ContextWithOpcode(ctx, 0)
	res, err := h.Call(ctxOp0, []byte{0, 42})
	if err != nil {
		t.Fatalf("opcode 0 fallback to diagnostic engine failed: %v", err)
	}
	if len(res) != 2 || res[1] != 42 {
		t.Fatalf("unexpected echo response: %v", res)
	}
}

// TestZeroCopyEgress_ConcurrentStress tests high concurrency with WaitBuffer.
func TestZeroCopyEgress_ConcurrentStress(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(8), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const numGoroutines = 20
	const iterations = 50
	var wg sync.WaitGroup
	var completed atomic.Int64

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				payload := []byte{0, byte(gid), byte(i)}
				ticket, err := h.Submit(ctx, payload)
				if err != nil {
					t.Errorf("Submit failed: %v", err)
					return
				}
				buf, err := h.WaitBuffer(ctx, ticket)
				if err != nil {
					t.Errorf("WaitBuffer failed: %v", err)
					return
				}
				slice := buf.Bytes()
				if len(slice) != len(payload) || slice[1] != byte(gid) || slice[2] != byte(i) {
					t.Errorf("corrupted buffer: %v", slice)
					_ = buf.Free()
					return
				}
				if err := buf.Free(); err != nil {
					t.Errorf("buf.Free failed: %v", err)
					return
				}
				completed.Add(1)
			}
		}(g)
	}

	wg.Wait()
	if completed.Load() != int64(numGoroutines*iterations) {
		t.Fatalf("expected %d completions, got %d", numGoroutines*iterations, completed.Load())
	}
}

// TestZeroCopyEgress_WaitBufferZeroByteOutput verifies that a job returning 0 bytes
// cleanly produces a valid, non-error *Buffer with empty Bytes() and safe Free().
func TestZeroCopyEgress_WaitBufferZeroByteOutput(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Submit empty payload with diagnostic engine (mode 0 with empty slice returns empty slice)
	ticket, err := h.Submit(ctx, []byte{})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	buf, err := h.WaitBuffer(ctx, ticket)
	if err != nil {
		t.Fatalf("WaitBuffer failed on empty output: %v", err)
	}
	if buf == nil {
		t.Fatal("expected non-nil Buffer")
	}

	if len(buf.Bytes()) != 0 {
		t.Fatalf("expected 0 bytes, got %v", buf.Bytes())
	}

	if err := buf.Free(); err != nil {
		t.Fatalf("buf.Free failed on 0-byte buffer: %v", err)
	}
}

// TestAdversarial_SubmitNilBufferRejection ensures that passing a nil *Buffer
// is rejected with an error instead of panicking with a nil-pointer dereference.
func TestAdversarial_SubmitNilBufferRejection(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx := context.Background()
	var nilBuf *gusset.Buffer

	_, err = h.Submit(ctx, nilBuf)
	if err == nil {
		t.Fatal("expected error submitting nil *Buffer, got nil")
	}
	if !strings.Contains(err.Error(), "buffer is nil") {
		t.Fatalf("expected 'buffer is nil' error, got: %v", err)
	}
}

// TestAdversarial_SubmitFreedBufferRejection ensures that passing a freed *Buffer
// is rejected at the boundary.
func TestAdversarial_SubmitFreedBufferRejection(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	buf, err := h.NewBuffer(64)
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}
	if err := buf.Free(); err != nil {
		t.Fatalf("buf.Free failed: %v", err)
	}

	ctx := context.Background()
	_, err = h.Submit(ctx, buf)
	if err == nil {
		t.Fatal("expected error submitting freed buffer, got nil")
	}
	if !strings.Contains(err.Error(), "buffer is freed or closed") {
		t.Fatalf("expected 'buffer is freed or closed' error, got: %v", err)
	}
}

// TestAdversarial_SubmitCrossHandleBufferRejection ensures that a Buffer allocated
// on Handle A cannot be submitted to Handle B.
func TestAdversarial_SubmitCrossHandleBufferRejection(t *testing.T) {
	h1, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open h1 failed: %v", err)
	}
	defer h1.Close()

	h2, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open h2 failed: %v", err)
	}
	defer h2.Close()

	buf, err := h1.NewBuffer(64)
	if err != nil {
		t.Fatalf("h1.NewBuffer failed: %v", err)
	}
	defer buf.Free()

	ctx := context.Background()
	_, err = h2.Submit(ctx, buf)
	if err == nil {
		t.Fatal("expected cross-handle buffer submission to fail, got nil")
	}
	if !strings.Contains(err.Error(), "buffer belongs to a different handle") {
		t.Fatalf("expected 'buffer belongs to a different handle' error, got: %v", err)
	}
}

// TestHardening_AdviseMemoryLimitNegativeQuery verifies that AdviseMemoryLimit(-1)
// queries the current limit non-destructively without forcing to the floor.
func TestHardening_AdviseMemoryLimitNegativeQuery(t *testing.T) {
	current := gusset.AdviseMemoryLimit(-1)
	if current <= 0 {
		t.Fatalf("expected positive memory limit from query, got %d", current)
	}

	// Calling AdviseMemoryLimit(-1) a second time should return the exact same setting
	second := gusset.AdviseMemoryLimit(-1)
	if second != current {
		t.Fatalf("non-destructive query altered memory limit: first=%d, second=%d", current, second)
	}
}

// TestPitfall_WaitBufferDoesNotCopyTakeAllocation is the R16 WaitBuffer contract
// for JobResult::Ok, not just JobResult::Buffer.
//
// gusset_take copies engine bytes into a Rust-owned buffer and returns that id.
// drainPipe then allocated a Go slice, copied the bytes, and freed the Rust
// buffer; WaitBuffer allocated a *second* Rust buffer and copied again. A 64 KiB
// result therefore hit the Go heap once per completion, which is exactly the copy
// WaitBuffer exists to avoid. The take buffer is now the WaitBuffer result.
func TestPitfall_WaitBufferDoesNotCopyTakeAllocation(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const payload = 64 * 1024
	in, err := h.NewBuffer(payload)
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}
	defer in.Free()
	b := in.Bytes()
	b[0] = 0 // diagnostic echo
	for i := 1; i < len(b); i++ {
		b[i] = byte(i)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const runs = 8
	var totalDelta uint64
	for i := 0; i < runs; i++ {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		before := ms.TotalAlloc

		ticket, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatalf("Submit %d failed: %v", i, err)
		}
		out, err := h.WaitBuffer(ctx, ticket)
		if err != nil {
			t.Fatalf("WaitBuffer %d failed: %v", i, err)
		}
		got := out.Bytes()
		if len(got) != payload {
			t.Fatalf("expected %d bytes, got %d", payload, len(got))
		}
		if got[100] != byte(100) {
			t.Fatalf("echo corrupted at 100: %d", got[100])
		}
		if err := out.Free(); err != nil {
			t.Fatalf("Free %d failed: %v", i, err)
		}

		runtime.ReadMemStats(&ms)
		totalDelta += ms.TotalAlloc - before
	}

	avg := totalDelta / runs
	// drainPipe used to make([]byte, 64KiB) per completion. Wrapper bookkeeping
	// sits in the low kilobytes; a payload copy cannot hide under 32 KiB.
	if avg >= 32*1024 {
		t.Fatalf("WaitBuffer averaged %d Go-heap bytes per 64 KiB result; the take buffer must be wrapped, not copied", avg)
	}
}

// TestPitfall_WaitOfLargeTakeBufferDoesNotPayWrapperAllocs is the Wait() twin
// of TestPitfall_WaitBufferDoesNotCopyTakeAllocation.
//
// drainPipe wrapping take()'s buffer as a *Buffer so WaitBuffer can steal it
// taxes every Wait() of a large result: a Buffer object, AddCleanup, then a
// 64 KiB copy, then Free. HEAD copied once in drainPipe (3 allocs/op). The
// wrapper belongs at WaitBuffer consume time, not at take time.
func TestPitfall_WaitOfLargeTakeBufferDoesNotPayWrapperAllocs(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const payload = 64 * 1024
	in, err := h.NewBuffer(payload)
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}
	defer in.Free()
	b := in.Bytes()
	b[0] = 0
	for i := 1; i < len(b); i++ {
		b[i] = byte(i)
	}

	ctx := context.Background()
	// Warm the path so AllocsPerRun does not count first-call setup.
	ticket, err := h.Submit(ctx, in)
	if err != nil {
		t.Fatalf("warmup Submit failed: %v", err)
	}
	out, err := h.Wait(ctx, ticket)
	if err != nil {
		t.Fatalf("warmup Wait failed: %v", err)
	}
	if len(out) != payload || out[100] != byte(100) {
		t.Fatalf("warmup echo mismatch: len=%d byte100=%d", len(out), out[100])
	}

	allocs := testing.AllocsPerRun(20, func() {
		ticket, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatalf("Submit failed: %v", err)
		}
		out, err := h.Wait(ctx, ticket)
		if err != nil {
			t.Fatalf("Wait failed: %v", err)
		}
		if len(out) != payload {
			t.Fatalf("expected %d bytes, got %d", payload, len(out))
		}
	})
	// Submit+Wait bookkeeping on the small path is 2 allocs/op. The 64 KiB
	// copy is the third. An eager *Buffer wrap in drainPipe pushes this to 6.
	if allocs > 4 {
		t.Fatalf("Wait of a 64 KiB take buffer allocated %.1f objects/op; want ≤ 4 (copy + submit bookkeeping). Eager Buffer wrap in drainPipe is the extra tax", allocs)
	}
}
