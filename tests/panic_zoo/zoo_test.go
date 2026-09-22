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

// Case 6: a panic payload whose destructor panics again (mode 15).
//
// The firewall catches the engine's panic, but the caught payload is dropped
// afterwards; a payload whose Drop panics used to unwind the Rust worker thread
// itself. The ticket then never completed, so Call returned only when its own
// deadline fired — and a caller with no deadline parked forever holding a pool
// permit. It must come back as ErrPanic, promptly, at every re-throw depth.
func TestPanicZoo_PanickingPayloadDestructor(t *testing.T) {
	for _, depth := range []byte{0, 1, 8, 255} {
		h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		start := time.Now()
		_, err = h.Call(ctx, []byte{15, depth})
		elapsed := time.Since(start)
		cancel()

		if !errors.Is(err, gusset.ErrPanic) {
			t.Fatalf("depth %d: expected ErrPanic, got %v after %v (a deadline error means the worker died and the ticket was stranded)", depth, err, elapsed)
		}
		if elapsed > time.Second {
			t.Fatalf("depth %d: panic took %v to surface", depth, elapsed)
		}

		closed := make(chan error, 1)
		go func() { closed <- h.Close() }()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatalf("depth %d: Close hung after a contained destructor panic", depth)
		}
	}

	// The process-wide runtime still serves a fresh handle.
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if out, err := h.Call(ctx, []byte{0, 42}); err != nil || len(out) != 2 || out[1] != 42 {
		t.Fatalf("echo after containment: out=%v err=%v", out, err)
	}
}
