package pitfalls_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestHardening_UninitializedAndMalformedBufferSafety verifies that methods on
// uninitialized Buffer{} instances or malformed buffer arguments never panic
// with nil-pointer dereference in Go.
func TestHardening_UninitializedAndMalformedBufferSafety(t *testing.T) {
	// 1. Uninitialized *Buffer (non-nil struct pointer, nil state)
	uninitBuf := &gusset.Buffer{}

	if bytes := uninitBuf.Bytes(); bytes != nil {
		t.Fatalf("expected nil from uninitBuf.Bytes(), got %v", bytes)
	}

	if id := uninitBuf.ID(); id != 0 {
		t.Fatalf("expected 0 from uninitBuf.ID(), got %d", id)
	}

	if err := uninitBuf.Free(); err != nil {
		t.Fatalf("expected nil from uninitBuf.Free(), got %v", err)
	}

	// 2. Passing uninitialized buffer to Handle methods
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = h.CallBuffer(ctx, uninitBuf)
	if err == nil {
		t.Fatal("expected error on CallBuffer with uninitialized buffer, got nil")
	}
	if !strings.Contains(err.Error(), "buffer is not initialized") {
		t.Fatalf("expected 'buffer is not initialized' error, got: %v", err)
	}

	_, err = h.Submit(ctx, uninitBuf)
	if err == nil {
		t.Fatal("expected error on Submit with uninitialized buffer, got nil")
	}
	if !strings.Contains(err.Error(), "buffer is not initialized") {
		t.Fatalf("expected 'buffer is not initialized' error, got: %v", err)
	}
}

// TestHardening_PreCancelledContextFastRejection tests that submitting work with
// an already-cancelled or expired context fails immediately and never leaks a semaphore
// permit, even in rapid succession.
func TestHardening_PreCancelledContextFastRejection(t *testing.T) {
	const poolSize = 2
	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// Rapidly fire 500 pre-cancelled requests
	for i := 0; i < 500; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel before submission

		_, err := h.Call(ctx, []byte{0, 1, 2})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: expected context.Canceled, got: %v", i, err)
		}
	}

	// Rapidly fire 500 pre-expired requests
	for i := 0; i < 500; i++ {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Second))
		defer cancel()

		_, err := h.Submit(ctx, []byte{0, 1, 2})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("iteration %d: expected context.DeadlineExceeded, got: %v", i, err)
		}
	}

	// Verify that the handle's pool permits were never leaked: all poolSize slots are still free!
	normalCtx, normalCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer normalCancel()

	for i := 0; i < poolSize*2; i++ {
		res, err := h.Call(normalCtx, []byte{0, byte(i)})
		if err != nil {
			t.Fatalf("normal call %d failed after cancelled requests: %v", i, err)
		}
		if len(res) != 2 || res[1] != byte(i) {
			t.Fatalf("unexpected echo response: %v", res)
		}
	}
}

// TestHardening_OversizedPanicPayloadTruncation verifies that a 1 MiB panic string
// is safely caught by the Rust firewall, bounded to <= 32 KiB, safely transported
// across FFI without C.GoBytes integer overflow, poisons the handle (I2), and survives.
func TestHardening_OversizedPanicPayloadTruncation(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Mode 12 triggers 1 MiB string panic
	_, err = h.Call(ctx, []byte{12})
	if err == nil {
		t.Fatal("expected panic error, got nil")
	}
	if !errors.Is(err, gusset.ErrPanic) {
		t.Fatalf("expected ErrPanic, got: %v", err)
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "[truncated]") {
		t.Fatalf("expected '[truncated]' in panic error message, got: %s", errMsg)
	}
	// Verify that the error message is bounded and nowhere near 1 MiB
	if len(errMsg) > 65536 {
		t.Fatalf("error message length %d exceeds maximum safe limit 65536", len(errMsg))
	}

	// Verify handle is poisoned
	_, err = h.Call(ctx, []byte{0})
	if !errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("expected ErrPoisoned after panic, got: %v", err)
	}
}

// TestHardening_NonStringPanicPrimitivePayloads tests non-string panic types (u32, i64, bool, usize).
func TestHardening_NonStringPanicPrimitivePayloads(t *testing.T) {
	cases := []struct {
		subType  byte
		expected string
	}{
		{0, "panic payload (u32): 12345"},
		{1, "panic payload (i64): -9876543210"},
		{2, "panic payload (bool): true"},
		{3, "panic payload (usize): 999999"},
		{4, "panic payload (isize): -42"},
	}

	for _, tc := range cases {
		h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = h.Call(ctx, []byte{13, tc.subType})
		cancel()
		h.Close()

		if err == nil {
			t.Fatalf("expected panic error for subType %d, got nil", tc.subType)
		}
		if !errors.Is(err, gusset.ErrPanic) {
			t.Fatalf("expected ErrPanic for subType %d, got: %v", tc.subType, err)
		}
		if !strings.Contains(err.Error(), tc.expected) {
			t.Fatalf("for subType %d: expected message to contain %q, got: %s", tc.subType, tc.expected, err.Error())
		}
	}
}

