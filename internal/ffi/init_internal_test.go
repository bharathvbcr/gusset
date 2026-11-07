package ffi

import (
	"errors"
	"strings"
	"testing"
)

// gusset_init has no status out-parameter, so a panic inside it reaches the
// host only as a line in the Rust log ring. Init used to return a constant
// message and leave that line in the ring. The Rust side is covered by
// a_panic_inside_init_is_reported_as_a_panic, which also checks the line lands
// in the ring; this checks that Init drains it once and surfaces the last line.
func TestInit_NonOKCodeSurfacesTheLastLogLine(t *testing.T) {
	origInit, origDrain := cInit, drainLogs
	t.Cleanup(func() { cInit, drainLogs = origInit, origDrain })

	drains := 0
	cInit = func() int { return FFI_PANIC }
	drainLogs = func(buf []byte) int {
		drains++
		return copy(buf, "gusset: earlier line\ngusset: init panicked: boom\n")
	}

	err := Init()
	if err == nil {
		t.Fatal("Init returned nil for a non-OK code")
	}
	if !strings.Contains(err.Error(), "gusset: init panicked: boom") {
		t.Fatalf("the panic text is missing from the error: %v", err)
	}
	if strings.Contains(err.Error(), "earlier line") {
		t.Fatalf("only the last line belongs in the error: %v", err)
	}
	if !errors.Is(err, ErrPanic) {
		t.Fatalf("FFI_PANIC must still match ErrPanic: %v", err)
	}
	if drains != 1 {
		t.Fatalf("the log ring must be drained exactly once, got %d", drains)
	}
}

func TestInit_NonOKCodeWithAnEmptyRingKeepsTheConstantMessage(t *testing.T) {
	origInit, origDrain := cInit, drainLogs
	t.Cleanup(func() { cInit, drainLogs = origInit, origDrain })

	cInit = func() int { return FFI_ERR }
	drainLogs = func([]byte) int { return 0 }

	err := Init()
	var e *Error
	if !errors.As(err, &e) || e.Msg != "initialization failed" || e.Code != FFI_ERR {
		t.Fatalf("got %v, want the constant message with code FFI_ERR", err)
	}
}

func TestInit_OKDoesNotDrain(t *testing.T) {
	origInit, origDrain := cInit, drainLogs
	t.Cleanup(func() { cInit, drainLogs = origInit, origDrain })

	cInit = func() int { return FFI_OK }
	drainLogs = func([]byte) int {
		t.Fatal("a successful init must leave the ring alone")
		return 0
	}
	if err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
}
