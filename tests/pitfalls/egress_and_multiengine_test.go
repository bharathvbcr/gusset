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

	statsBefore := gusset.Stats()

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

	// Force GC and give finalizers / cleanups time to run
	for i := 0; i < 5; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}

	statsAfter := gusset.Stats()

	// All abandoned buffers must be reclaimed by AddCleanup
	if statsAfter.LiveBytes != statsBefore.LiveBytes {
		t.Fatalf("unfreed buffers leaked memory: before=%d live=%d", statsBefore.LiveBytes, statsAfter.LiveBytes)
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

	statsBefore := gusset.Stats()

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

	// Wait for the slow task to finish and pipe drainer to process it
	time.Sleep(150 * time.Millisecond)

	statsAfter := gusset.Stats()

	if statsAfter.LiveBytes != statsBefore.LiveBytes {
		t.Fatalf("timed-out buffer leaked: before=%d, after=%d", statsBefore.LiveBytes, statsAfter.LiveBytes)
	}
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
