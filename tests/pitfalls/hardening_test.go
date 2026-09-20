package pitfalls_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestHardening_UntrustedInputCannotSelectPanic is the headline regression.
//
// Gusset's built-in diagnostic engine picks its behaviour from input[0]: byte 1 is
// panic!("plain panic"), byte 2 panics with an embedded NUL, byte 3 is
// panic_any(42). That engine used to be the *default* whenever no adopter engine was
// registered in Rust — and no C export registers one, so any Go service linking
// libgusset.a before its Rust engine registered itself ran it.
//
// The consequence: the first byte of an untrusted payload selected a Rust panic,
// which poisoned the handle and failed every subsequent call. One attacker-chosen
// byte, and the handle is dead until the process restarts.
//
// A handle opened without WithDiagnosticEngine must now refuse every byte instead.
func TestHardening_UntrustedInputCannotSelectPanic(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Every possible first byte, including the panic selectors.
	for b := 0; b < 256; b++ {
		_, err := h.Call(ctx, []byte{byte(b), 0xAA, 0xBB})
		if err == nil {
			t.Fatalf("byte %d was executed by an implicit engine; submission must be refused", b)
		}
		if errors.Is(err, gusset.ErrPanic) {
			t.Fatalf("byte %d drove the engine into a panic (I2 breach via untrusted input)", b)
		}
		if !strings.Contains(err.Error(), "no engine handler registered") {
			t.Fatalf("byte %d: expected a refusal naming the missing engine, got: %v", b, err)
		}
	}

	// 256 refusals, and the handle is still healthy: a refusal is not a panic, so
	// it must not poison.
	if _, err := h.Call(ctx, []byte{1}); !strings.Contains(err.Error(), "no engine handler registered") {
		t.Fatalf("handle was poisoned by refusals; expected a plain refusal, got: %v", err)
	}
}

// TestHardening_DiagnosticEngineOptionExercisesBothSettings satisfies the working
// rule that every config option has a test covering both settings: the same input
// byte is refused without the option and executed with it.
func TestHardening_DiagnosticEngineOptionExercisesBothSettings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	off, err := gusset.Open(gusset.WithPoolSize(2))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer off.Close()

	if _, err := off.Call(ctx, []byte{0, 9, 9}); err == nil {
		t.Fatal("without WithDiagnosticEngine, the diagnostic echo must not run")
	}

	on, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer on.Close()

	out, err := on.Call(ctx, []byte{0, 9, 9})
	if err != nil {
		t.Fatalf("with WithDiagnosticEngine, mode 0 echo must run: %v", err)
	}
	if len(out) != 3 || out[1] != 9 || out[2] != 9 {
		t.Fatalf("diagnostic echo returned %v", out)
	}
}

// TestHardening_PoolSizeIsBounded pins the ceiling on worker threads.
//
// WithPoolSize fed straight through to thread::Builder::spawn with no bound, so
// WithPoolSize(1<<20) asked for a million OS threads with 8 MiB stacks each.
func TestHardening_PoolSizeIsBounded(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(gusset.MaxPoolSize + 1))
	if err == nil {
		_ = h.Close()
		t.Fatalf("pool size %d must be refused, not spawned", gusset.MaxPoolSize+1)
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("expected the error to name the ceiling, got: %v", err)
	}

	// An ordinary size still works: the bound must not break the normal path.
	ok, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("ordinary pool size rejected: %v", err)
	}
	defer ok.Close()
}

// Unknown CallHeader flag bits are rejected rather than silently dropped. Covered
// in tests/rust_runtime.rs (submit_rejects_unknown_header_flags): the public Go API
// deliberately exposes no way to set arbitrary flag bits, and adding a test-only
// accessor would put production surface in the package to serve a test.

// TestHardening_BufferBytesNotServedAfterRelease closes the window where Bytes()
// handed out a Go slice over Rust memory that had already been released.
//
// The Go garbage collector does not trace Rust memory, so nothing about holding the
// slice keeps the buffer alive or marks it invalid.
func TestHardening_BufferBytesNotServedAfterRelease(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	buf, err := h.NewBuffer(8192)
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}
	if got := buf.Bytes(); len(got) != 8192 {
		t.Fatalf("live buffer must expose its bytes, got %d", len(got))
	}

	if err := buf.Free(); err != nil {
		t.Fatalf("Free failed: %v", err)
	}
	if got := buf.Bytes(); got != nil {
		t.Fatalf("Bytes() must not serve released memory, got %d bytes", len(got))
	}

	// A buffer outlived by its handle is released when the handle closes, so the
	// same rule applies there.
	buf2, err := h.NewBuffer(4096)
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if got := buf2.Bytes(); got != nil {
		t.Fatalf("Bytes() must not serve memory released by Handle.Close, got %d bytes", len(got))
	}
}

// TestHardening_TraceContextPropagates covers R9's trace correlation.
//
// The header was read from ctx.Value("spanContext") — a bare string key, which go
// vet flags and which no real tracing library writes to, so trace_id and span_id
// were always zero and the code was unreachable in practice.
func TestHardening_TraceContextPropagates(t *testing.T) {
	ctx := context.WithValue(context.Background(), gusset.SpanContextKey, testSpan{})

	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Diagnostic mode 7 returns the header's trace_id followed by its span_id, so
	// this reads back what actually crossed the boundary.
	out, err := h.Call(ctx, []byte{7})
	if err != nil {
		t.Fatalf("call with a trace carrier failed: %v", err)
	}
	if len(out) != 24 {
		t.Fatalf("expected 16-byte trace id + 8-byte span id, got %d bytes", len(out))
	}
	if out[0] != 0xAB {
		t.Fatalf("trace id did not reach Rust: got %#v", out[:16])
	}
	if out[16] != 0xCD {
		t.Fatalf("span id did not reach Rust: got %#v", out[16:])
	}

	// A context with no carrier must leave the ids zeroed, not carry stale values.
	bare, cancelBare := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelBare()
	out2, err := h.Call(bare, []byte{7})
	if err != nil {
		t.Fatalf("call without a trace carrier failed: %v", err)
	}
	for i, b := range out2 {
		if b != 0 {
			t.Fatalf("expected a zeroed header without a carrier, byte %d = %d", i, b)
		}
	}
}

type testSpan struct{}

func (testSpan) TraceID() [16]byte {
	var b [16]byte
	b[0] = 0xAB
	return b
}

func (testSpan) SpanID() [8]byte {
	var b [8]byte
	b[0] = 0xCD
	return b
}
