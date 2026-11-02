package pitfalls_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestTeardown_UnconsumedTakeBuffersReclaimedOnClose verifies that when a handle
// is closed with uncollected take buffers (large results > 4096 bytes), the teardown
// ordering in handleState.close() deallocates all Rust buffer memory before
// nulling the handle pointer, restoring LiveBytes to baseline.
func TestTeardown_UnconsumedTakeBuffersReclaimedOnClose(t *testing.T) {
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
	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baselineLive {
		t.Fatalf("expected LiveBytes to return to baseline %d after Close, got %d", baselineLive, finalLive)
	}
}

// TestTeardown_LargeBatchTakeBuffersReclaimedOnClose submits 200 large asynchronous jobs
// (>4096 bytes) returning Rust take buffers, waits for them to populate takeIDs without
// calling Wait() or Discard(), and asserts that Close() reclaims every byte.
func TestTeardown_LargeBatchTakeBuffersReclaimedOnClose(t *testing.T) {
	baselineLive := gusset.Stats().LiveBytes

	const poolSize = 16
	const jobCount = 200
	const bufferSize = 8192 // > 4096 MAX_INLINE_INPUT, promotes to Rust take buffer

	h, err := gusset.Open(
		gusset.WithPoolSize(poolSize),
		gusset.WithDiagnosticEngine(),
		gusset.WithMaxUncollected(jobCount+50),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	ctx := context.Background()
	mode16Input := func(seed byte) []byte {
		return []byte{16, 0x00, 0x20, 0x00, 0x00, seed}
	}

	for i := 0; i < jobCount; i++ {
		tk, err := h.Submit(ctx, mode16Input(byte(i)))
		if err != nil {
			t.Fatalf("Submit %d failed: %v", i, err)
		}
		if tk == 0 {
			t.Fatalf("Submit %d returned zero ticket", i)
		}
	}

	expectedGrowth := uint64(jobCount * bufferSize)
	deadline := time.Now().Add(5 * time.Second)
	var activeLive uint64
	for time.Now().Before(deadline) {
		activeLive = gusset.Stats().LiveBytes
		if activeLive >= baselineLive+expectedGrowth {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	if activeLive < baselineLive+expectedGrowth {
		t.Fatalf("expected LiveBytes to increase to at least %d (growth >= %d), got activeLive %d",
			baselineLive+expectedGrowth, expectedGrowth, activeLive)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baselineLive {
		t.Fatalf("LEAK DETECTED: expected LiveBytes to return to baseline %d after Close(), got %d (delta %d)",
			baselineLive, finalLive, int64(finalLive)-int64(baselineLive))
	}
}

// TestTeardown_InFlightLargeTakeBuffersReclaimedOnClose tests closing a handle immediately
// while 200 large jobs are actively in flight.
func TestTeardown_InFlightLargeTakeBuffersReclaimedOnClose(t *testing.T) {
	baselineLive := gusset.Stats().LiveBytes

	const poolSize = 16
	const jobCount = 200

	h, err := gusset.Open(
		gusset.WithPoolSize(poolSize),
		gusset.WithDiagnosticEngine(),
		gusset.WithMaxUncollected(jobCount+50),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	ctx := context.Background()
	mode16Input := []byte{16, 0x00, 0x20, 0x00, 0x00, 0x55}

	for i := 0; i < jobCount; i++ {
		_, err := h.Submit(ctx, mode16Input)
		if err != nil && !errors.Is(err, gusset.ErrClosed) {
			t.Fatalf("Submit %d failed: %v", i, err)
		}
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baselineLive {
		t.Fatalf("LEAK DETECTED: expected LiveBytes to return to baseline %d after in-flight Close(), got %d (delta %d)",
			baselineLive, finalLive, int64(finalLive)-int64(baselineLive))
	}
}

// TestTeardown_ConcurrentSubmitAndCloseTakeBufferReclamation runs 100 concurrent goroutines
// submitting and abandoning take buffers while concurrently closing and reopening handles.
func TestTeardown_ConcurrentSubmitAndCloseTakeBufferReclamation(t *testing.T) {
	baselineLive := gusset.Stats().LiveBytes

	type handleHolder struct {
		mu sync.RWMutex
		h  *gusset.Handle
	}

	createHandle := func() *gusset.Handle {
		h, err := gusset.Open(
			gusset.WithPoolSize(8),
			gusset.WithDiagnosticEngine(),
			gusset.WithMaxUncollected(1024),
		)
		if err != nil {
			panic(err)
		}
		return h
	}

	holder := &handleHolder{
		h: createHandle(),
	}

	const goroutines = 100
	const runDuration = 3 * time.Second

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	var submitSuccess atomic.Int64
	var submitClosed atomic.Int64
	var waitSuccess atomic.Int64
	var waitClosed atomic.Int64
	var discardCount atomic.Int64
	var abandonTimeoutCount atomic.Int64
	var handleSwapCount atomic.Int64

	mode16Input := []byte{16, 0x00, 0x20, 0x00, 0x00, 0x42}

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for {
				select {
				case <-stopCh:
					return
				default:
				}

				holder.mu.RLock()
				curH := holder.h
				holder.mu.RUnlock()

				if curH == nil {
					time.Sleep(1 * time.Millisecond)
					continue
				}

				action := (id + int(time.Now().UnixNano())) % 4

				switch action {
				case 0:
					sCtx, sCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
					tk, err := curH.Submit(sCtx, mode16Input)
					sCancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							submitClosed.Add(1)
						}
						continue
					}
					submitSuccess.Add(1)

					wCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					buf, err := curH.WaitBuffer(wCtx, tk)
					cancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) || errors.Is(err, gusset.ErrUnknownTicket) {
							waitClosed.Add(1)
						}
					} else {
						waitSuccess.Add(1)
						_ = buf.Free()
					}

				case 1:
					sCtx, sCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
					tk, err := curH.Submit(sCtx, mode16Input)
					sCancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							submitClosed.Add(1)
						}
						continue
					}
					submitSuccess.Add(1)

					if err := curH.Discard(tk); err == nil {
						discardCount.Add(1)
					}

				case 2:
					sCtx, sCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
					tk, err := curH.Submit(sCtx, mode16Input)
					sCancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							submitClosed.Add(1)
						}
						continue
					}
					submitSuccess.Add(1)

					expCtx, expCancel := context.WithTimeout(context.Background(), 1*time.Microsecond)
					time.Sleep(2 * time.Microsecond)
					_, err = curH.Wait(expCtx, tk)
					expCancel()
					if errors.Is(err, context.DeadlineExceeded) {
						abandonTimeoutCount.Add(1)
					}

				case 3:
					sCtx, sCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
					_, err := curH.Submit(sCtx, mode16Input)
					sCancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							submitClosed.Add(1)
						}
						continue
					}
					submitSuccess.Add(1)
				}
			}
		}(i)
	}

	churnStop := make(chan struct{})
	var churnWg sync.WaitGroup
	churnWg.Add(1)
	go func() {
		defer churnWg.Done()
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-churnStop:
				return
			case <-ticker.C:
				holder.mu.Lock()
				oldH := holder.h
				holder.h = createHandle()
				holder.mu.Unlock()

				handleSwapCount.Add(1)

				if oldH != nil {
					_ = oldH.Close()
				}
			}
		}
	}()

	time.Sleep(runDuration)

	close(churnStop)
	churnWg.Wait()

	close(stopCh)
	wg.Wait()

	holder.mu.Lock()
	if holder.h != nil {
		_ = holder.h.Close()
		holder.h = nil
	}
	holder.mu.Unlock()

	finalLive := gusset.Stats().LiveBytes
	t.Logf("Concurrency Stats: Submits=%d (Closed=%d), WaitBuffer=%d (Closed=%d), Discards=%d, AbandonTimeouts=%d, HandleSwaps=%d",
		submitSuccess.Load(), submitClosed.Load(), waitSuccess.Load(), waitClosed.Load(),
		discardCount.Load(), abandonTimeoutCount.Load(), handleSwapCount.Load())

	if finalLive != baselineLive {
		t.Fatalf("LEAK DETECTED after high-concurrency race test: expected LiveBytes to return to baseline %d, got %d (delta %d)",
			baselineLive, finalLive, int64(finalLive)-int64(baselineLive))
	}
}

