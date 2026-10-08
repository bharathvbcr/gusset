package pitfalls_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bharathvbcr/gusset"
)

// TestPitfall_BoundaryRefusalsAreSentinels: nil-handle, nil-context,
// nil-buffer and over-the-copy-limit refusals were built inline with
// errors.New, so a caller could tell "input too large, use NewBuffer" from any
// other failure only by matching text — the problem the ErrClosed
// consolidation already fixed once. Each is now an exported sentinel, and the
// texts callers may already match are unchanged.
func TestPitfall_BoundaryRefusalsAreSentinels(t *testing.T) {
	ctx := context.Background()
	is := func(what string, err, want error, text string) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("%s: %v does not match %v", what, err, want)
		}
		if err.Error() != text {
			t.Fatalf("%s: text changed to %q, want %q", what, err.Error(), text)
		}
	}

	var nilHandle *gusset.Handle
	_, err := nilHandle.Call(ctx, nil)
	is("nil handle Call", err, gusset.ErrNilHandle, "gusset: handle is nil")
	_, err = nilHandle.Submit(ctx, nil)
	is("nil handle Submit", err, gusset.ErrNilHandle, "gusset: handle is nil")
	_, err = nilHandle.Wait(ctx, 1)
	is("nil handle Wait", err, gusset.ErrNilHandle, "gusset: handle is nil")
	_, err = nilHandle.NewBuffer(8)
	is("nil handle NewBuffer", err, gusset.ErrNilHandle, "gusset: handle is nil")

	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	//lint:ignore SA1012 a nil context is the input under test
	_, err = h.Call(nil, []byte{0})
	is("nil context Call", err, gusset.ErrNilContext, "gusset: nil context")
	//lint:ignore SA1012 a nil context is the input under test
	_, err = h.WaitBuffer(nil, 1)
	is("nil context WaitBuffer", err, gusset.ErrNilContext, "gusset: nil context")

	_, err = h.CallBuffer(ctx, nil)
	is("nil CallBuffer input", err, gusset.ErrNilBuffer, "gusset: buffer is nil")
	_, err = h.Submit(ctx, (*gusset.Buffer)(nil))
	is("nil Submit buffer", err, gusset.ErrNilBuffer, "gusset: buffer is nil")

	_, err = h.Call(ctx, make([]byte, 4097))
	is("4097-byte Call", err, gusset.ErrInputTooLarge,
		"gusset: []byte input exceeds 4096-byte copy limit; use NewBuffer")
	if errors.Is(err, gusset.ErrNilBuffer) || errors.Is(err, gusset.ErrClosed) {
		t.Fatalf("over-limit input matches an unrelated sentinel: %v", err)
	}

	_, err = h.Wait(ctx, 1<<62)
	is("unknown ticket", err, gusset.ErrUnknownTicket, "gusset: unknown or already-awaited ticket")
}
