package pitfalls_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestHardening_M2_TeardownUnconsumedTakeBuffersDeallocated verifies that when a handle
// is closed with uncollected take buffers (large results > 4096 bytes), the teardown
// reordering in handleState.close() correctly deallocates all Rust buffer memory before
// nulling the handle pointer, restoring LiveBytes to 0.
func TestHardening_M2_TeardownUnconsumedTakeBuffersDeallocated(t *testing.T) {
	baselineLive := gusset.Stats().LiveBytes

	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Mode 16 (diagnostic_allocated): input[1..5] is output length in u32 LE.
	// 8192 = 0x2000 => []byte{16, 0x00, 0x20, 0x00, 0x00, 42}
	// Because 8192 > 4096 (MAX_INLINE_INPUT), worker promotes the output
	// to a Rust-owned take buffer and drain records it in takeIDs.
	ctx := context.Background()
	mode16Input := []byte{16, 0x00, 0x20, 0x00, 0x00, 42}

	const jobs = 6
	for i := 0; i < jobs; i++ {
		_, err := h.Submit(ctx, mode16Input)
		if err != nil {
			t.Fatalf("Submit %d failed: %v", i, err)
		}
	}

	// Allow the worker pool and drain loop to process and buffer the completions into takeIDs.
	// Give up to 1 second for LiveBytes to exceed baseline.
	deadline := time.Now().Add(1 * time.Second)
	var activeLive uint64
	for time.Now().Before(deadline) {
		activeLive = gusset.Stats().LiveBytes
		if activeLive > baselineLive {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if activeLive <= baselineLive {
		t.Fatalf("expected LiveBytes to increase from baseline %d, got %d", baselineLive, activeLive)
	}

	// Close the handle WITHOUT calling Wait() or Discard() on the tickets.
	// The fix in handleState.close() must free all takeIDs before destroying the handle.
	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baselineLive {
		t.Fatalf("expected LiveBytes to return to baseline %d after Close, got %d", baselineLive, finalLive)
	}
}

// TestHardening_M2_BoundedUncollectedResultsBackpressureAndRejection verifies that
// WithMaxUncollected bounds uncollected completed results on a handle, applying backpressure
// and rejecting invalid bounds.
func TestHardening_M2_BoundedUncollectedResultsBackpressureAndRejection(t *testing.T) {
	// 1. Negative bound must be refused as ErrBadArg on Open.
	_, err := gusset.Open(gusset.WithMaxUncollected(-1))
	if err == nil {
		t.Fatal("expected Open(WithMaxUncollected(-1)) to fail, got nil")
	}
	var gussetErr *gusset.Error
	if !errors.As(err, &gussetErr) || gussetErr.Code != gusset.ErrBadArg.Code {
		t.Fatalf("expected ErrBadArg, got: %v", err)
	}

	// 2. Bound of 3 uncollected results.
	const maxUncollected = 3
	h, err := gusset.Open(
		gusset.WithPoolSize(2),
		gusset.WithDiagnosticEngine(),
		gusset.WithMaxUncollected(maxUncollected),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx := context.Background()
	in := []byte{0, 1, 2, 3} // Mode 0 Echo

	var tickets []uint64
	for i := 0; i < maxUncollected; i++ {
		ticket, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatalf("Submit %d failed: %v", i, err)
		}
		tickets = append(tickets, ticket)
	}

	// Wait briefly for drain loop to collect all 3 into completed map.
	time.Sleep(50 * time.Millisecond)

	// 3. Submitting when uncollected limit is reached must block until timeout or return ErrTooManyUncollected.
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = h.Submit(timeoutCtx, in)
	if err == nil {
		t.Fatal("expected Submit to block and fail under full uncollected queue, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, gusset.ErrTooManyUncollected) {
		t.Fatalf("expected DeadlineExceeded or ErrTooManyUncollected, got: %v", err)
	}

	// 4. Claiming or discarding one uncollected ticket unblocks submission.
	if err := h.Discard(tickets[0]); err != nil {
		t.Fatalf("Discard failed: %v", err)
	}

	// Next submit must now succeed immediately.
	ticket, err := h.Submit(ctx, in)
	if err != nil {
		t.Fatalf("Submit after Discard failed: %v", err)
	}
	if err := h.Discard(ticket); err != nil {
		t.Fatalf("Discard of final ticket failed: %v", err)
	}
}

// TestHardening_M2_PanicFirewall_NonStringAndDoublePanicDestructor verifies that the panic
// firewall cleanly extracts non-string panic payloads (e.g. i32, i64, bool) and contains
// recursive destructor panics (DropBomb) without crashing the process.
func TestHardening_M2_PanicFirewall_NonStringAndDoublePanicDestructor(t *testing.T) {
	// Mode 3: panic_any(42i32)
	{
		h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer h.Close()

		_, err = h.Call(context.Background(), []byte{3})
		if err == nil {
			t.Fatal("expected panic error from Mode 3, got nil")
		}
		if !errors.Is(err, gusset.ErrPanic) {
			t.Fatalf("expected ErrPanic, got: %v", err)
		}
		if !strings.Contains(err.Error(), "42") {
			t.Fatalf("expected panic description to contain '42', got: %v", err)
		}

		// Subsequent calls must report ErrPoisoned.
		_, err = h.Call(context.Background(), []byte{0, 1})
		if !errors.Is(err, gusset.ErrPoisoned) {
			t.Fatalf("expected ErrPoisoned after panic, got: %v", err)
		}
	}

	// Mode 15: DropBomb recursive destructor panic.
	{
		h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer h.Close()

		// Mode 15 with count 4: destructor panics 4 times recursively on drop.
		_, err = h.Call(context.Background(), []byte{15, 4})
		if err == nil {
			t.Fatal("expected panic error from Mode 15 DropBomb, got nil")
		}
		if !errors.Is(err, gusset.ErrPanic) {
			t.Fatalf("expected ErrPanic, got: %v", err)
		}
		if !strings.Contains(err.Error(), "diagnostic payload destructor panicked") &&
			!strings.Contains(err.Error(), "Unknown Rust panic payload") {
			t.Fatalf("unexpected panic message: %v", err)
		}

		// Subsequent calls must report ErrPoisoned.
		_, err = h.Call(context.Background(), []byte{0, 1})
		if !errors.Is(err, gusset.ErrPoisoned) {
			t.Fatalf("expected ErrPoisoned after DropBomb panic, got: %v", err)
		}
	}
}

// TestHardening_M2_BufferFreePoisoning verifies that Buffer.Free() immediately poisons
// user-accessible slice memory with 0xDE and clears the view.
func TestHardening_M2_BufferFreePoisoning(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// 1. Rust-backed buffer
	buf, err := h.NewBuffer(128)
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}
	slice := buf.Bytes()
	for i := range slice {
		slice[i] = 0xAA
	}

	if err := buf.Free(); err != nil {
		t.Fatalf("Free failed: %v", err)
	}

	// Bytes() must immediately return nil.
	if buf.Bytes() != nil {
		t.Fatal("expected Bytes() == nil after Free()")
	}

	// Buffer ID must be 0 after Free().
	if buf.ID() != 0 {
		t.Fatalf("expected ID() == 0 after Free(), got %d", buf.ID())
	}

	// The previously extracted slice must be poisoned or zeroed by deallocation,
	// and must never retain the original user data (0xAA).
	for i, b := range slice {
		if b == 0xAA {
			t.Fatalf("byte at index %d was not poisoned: retained original 0xAA", i)
		}
	}

	// 2. Go-memory buffer from WaitBuffer (<= 4 KiB inline result)
	resBuf, err := h.WaitBuffer(context.Background(), func() uint64 {
		tk, err := h.Submit(context.Background(), []byte{0, 1, 2, 3, 4})
		if err != nil {
			t.Fatalf("Submit failed: %v", err)
		}
		return tk
	}())
	if err != nil {
		t.Fatalf("WaitBuffer failed: %v", err)
	}

	resSlice := resBuf.Bytes()
	if !bytes.Equal(resSlice, []byte{0, 1, 2, 3, 4}) {
		t.Fatalf("unexpected resSlice content: %v", resSlice)
	}

	if err := resBuf.Free(); err != nil {
		t.Fatalf("resBuf.Free failed: %v", err)
	}

	if resBuf.Bytes() != nil {
		t.Fatal("expected resBuf.Bytes() == nil after Free()")
	}

	for i, b := range resSlice {
		if b != 0xDE {
			t.Fatalf("resSlice byte at index %d was not poisoned: got 0x%02X, want 0xDE", i, b)
		}
	}
}