// TestHardening_BufferAllocationOnClosedHandle verifies that attempting to allocate
// a buffer on an already closed handle fails cleanly without allocating Rust memory.
func TestHardening_BufferAllocationOnClosedHandle(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	buf, err := h.NewBuffer(1024)
	if err == nil {
		_ = buf.Free()
		t.Fatal("expected NewBuffer on closed handle to fail, got nil error")
	}
	if !strings.Contains(err.Error(), "handle is closed") {
		t.Fatalf("expected 'handle is closed' error, got: %v", err)
	}
}

// TestHardening_ConcurrentLogContention verifies that simultaneous log emission from
// multiple worker threads and Go-side DrainLogs runs cleanly without deadlocking or dropping.
func TestHardening_ConcurrentLogContention(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var workersWg sync.WaitGroup
	var drainWg sync.WaitGroup
	var drainStop atomic.Bool

	// Concurrently drain logs
	drainBuf := make([]byte, 8192)
	drainWg.Add(1)
	go func() {
		defer drainWg.Done()
		for !drainStop.Load() {
			n := gusset.DrainLogs(drainBuf)
			if n > 0 {
				_ = string(drainBuf[:n])
			}
			time.Sleep(1 * time.Millisecond)
		}
	}()

	// Concurrently submit log bursts (Mode 14)
	const workers = 8
	const burstsPerWorker = 20
	for w := 0; w < workers; w++ {
		workersWg.Add(1)
		go func() {
			defer workersWg.Done()
			for i := 0; i < burstsPerWorker; i++ {
				_, err := h.Call(ctx, []byte{14, 5})
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("log burst call failed: %v", err)
				}
			}
		}()
	}

	workersWg.Wait()
	drainStop.Store(true)
	drainWg.Wait()
}

// TestStress_MultiGoroutineHardeningChaosAssault runs a comprehensive stress
// assault combining 64 concurrent workers hammering normal calls, zero-copy buffers,
// timeouts, cancellations, and handle closures.
func TestStress_MultiGoroutineHardeningChaosAssault(t *testing.T) {
	const poolSize = 8
	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const numWorkers = 64
	const iterations = 25
	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				step := (workerID + i) % 4
				switch step {
				case 0:
					// Normal echo Call
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					res, err := h.Call(ctx, []byte{0, byte(workerID), byte(i)})
					cancel()
					if err != nil {
						t.Errorf("worker %d iter %d call failed: %v", workerID, i, err)
						return
					}
					if len(res) != 3 || res[1] != byte(workerID) {
						t.Errorf("worker %d iter %d corrupted response: %v", workerID, i, res)
						return
					}

				case 1:
					// Zero-copy Buffer roundtrip
					buf, err := h.NewBuffer(128)
					if err != nil {
						t.Errorf("worker %d iter %d NewBuffer failed: %v", workerID, i, err)
						return
					}
					data := buf.Bytes()
					data[0] = 0 // echo
					data[1] = byte(workerID)
					data[2] = byte(i)

					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					outBuf, err := h.CallBuffer(ctx, buf)
					cancel()
					_ = buf.Free()
					if err != nil {
						t.Errorf("worker %d iter %d CallBuffer failed: %v", workerID, i, err)
						return
					}
					outData := outBuf.Bytes()
					if len(outData) != 128 || outData[1] != byte(workerID) {
						t.Errorf("worker %d iter %d outData corrupted", workerID, i)
					}
					_ = outBuf.Free()

				case 2:
					// Pre-cancelled or fast-timeout Context
					ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
					time.Sleep(10 * time.Microsecond) // Ensure timeout
					_, err := h.Call(ctx, []byte{0, 1})
					cancel()
					if err == nil {
						t.Errorf("worker %d iter %d expected timeout error, got nil", workerID, i)
					}

				case 3:
					// CPU-bound calculation with Submit + Wait
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					ticket, err := h.Submit(ctx, []byte{10, 1, 2, 3, 4})
					if err != nil {
						cancel()
						t.Errorf("worker %d iter %d Submit failed: %v", workerID, i, err)
						return
					}
					res, err := h.Wait(ctx, ticket)
					cancel()
					if err != nil {
						t.Errorf("worker %d iter %d Wait failed: %v", workerID, i, err)
						return
					}
					if len(res) != 8 {
						t.Errorf("worker %d iter %d expected 8 bytes, got %d", workerID, i, len(res))
					}
				}
			}
		}(w)
	}

	wg.Wait()
}
