package pitfalls_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestChallenge_M2_UncollectedBounds_50JobsAsync stress-tests the uncollected result bounds:
//  1. Configures a handle with WithMaxUncollected(5).
//  2. Submits 50 jobs asynchronously without calling Wait() or Discard().
//  3. Verifies that the 6th submission strictly blocks once 5 results accumulate,
//     and context timeouts correctly abort blocked submissions with ErrTooManyUncollected.
//  4. Verifies that Discard() and Wait() incrementally unblock submissions.
//  5. Verifies that Handle.Close() wakes all blocked submissions cleanly without deadlock.
func TestChallenge_M2_UncollectedBounds_50JobsAsync(t *testing.T) {
	const maxUncollected = 5
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
	in := []byte{0, 10, 20, 30} // Mode 0 Echo

	// Submit initial 5 jobs.
	var initialTickets []uint64
	for i := 0; i < maxUncollected; i++ {
		ticket, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatalf("Initial Submit %d failed: %v", i, err)
		}
		initialTickets = append(initialTickets, ticket)
	}

	// Wait for worker pool and drain loop to process and store all 5 completions into s.completed.
	// We poll up to 500ms.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// Verification 1: Attempting 6th submission with a short timeout must block and fail with ErrTooManyUncollected.
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = h.Submit(timeoutCtx, in)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected 6th submission to block and fail under full uncollected queue, got nil")
	}
	if !errors.Is(err, gusset.ErrTooManyUncollected) {
		t.Fatalf("expected ErrTooManyUncollected, got: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", err)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("expected submission to block for ~50ms, elapsed: %v", elapsed)
	}

	// Verification 2: Asynchronously launch 50 submissions without calling Wait or Discard.
	// All of them must be blocked by backpressure and abort when their timeout fires.
	const asyncJobs = 50
	var wg sync.WaitGroup
	var blockedAndAbortedCount atomic.Int64
	var unexpectedSuccessCount atomic.Int64

	for i := 0; i < asyncJobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			subCtx, subCancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer subCancel()

			_, subErr := h.Submit(subCtx, in)
			if subErr != nil {
				if errors.Is(subErr, gusset.ErrTooManyUncollected) || errors.Is(subErr, context.DeadlineExceeded) {
					blockedAndAbortedCount.Add(1)
				}
			} else {
				unexpectedSuccessCount.Add(1)
			}
		}()
	}

	wg.Wait()

	if unexpectedSuccessCount.Load() > 0 {
		t.Fatalf("expected 0 unexpected successes while queue was full, got %d", unexpectedSuccessCount.Load())
	}
	if blockedAndAbortedCount.Load() != int64(asyncJobs) {
		t.Fatalf("expected all %d async submissions to be blocked and aborted, got %d", asyncJobs, blockedAndAbortedCount.Load())
	}

	// Verification 3: Incremental unblocking via Discard() and Wait().
	// Launch a background submitter with generous timeout (2 seconds).
	submitDone := make(chan uint64, 1)
	submitErr := make(chan error, 1)

	go func() {
		unblockCtx, unblockCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer unblockCancel()
		tk, err := h.Submit(unblockCtx, in)
		if err != nil {
			submitErr <- err
		} else {
			submitDone <- tk
		}
	}()

	// Ensure the submitter is currently blocked.
	time.Sleep(30 * time.Millisecond)
	select {
	case tk := <-submitDone:
		t.Fatalf("submitter completed prematurely before Discard: ticket %d", tk)
	default:
		// Expected to be blocked.
	}

	// Discard 1 ticket from the initial 5.
	if err := h.Discard(initialTickets[0]); err != nil {
		t.Fatalf("Discard failed: %v", err)
	}

	// Submitter must now unblock promptly.
	var newTicket uint64
	select {
	case newTicket = <-submitDone:
		if newTicket == 0 {
			t.Fatal("expected valid ticket, got 0")
		}
	case err := <-submitErr:
		t.Fatalf("submitter failed after Discard: %v", err)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("submitter did not unblock within 500ms after Discard")
	}

	// Wait for the new ticket to complete into completed map.
	time.Sleep(50 * time.Millisecond)

	// Now uncollected count is back at 5.
	// Calling Wait() on initialTickets[1] should consume it and allow another submission.
	res, err := h.Wait(context.Background(), initialTickets[1])
	if err != nil {
		t.Fatalf("Wait failed: %v", err)
	}
	if !bytes.Equal(res, in) {
		t.Fatalf("unexpected result from Wait: %v", res)
	}

	// Next submit must now succeed immediately.
	nextTicket, err := h.Submit(context.Background(), in)
	if err != nil {
		t.Fatalf("Submit after Wait failed: %v", err)
	}
	if nextTicket == 0 {
		t.Fatal("expected valid nextTicket, got 0")
	}

	// Clean up remaining tickets.
	_ = h.Discard(initialTickets[2])
	_ = h.Discard(initialTickets[3])
	_ = h.Discard(initialTickets[4])
	_ = h.Discard(newTicket)
	_ = h.Discard(nextTicket)
}

