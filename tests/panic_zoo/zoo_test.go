package paniczoo_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// Panic zoo cases (AGENTS.md "Panic zoo cases"):
// All must return FFI_PANIC, Go process remains alive, message and location populated.

// requireEnginePanic asserts err is the engine's own panic, caught by the
// engine firewall inside execute_unit and reported with exactly its text:
// "gusset error [2]: PANIC: <text> (at <file>:<line>)".
//
// The worker has a second, outer catch_unwind for everything around the engine
// call, and it words its result "worker fault outside the engine call: ...".
// With the inner firewall removed that outer one absorbs the same panic, still
// returns ErrPanic and still poisons, so only the exact text tells the two
// apart. Asserting no such prefix is what makes the inner firewall tested.
func requireEnginePanic(t *testing.T, err error, textPrefix string) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected panic error, got nil")
	}
	if !errors.Is(err, gusset.ErrPanic) {
		t.Fatalf("expected ErrPanic, got: %v", err)
	}
	msg := err.Error()
	if strings.Contains(msg, "worker fault") {
		t.Fatalf("panic was absorbed by the worker's outer catch_unwind, not the engine firewall: %s", msg)
	}
	const head = "gusset error [2]: PANIC: "
	if !strings.HasPrefix(msg, head+textPrefix) {
		t.Fatalf("panic text is not exactly %q followed by %q:\n%s", head, textPrefix, msg)
	}
	if !strings.Contains(msg, " (at crates/gusset/src/pool/diagnostic.rs:") {
		t.Fatalf("panic location missing or wrong: %s", msg)
	}
	return msg
}

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
	requireEnginePanic(t, err, "plain panic (at ")

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
	requireEnginePanic(t, err, "panic with embedded NUL \x00 byte (at ")

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
	requireEnginePanic(t, err, "panic payload (i32): 42 (at ")
}

// Case 3b: String payload (panic!("{}", s) boxes a String, not a &'static str).
// Mode 12 is a 1 MiB string, so the text is the truncated head of it.
func TestPanicZoo_StringPayload(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = h.Call(ctx, []byte{12})
	msg := requireEnginePanic(t, err, strings.Repeat("A", 1024))
	if !strings.Contains(msg, "... [truncated] (at ") {
		t.Fatalf("a 1 MiB String payload must be reported truncated, got tail %q", msg[len(msg)-80:])
	}
	if len(msg) > 65536 {
		t.Fatalf("message length %d is not bounded", len(msg))
	}

	_, err = h.Call(ctx, []byte{0})
	if !errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("expected ErrPoisoned after the String panic, got: %v", err)
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
	requireEnginePanic(t, err, "panic inside Display implementation (at ")
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
// permit. It must come back as ErrPanic at every re-throw depth. A wall-clock
// bound on that return could only false-fail under load.
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
		// No wall-clock bound. It could only false-fail under load. ErrPanic
		// is the behavioural assertion: a caller that waits out the engine
		// still fails it. The Close hang check below stays.

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