// TestUncollected_MaxUncollectedBackpressureAndLimit verifies that WithMaxUncollected
// bounds uncollected completed results on a handle, applying backpressure and rejecting
// negative bounds with ErrBadArg.
func TestUncollected_MaxUncollectedBackpressureAndLimit(t *testing.T) {
	_, err := gusset.Open(gusset.WithMaxUncollected(-1))
	if err == nil {
		t.Fatal("expected Open(WithMaxUncollected(-1)) to fail, got nil")
	}
	var gussetErr *gusset.Error
	if !errors.As(err, &gussetErr) || gussetErr.Code != gusset.ErrBadArg.Code {
		t.Fatalf("expected ErrBadArg, got: %v", err)
	}

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

	var initialTickets []uint64
	for i := 0; i < maxUncollected; i++ {
		ticket, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatalf("Initial Submit %d failed: %v", i, err)
		}
		initialTickets = append(initialTickets, ticket)
	}

	time.Sleep(100 * time.Millisecond)

	// 1. Attempting 6th submission with a short timeout must block and fail with ErrTooManyUncollected.
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

	// 2. Asynchronously launch 50 submissions without calling Wait or Discard.
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

	// 3. Incremental unblocking via Discard() and Wait().
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

	time.Sleep(30 * time.Millisecond)
	select {
	case tk := <-submitDone:
		t.Fatalf("submitter completed prematurely before Discard: ticket %d", tk)
	default:
	}

	if err := h.Discard(initialTickets[0]); err != nil {
		t.Fatalf("Discard failed: %v", err)
	}

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

	time.Sleep(50 * time.Millisecond)

	res, err := h.Wait(context.Background(), initialTickets[1])
	if err != nil {
		t.Fatalf("Wait failed: %v", err)
	}
	if !bytes.Equal(res, in) {
		t.Fatalf("unexpected result from Wait: %v", res)
	}

	nextTicket, err := h.Submit(context.Background(), in)
	if err != nil {
		t.Fatalf("Submit after Wait failed: %v", err)
	}
	if nextTicket == 0 {
		t.Fatal("expected valid nextTicket, got 0")
	}

	_ = h.Discard(initialTickets[2])
	_ = h.Discard(initialTickets[3])
	_ = h.Discard(initialTickets[4])
	_ = h.Discard(newTicket)
	_ = h.Discard(nextTicket)
}