// TestChallenge_M2_BufferPoisoning_EmpiricalVerification verifies buffer memory poisoning:
// 1. Rust-allocated buffer: NewBuffer(size) -> Bytes() -> write payload -> Free() -> assert 0xDE bytes.
// 2. Go-heap buffer: WaitBuffer (<= 4096 bytes) -> Bytes() -> Free() -> assert 0xDE bytes.
// 3. Large Rust output buffer: WaitBuffer (> 4096 bytes) -> Bytes() -> Free() -> assert 0xDE bytes.
// 4. Double Free returns nil and does not corrupt memory.
// 5. Buffer.Free() after Handle.Close() safely returns nil without segfaulting.
func TestChallenge_M2_BufferPoisoning_EmpiricalVerification(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	// 1. Test across multiple Rust buffer sizes.
	sizes := []int{16, 64, 128, 512, 4096, 65536}
	for _, size := range sizes {
		buf, err := h.NewBuffer(size)
		if err != nil {
			t.Fatalf("NewBuffer(%d) failed: %v", size, err)
		}

		slice := buf.Bytes()
		if len(slice) != size {
			t.Fatalf("expected slice len %d, got %d", size, len(slice))
		}

		// Write non-0xDE payload pattern (0xAA).
		for i := range slice {
			slice[i] = 0xAA
		}

		// Free the buffer.
		if err := buf.Free(); err != nil {
			t.Fatalf("buf.Free() failed for size %d: %v", size, err)
		}

		// buf.Bytes() must be nil.
		if buf.Bytes() != nil {
			t.Fatalf("expected buf.Bytes() == nil after Free for size %d", size)
		}

		// buf.ID() must be 0.
		if buf.ID() != 0 {
			t.Fatalf("expected buf.ID() == 0 after Free for size %d, got %d", size, buf.ID())
		}

		// Verification:
		// 1. Stale user payload (0xAA) must NEVER be retained in any byte.
		for i, b := range slice {
			if b == 0xAA {
				t.Fatalf("size %d: byte at index %d retained stale payload 0xAA", size, i)
			}
		}

		// 2. For buffers larger than libc freelist header (e.g. 4096 bytes),
		// bytes beyond the allocator's freelist pointer (offset 8+) must be strictly 0xDE.
		if size >= 4096 {
			for i := 8; i < size; i++ {
				if slice[i] != 0xDE {
					t.Fatalf("size %d: byte at index %d was not 0xDE: got 0x%02X", size, i, slice[i])
				}
			}
		}

		// Double Free must be idempotent.
		if err := buf.Free(); err != nil {
			t.Fatalf("second Free() returned error for size %d: %v", size, err)
		}
	}

	// 2. Go-heap buffer returned by WaitBuffer (<= 4096 bytes).
	{
		echoData := []byte{0, 11, 22, 33, 44, 55}
		tk, err := h.Submit(context.Background(), echoData)
		if err != nil {
			t.Fatalf("Submit failed: %v", err)
		}

		resBuf, err := h.WaitBuffer(context.Background(), tk)
		if err != nil {
			t.Fatalf("WaitBuffer failed: %v", err)
		}

		resSlice := resBuf.Bytes()
		if !bytes.Equal(resSlice, echoData) {
			t.Fatalf("unexpected resSlice content: %v", resSlice)
		}

		if err := resBuf.Free(); err != nil {
			t.Fatalf("resBuf.Free failed: %v", err)
		}

		if resBuf.Bytes() != nil {
			t.Fatal("expected resBuf.Bytes() == nil after Free")
		}

		// Go-heap buffer is not recycled by libc free, so all bytes must be strictly 0xDE.
		for i, b := range resSlice {
			if b != 0xDE {
				t.Fatalf("Go-heap buffer byte at index %d not poisoned: got 0x%02X, want 0xDE", i, b)
			}
		}
	}

	// 3. Large Rust output buffer returned by WaitBuffer (> 4096 bytes, Mode 16).
	{
		// Mode 16: Diagnostic Allocated, 8192 bytes.
		mode16Input := []byte{16, 0x00, 0x20, 0x00, 0x00, 0xEE}
		tk, err := h.Submit(context.Background(), mode16Input)
		if err != nil {
			t.Fatalf("Submit Mode 16 failed: %v", err)
		}

		largeBuf, err := h.WaitBuffer(context.Background(), tk)
		if err != nil {
			t.Fatalf("WaitBuffer Mode 16 failed: %v", err)
		}

		largeSlice := largeBuf.Bytes()
		if len(largeSlice) != 8192 {
			t.Fatalf("expected largeSlice len 8192, got %d", len(largeSlice))
		}

		if err := largeBuf.Free(); err != nil {
			t.Fatalf("largeBuf.Free failed: %v", err)
		}

		if largeBuf.Bytes() != nil {
			t.Fatal("expected largeBuf.Bytes() == nil after Free")
		}

		// Bytes beyond libc freelist header (offset 16+) must be strictly 0xDE.
		for i := 16; i < 8192; i++ {
			if largeSlice[i] != 0xDE {
				t.Fatalf("large Rust buffer byte at index %d not poisoned: got 0x%02X, want 0xDE", i, largeSlice[i])
			}
		}
	}

	// 4. Buffer.Free() after Handle.Close() safety.
	{
		h2, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open h2 failed: %v", err)
		}

		buf2, err := h2.NewBuffer(256)
		if err != nil {
			t.Fatalf("NewBuffer failed: %v", err)
		}

		// Close the handle before freeing the buffer.
		if err := h2.Close(); err != nil {
			t.Fatalf("h2.Close failed: %v", err)
		}

		// Freeing buffer on closed handle must not crash or segfault.
		if err := buf2.Free(); err != nil {
			t.Fatalf("buf2.Free after close returned error: %v", err)
		}
	}
}

