package e2e_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestTier2_BufferAllocation_Limits validates enforcement of buffer length boundaries and copy limits.
func TestTier2_BufferAllocation_Limits(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	// Zero-length buffer
	_, errZero := h.NewBuffer(0)
	if !errors.Is(errZero, gusset.ErrBadArg) {
		t.Errorf("expected NewBuffer(0) to match ErrBadArg, got: %v", errZero)
	}

	// Negative-length buffer
	_, errNeg := h.NewBuffer(-1)
	if !errors.Is(errNeg, gusset.ErrBadArg) {
		t.Errorf("expected NewBuffer(-1) to match ErrBadArg, got: %v", errNeg)
	}

	// Exceeding MaxBufferBytes (1 GiB)
	_, errExceed := h.NewBuffer(gusset.MaxBufferBytes + 1)
	if !errors.Is(errExceed, gusset.ErrBadArg) {
		t.Errorf("expected NewBuffer(MaxBufferBytes+1) to match ErrBadArg, got: %v", errExceed)
	}

	// Inline payload exceeding 4096-byte copy limit
	ctx := context.Background()
	largeInline := make([]byte, 4097)
	_, errInline := h.Call(ctx, largeInline)
	if !errors.Is(errInline, gusset.ErrInputTooLarge) {
		t.Errorf("expected inline payload > 4096 to return ErrInputTooLarge, got: %v", errInline)
	}
}

// TestTier2_BufferBudget_Exhaustion_And_Reclaim validates WithBufferBudget enforcement and dynamic reclaim upon Free.
func TestTier2_BufferBudget_Exhaustion_And_Reclaim(t *testing.T) {
	const budget = 128 * 1024 // 128 KiB
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine(), gusset.WithBufferBudget(budget))
	requireNoError(t, err, "gusset.Open with budget")
	defer h.Close()

	// 1st allocation: 64 KiB (within budget)
	buf1, err := h.NewBuffer(64 * 1024)
	requireNoError(t, err, "1st 64 KiB buffer allocation")

	// 2nd allocation: 64 KiB (at budget ceiling)
	buf2, err := h.NewBuffer(64 * 1024)
	requireNoError(t, err, "2nd 64 KiB buffer allocation")

	// 3rd allocation: 1024 bytes (must be refused by budget)
	buf3, errExhaust := h.NewBuffer(1024)
	if !errors.Is(errExhaust, gusset.ErrBufferBudget) {
		t.Fatalf("expected ErrBufferBudget when budget exhausted, got: %v", errExhaust)
	}
	if buf3 != nil {
		buf3.Free()
		t.Fatal("expected nil buffer on budget exhaustion")
	}

	// Free buf1 to reclaim 64 KiB
	err = buf1.Free()
	requireNoError(t, err, "buf1.Free()")

	// Now 3rd allocation should succeed
	buf3, errRetry := h.NewBuffer(1024)
	requireNoError(t, errRetry, "allocating after freeing buf1")

	// Clean up remaining buffers
	_ = buf2.Free()
	_ = buf3.Free()
}

// TestTier2_OpenOptions_InvalidInputs validates constructor argument validation and ErrBadArg reporting.
func TestTier2_OpenOptions_InvalidInputs(t *testing.T) {
	// Nil Option
	_, errNilOpt := gusset.Open(nil)
	if !errors.Is(errNilOpt, gusset.ErrBadArg) {
		t.Errorf("expected Open(nil) to return ErrBadArg, got: %v", errNilOpt)
	}

	// Zero pool size
	_, errZeroPool := gusset.Open(gusset.WithPoolSize(0))
	if !errors.Is(errZeroPool, gusset.ErrBadArg) {
		t.Errorf("expected WithPoolSize(0) to return ErrBadArg, got: %v", errZeroPool)
	}

	// Negative pool size
	_, errNegPool := gusset.Open(gusset.WithPoolSize(-5))
	if !errors.Is(errNegPool, gusset.ErrBadArg) {
		t.Errorf("expected WithPoolSize(-5) to return ErrBadArg, got: %v", errNegPool)
	}

	// Pool size exceeding MaxPoolSize (1024)
	_, errMaxExceed := gusset.Open(gusset.WithPoolSize(gusset.MaxPoolSize + 1))
	if !errors.Is(errMaxExceed, gusset.ErrBadArg) {
		t.Errorf("expected WithPoolSize(MaxPoolSize+1) to return ErrBadArg, got: %v", errMaxExceed)
	}

	// Negative buffer budget
	_, errNegBudget := gusset.Open(gusset.WithBufferBudget(-100))
	if !errors.Is(errNegBudget, gusset.ErrBadArg) {
		t.Errorf("expected WithBufferBudget(-100) to return ErrBadArg, got: %v", errNegBudget)
	}
}

