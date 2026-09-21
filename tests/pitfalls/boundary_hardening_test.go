package pitfalls_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestHardening_OversizedOpcodeDoesNotCollapseToDiagnostic proves that a context
// opcode wider than uint32 is refused. Narrowing it with uint32(v) turns
// 1<<32 into 0, and opcode 0 is the diagnostic engine: payload byte 1 is a
// plain panic, so the truncation poisons the handle.
func TestHardening_OversizedOpcodeDoesNotCollapseToDiagnostic(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("uint is 32 bits wide; this narrowing only exists on 64-bit targets")
	}

	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	// uint(1<<32) does not fit in the header's u32. A truncating cast stores 0.
	huge := uint(uint64(1) << 32)
	ctx := context.WithValue(context.Background(), gusset.OpcodeContextKey, huge)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	_, err = h.Call(ctx, []byte{1})
	if errors.Is(err, gusset.ErrPanic) || errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("oversized opcode collapsed onto the diagnostic engine and panicked: %v", err)
	}
	if err == nil {
		t.Fatal("expected the opcode to be refused")
	}
	if !strings.Contains(err.Error(), "opcode") {
		t.Fatalf("expected an opcode refusal, got: %v", err)
	}

	echoCtx, echoCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer echoCancel()
	out, err := h.Call(echoCtx, []byte{0, 7})
	if err != nil {
		t.Fatalf("handle must stay usable after a refused opcode, got: %v", err)
	}
	if len(out) != 2 || out[1] != 7 {
		t.Fatalf("echo corrupted: %v", out)
	}
}
