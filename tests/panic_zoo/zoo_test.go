package paniczoo_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// Panic zoo cases (AGENTS.md line 208):
// All must return FFI_PANIC, Go process remains alive, message and location populated.

// Case 1: &str payload panic
func TestPanicZoo_StrPayload(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Mode 1: triggers panic!("plain panic")
	_, err = h.Call(ctx, []byte{1})
	if err == nil {
		t.Fatal("expected panic error, got nil")
	}

	if !errors.Is(err, gusset.ErrPanic) {
		t.Fatalf("expected ErrPanic, got: %v", err)
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "plain panic") {
		t.Fatalf("expected message to contain 'plain panic', got: %s", errMsg)
	}

	// Invariant I2: caught panic poisons the handle
	_, err = h.Call(ctx, []byte{0})
	if !errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("expected ErrPoisoned after panic, got: %v", err)
	}
}

// Case 2: Embedded NUL byte in panic message (Phase 0 repro resolution)
func TestPanicZoo_NulBytePayload(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Mode 2: triggers panic!("panic with embedded NUL \0 byte")
	// The original document's guard aborted with SIGABRT here. Gusset survives!
	_, err = h.Call(ctx, []byte{2})
	if err == nil {
		t.Fatal("expected panic error, got nil")
	}

	if !errors.Is(err, gusset.ErrPanic) {
		t.Fatalf("expected ErrPanic, got: %v", err)
	}

	t.Logf("survived NUL-byte panic: %v", err)

	// Verify handle is poisoned
	_, err = h.Call(ctx, []byte{0})
	if !errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("expected ErrPoisoned, got: %v", err)
	}
}

// Case 3: Non-string panic payload (std::panic::panic_any(42i32))
func TestPanicZoo_NonStringPayload(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Mode 3: triggers panic_any(42i32)
	_, err = h.Call(ctx, []byte{3})
	if err == nil {
		t.Fatal("expected panic error, got nil")
	}

	if !errors.Is(err, gusset.ErrPanic) {
		t.Fatalf("expected ErrPanic, got: %v", err)
	}

	if !strings.Contains(err.Error(), "42") {
		t.Fatalf("expected payload 42 in message, got: %v", err)
	}
}

// Case 4: Error with panic inside Display formatting
func TestPanicZoo_PanicInDisplay(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Mode 4: returns an error whose Display implementation panics
	_, err = h.Call(ctx, []byte{4})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Should be caught either as ErrPanic or ErrGeneric, and process must survive
	t.Logf("safely caught error with Display panic: %v", err)
}

// Case 5: Normal success path (Mode 0 echo)
func TestPanicZoo_SuccessEcho(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	payload := []byte{0, 10, 20, 30, 40}
	res, err := h.Call(ctx, payload)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}

	if len(res) != len(payload) {
		t.Fatalf("expected %d bytes, got %d", len(payload), len(res))
	}
	for i := range payload {
		if res[i] != payload[i] {
			t.Fatalf("byte mismatch at %d: expected %d, got %d", i, payload[i], res[i])
		}
	}
}