// TestTier2_Context_TimeoutAndCancellation tests deadline and cancellation propagation across interop.
func TestTier2_Context_TimeoutAndCancellation(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	// 1. Pre-cancelled context
	ctxPreCancelled, cancelPre := context.WithCancel(context.Background())
	cancelPre()
	_, errPre := h.Call(ctxPreCancelled, []byte{0})
	if !errors.Is(errPre, context.Canceled) {
		t.Errorf("expected context.Canceled on pre-cancelled call, got: %v", errPre)
	}

	// 2. Pre-expired deadline
	pastDeadline := time.Now().Add(-50 * time.Millisecond)
	ctxExpired, cancelExpired := context.WithDeadline(context.Background(), pastDeadline)
	defer cancelExpired()
	_, errExpired := h.Call(ctxExpired, []byte{0})
	if !errors.Is(errExpired, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded on expired call, got: %v", errExpired)
	}

	// 3. Dynamic cancellation during long execution
	// Mode 5 with input[1]=50 runs 50 iterations * 10ms = 500ms, checking ctx.check()
	ctxCancel, cancelDynamic := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancelDynamic()
	}()

	start := time.Now()
	_, errDyn := h.Call(ctxCancel, []byte{5, 50})
	elapsed := time.Since(start)

	if !errors.Is(errDyn, context.Canceled) {
		t.Errorf("expected context.Canceled on dynamic cancel, got: %v", errDyn)
	}
	if elapsed > 300*time.Millisecond {
		t.Errorf("cancellation took too long to abort worker: %v", elapsed)
	}
}

// TestTier2_TicketLifecycle_DoubleWait_Discard_Unknown verifies ticket state machine transitions.
func TestTier2_TicketLifecycle_DoubleWait_Discard_Unknown(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx := context.Background()

	// 1. Normal submission and wait
	ticket1, err := h.Submit(ctx, []byte{0, 1, 2, 3})
	requireNoError(t, err, "h.Submit ticket1")

	res1, err := h.Wait(ctx, ticket1)
	requireNoError(t, err, "h.Wait ticket1")
	if len(res1) != 4 {
		t.Fatalf("unexpected res1 len: %d", len(res1))
	}

	// 2. Double wait on ticket1 must return ErrUnknownTicket
	_, errDoubleWait := h.Wait(ctx, ticket1)
	if !errors.Is(errDoubleWait, gusset.ErrUnknownTicket) {
		t.Errorf("expected second Wait on ticket1 to return ErrUnknownTicket, got: %v", errDoubleWait)
	}

	// 3. Submit and discard
	ticket2, err := h.Submit(ctx, []byte{0, 4, 5, 6})
	requireNoError(t, err, "h.Submit ticket2")

	errDiscard := h.Discard(ticket2)
	requireNoError(t, errDiscard, "h.Discard ticket2")

	// Waiting on discarded ticket must fail with ErrUnknownTicket
	_, errWaitDiscarded := h.Wait(ctx, ticket2)
	if !errors.Is(errWaitDiscarded, gusset.ErrUnknownTicket) {
		t.Errorf("expected Wait on discarded ticket2 to return ErrUnknownTicket, got: %v", errWaitDiscarded)
	}

	// Discarding already discarded ticket
	errDoubleDiscard := h.Discard(ticket2)
	if !errors.Is(errDoubleDiscard, gusset.ErrUnknownTicket) {
		t.Errorf("expected second Discard to return ErrUnknownTicket, got: %v", errDoubleDiscard)
	}

	// 4. Non-existent ticket
	const nonExistentTicket = uint64(0xFEEDFACECAFE)
	_, errUnknownWait := h.Wait(ctx, nonExistentTicket)
	if !errors.Is(errUnknownWait, gusset.ErrUnknownTicket) {
		t.Errorf("expected Wait on random ticket to return ErrUnknownTicket, got: %v", errUnknownWait)
	}

	errUnknownDiscard := h.Discard(nonExistentTicket)
	if !errors.Is(errUnknownDiscard, gusset.ErrUnknownTicket) {
		t.Errorf("expected Discard on random ticket to return ErrUnknownTicket, got: %v", errUnknownDiscard)
	}
}

