package gusset

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// R10 is enforced in two layers: Rust refuses a poisoned handle at the export,
// and Go latches handleState.poisoned and refuses before it enters Rust. The
// Rust layer answers ErrPoisoned (matched by code) for everything, so a test
// that only checks errors.Is(err, ErrPoisoned) passes with the Go layer gone.
// These read the Go latch directly, and tell the layers apart by their text:
// Go's is errHandlePoisoned ("...a job panicked; Close it and open a new
// handle"), Rust's is the bare "handle is poisoned".

const goPoisonText = "a job panicked; Close it and open a new handle"

// The Go latch is set by the reader goroutine off the take error of a panicked
// job, before the caller sees ErrPanic. Nothing else sets it on this path (the
// submit path latches only when Rust itself refused a submit).
func TestPoison_GoLatchIsSetByThePanickedJobsCompletion(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if h.state.poisoned.Load() {
		t.Fatal("a fresh handle is poisoned")
	}
	if _, err := h.Call(ctx, []byte{1}); !errors.Is(err, ErrPanic) {
		t.Fatalf("mode 1: want ErrPanic, got %v", err)
	}
	if !h.state.poisoned.Load() {
		t.Fatal("ErrPanic was delivered but the Go poison latch is clear (R10/I2)")
	}

	// The next submit is answered by Go itself, never reaching Rust.
	_, err = h.Submit(ctx, []byte{0})
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("want ErrPoisoned, got %v", err)
	}
	if !strings.Contains(err.Error(), goPoisonText) {
		t.Fatalf("the refusal came from Rust, not the Go layer: %v", err)
	}
}

// With the latch set and Rust's own state untouched (the handle is healthy
// there), Go must still refuse on its own: this is the Go layer with the Rust
// check out of the picture.
func TestPoison_GoLayerRefusesWithoutEnteringRust(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h.state.poisoned.Store(true)
	for name, call := range map[string]func() error{
		"Submit": func() error { _, err := h.Submit(ctx, []byte{0}); return err },
		"Call":   func() error { _, err := h.Call(ctx, []byte{0}); return err },
	} {
		err := call()
		if !errors.Is(err, ErrPoisoned) || !strings.Contains(err.Error(), goPoisonText) {
			t.Fatalf("%s on a Go-latched handle: want the Go ErrPoisoned text, got %v", name, err)
		}
	}
	if n := len(h.state.sem); n != 0 {
		t.Fatalf("a refused submit leaked %d pool permit(s)", n)
	}

	// Rust never saw the refused jobs, so a healthy handle with the latch
	// cleared works again.
	h.state.poisoned.Store(false)
	out, err := h.Call(ctx, []byte{0, 7})
	if err != nil || len(out) != 2 || out[1] != 7 {
		t.Fatalf("echo after clearing the latch: %v %v", out, err)
	}
}