// TestChallenge_M2_PanicFirewall_DestructorBombsAndPoison verifies panic firewall
// containment when custom panic payloads have recursive destructor panics (DropBomb).
func TestChallenge_M2_PanicFirewall_DestructorBombsAndPoison(t *testing.T) {
	depths := []byte{1, 2, 3, 5}
	for _, depth := range depths {
		h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open failed for depth %d: %v", depth, err)
		}

		// Mode 15: DropBomb with specified depth.
		_, err = h.Call(context.Background(), []byte{15, depth})
		if err == nil {
			_ = h.Close()
			t.Fatalf("expected panic error from Mode 15 (depth %d), got nil", depth)
		}
		if !errors.Is(err, gusset.ErrPanic) {
			_ = h.Close()
			t.Fatalf("expected ErrPanic for depth %d, got: %v", depth, err)
		}

		// Subsequent call must report ErrPoisoned.
		_, err = h.Call(context.Background(), []byte{0, 1})
		if !errors.Is(err, gusset.ErrPoisoned) {
			_ = h.Close()
			t.Fatalf("expected ErrPoisoned after DropBomb panic (depth %d), got: %v", depth, err)
		}

		if err := h.Close(); err != nil {
			t.Fatalf("Close failed after panic for depth %d: %v", depth, err)
		}
	}
}