// TestTier2_NilArguments_Refusal validates that nil handle, nil context, and nil buffer are rejected with typed sentinels.
func TestTier2_NilArguments_Refusal(t *testing.T) {
	ctx := context.Background()

	// 1. Nil Handle receiver
	var nilH *gusset.Handle
	_, errNilHCall := nilH.Call(ctx, []byte{0})
	if !errors.Is(errNilHCall, gusset.ErrNilHandle) {
		t.Errorf("expected nil Handle.Call to return ErrNilHandle, got: %v", errNilHCall)
	}
	_, errNilHSubmit := nilH.Submit(ctx, []byte{0})
	if !errors.Is(errNilHSubmit, gusset.ErrNilHandle) {
		t.Errorf("expected nil Handle.Submit to return ErrNilHandle, got: %v", errNilHSubmit)
	}
	_, errNilHWait := nilH.Wait(ctx, 1)
	if !errors.Is(errNilHWait, gusset.ErrNilHandle) {
		t.Errorf("expected nil Handle.Wait to return ErrNilHandle, got: %v", errNilHWait)
	}
	_, errNilHNewBuf := nilH.NewBuffer(16)
	if !errors.Is(errNilHNewBuf, gusset.ErrNilHandle) {
		t.Errorf("expected nil Handle.NewBuffer to return ErrNilHandle, got: %v", errNilHNewBuf)
	}
	errNilHClose := nilH.Close()
	if !errors.Is(errNilHClose, gusset.ErrNilHandle) {
		t.Errorf("expected nil Handle.Close to return ErrNilHandle, got: %v", errNilHClose)
	}

	// 2. Open a valid handle for nil argument testing
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	// 3. Nil Context
	//lint:ignore SA1012 a nil context is the input under test
	_, errNilCtxCall := h.Call(nil, []byte{0})
	if !errors.Is(errNilCtxCall, gusset.ErrNilContext) {
		t.Errorf("expected Call(nil context) to return ErrNilContext, got: %v", errNilCtxCall)
	}
	//lint:ignore SA1012 a nil context is the input under test
	_, errNilCtxSubmit := h.Submit(nil, []byte{0})
	if !errors.Is(errNilCtxSubmit, gusset.ErrNilContext) {
		t.Errorf("expected Submit(nil context) to return ErrNilContext, got: %v", errNilCtxSubmit)
	}
	//lint:ignore SA1012 a nil context is the input under test
	_, errNilCtxWait := h.Wait(nil, 1)
	if !errors.Is(errNilCtxWait, gusset.ErrNilContext) {
		t.Errorf("expected Wait(nil context) to return ErrNilContext, got: %v", errNilCtxWait)
	}

	// 4. Nil Buffer
	var nilBuf *gusset.Buffer
	_, errNilBufCall := h.CallBuffer(ctx, nilBuf)
	if !errors.Is(errNilBufCall, gusset.ErrNilBuffer) {
		t.Errorf("expected CallBuffer(nil buffer) to return ErrNilBuffer, got: %v", errNilBufCall)
	}
	_, errNilBufSubmit := h.Submit(ctx, nilBuf)
	if !errors.Is(errNilBufSubmit, gusset.ErrNilBuffer) {
		t.Errorf("expected Submit(nil buffer) to return ErrNilBuffer, got: %v", errNilBufSubmit)
	}
}

// TestTier2_PanicContainment_And_HandlePoisoning verifies Invariant I2: Rust panics are trapped,
// poison only the invoking handle, and leave sibling handles fully operational.
func TestTier2_PanicContainment_And_HandlePoisoning(t *testing.T) {
	hA, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open Handle A")
	defer hA.Close()

	hB, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open Handle B")
	defer hB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Trigger panic on Handle A using diagnostic mode 1
	_, errPanic := hA.Call(ctx, []byte{1})
	if !errors.Is(errPanic, gusset.ErrPanic) {
		t.Fatalf("expected ErrPanic on Handle A, got: %v", errPanic)
	}

	// 2. Handle A must now be poisoned
	_, errPoisoned := hA.Call(ctx, []byte{0})
	if !errors.Is(errPoisoned, gusset.ErrPoisoned) {
		t.Fatalf("expected Handle A to return ErrPoisoned after panic, got: %v", errPoisoned)
	}

	// 3. Handle B must NOT be poisoned and must process calls normally
	resB, errB := hB.Call(ctx, []byte{0, 42})
	requireNoError(t, errB, "Handle B Call after Handle A poisoned")
	assertBytesEqual(t, resB, []byte{0, 42}, "Handle B response")
}

// TestTier2_UnregisteredOpcode_NoPoisoning verifies that an unknown dispatch opcode returns an error
// without poisoning the handle.
func TestTier2_UnregisteredOpcode_NoPoisoning(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx := context.Background()

	// Opcode 65432 has no registered engine
	ctxOp := gusset.ContextWithOpcode(ctx, 65432)
	_, errOp := h.Call(ctxOp, []byte{0})
	if errOp == nil {
		t.Fatal("expected error calling with unregistered opcode, got nil")
	}
	if !strings.Contains(errOp.Error(), "no engine handler registered for opcode") {
		t.Logf("error message for unregistered opcode: %v", errOp)
	}

	// Crucial check: handle must NOT be poisoned!
	resNormal, errNormal := h.Call(ctx, []byte{0, 99})
	requireNoError(t, errNormal, "Call with opcode 0 after unregistered opcode error")
	assertBytesEqual(t, resNormal, []byte{0, 99}, "normal echo response")
}
