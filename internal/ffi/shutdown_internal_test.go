package ffi

import (
	"errors"
	"testing"
)

// A panic caught inside gusset_shutdown returns FFI_PANIC. Go mapped every
// non-OK code to the drain-budget message, so the panic surfaced as
// ErrShutdownIncomplete: "work still in flight" instead of "shutdown itself
// failed". The Rust side is covered by
// a_panic_inside_shutdown_is_not_reported_as_budget_expiry; the fault it
// injects is test-only, so this checks the mapping it feeds.
func TestShutdownError_PanicIsNotBudgetExpiry(t *testing.T) {
	if err := shutdownError(FFI_OK); err != nil {
		t.Fatalf("FFI_OK: got %v, want nil", err)
	}

	expired := shutdownError(FFI_ERR)
	if !errors.Is(expired, ErrShutdownIncomplete) {
		t.Fatalf("FFI_ERR must be ErrShutdownIncomplete, got %v", expired)
	}
	if errors.Is(expired, ErrPanic) {
		t.Fatalf("budget expiry must not match ErrPanic: %v", expired)
	}

	panicked := shutdownError(FFI_PANIC)
	if errors.Is(panicked, ErrShutdownIncomplete) {
		t.Fatalf("a shutdown panic must not read as budget expiry: %v", panicked)
	}
	if !errors.Is(panicked, ErrPanic) {
		t.Fatalf("a shutdown panic must match ErrPanic, got %v", panicked)
	}

	other := shutdownError(FFI_BAD_ARG)
	if other == nil || errors.Is(other, ErrShutdownIncomplete) || errors.Is(other, ErrPanic) {
		t.Fatalf("an unexpected code must be neither expiry nor panic, got %v", other)
	}
}