// TestBufferFree_ViewAndIDWithdrawalAndIdempotency verifies buffer post-Free lifecycle across
// Rust-allocated buffers, Go-heap inline buffers, and >4 KiB WaitBuffer outputs, plus
// double-Free idempotency and Free after Handle.Close().
func TestBufferFree_ViewAndIDWithdrawalAndIdempotency(t *testing.T) {
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

		for i := range slice {
			slice[i] = 0xAA
		}

		if err := buf.Free(); err != nil {
			t.Fatalf("buf.Free() failed for size %d: %v", size, err)
		}

		if buf.Bytes() != nil {
			t.Fatalf("expected buf.Bytes() == nil after Free for size %d", size)
		}
		if buf.ID() != 0 {
			t.Fatalf("expected buf.ID() == 0 after Free for size %d, got %d", size, buf.ID())
		}
		if _, err := h.Submit(context.Background(), buf); err == nil {
			t.Fatalf("size %d: expected Submit with freed buffer to fail", size)
		}
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
		if resBuf.ID() != 0 {
			t.Fatalf("expected resBuf.ID() == 0 after Free, got %d", resBuf.ID())
		}
		if _, err := h.Submit(context.Background(), resBuf); err == nil {
			t.Fatal("expected Submit with freed Go-heap buffer to fail")
		}
	}

	// 3. Large Rust output buffer returned by WaitBuffer (> 4096 bytes, Mode 16).
	{
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
		if largeBuf.ID() != 0 {
			t.Fatalf("expected largeBuf.ID() == 0 after Free, got %d", largeBuf.ID())
		}
		if _, err := h.Submit(context.Background(), largeBuf); err == nil {
			t.Fatal("expected Submit with freed large Rust buffer to fail")
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

		if err := h2.Close(); err != nil {
			t.Fatalf("h2.Close failed: %v", err)
		}

		if err := buf2.Free(); err != nil {
			t.Fatalf("buf2.Free after close returned error: %v", err)
		}
	}
}

// TestPanicFirewall_NonStringAndDoublePanicDestructor verifies that the panic
// firewall cleanly extracts non-string panic payloads (e.g. i32) and contains
// recursive destructor panics (DropBomb) across depths 1..5 without crashing the process.
func TestPanicFirewall_NonStringAndDoublePanicDestructor(t *testing.T) {
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

		_, err = h.Call(context.Background(), []byte{0, 1})
		if !errors.Is(err, gusset.ErrPoisoned) {
			t.Fatalf("expected ErrPoisoned after panic, got: %v", err)
		}
	}

	// Mode 15: DropBomb recursive destructor panic across depths 1..5.
	depths := []byte{1, 2, 3, 4, 5}
	for _, depth := range depths {
		h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open failed for depth %d: %v", depth, err)
		}

		_, err = h.Call(context.Background(), []byte{15, depth})
		if err == nil {
			_ = h.Close()
			t.Fatalf("expected panic error from Mode 15 (depth %d), got nil", depth)
		}
		if !errors.Is(err, gusset.ErrPanic) {
			_ = h.Close()
			t.Fatalf("expected ErrPanic for depth %d, got: %v", depth, err)
		}
		if !strings.Contains(err.Error(), "diagnostic payload destructor panicked") &&
			!strings.Contains(err.Error(), "Unknown Rust panic payload") {
			_ = h.Close()
			t.Fatalf("unexpected panic message for depth %d: %v", depth, err)
		}

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
