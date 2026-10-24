package gusset

// Argument refusals, from both sides of the boundary.
//
// DECISIONS.md 2026-10-24: the pool-level refusals of the caller's own
// arguments reach a C host as FFI_BAD_ARG rather than FFI_ERR, and Go reads
// FFI_BAD_ARG as errors.Is(err, ErrBadArg). Go validates the pool size and the
// buffer length before it calls in and never builds a bad descriptor or flag
// word, so the public API cannot reach the Rust refusals; these tests call the
// internal/ffi bindings directly, as a host with its own arguments would.

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// Open takes ...Option and Option is a func type, so nil compiles. handle.go
// called it unchecked and a nil Option panicked with a nil dereference.
func TestOpen_NilOptionIsRefusedNotPanicked(t *testing.T) {
	var zero Option
	cases := []struct {
		name string
		opts []Option
	}{
		{"Open(nil)", []Option{nil}},
		{"Open(WithPoolSize(2), nil)", []Option{WithPoolSize(2), nil}},
		{"Open(nil, WithPoolSize(2))", []Option{nil, WithPoolSize(2)}},
		{"a zero Option variable", []Option{zero}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				h   *Handle
				err error
			)
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("Open panicked on a nil Option: %v", r)
					}
				}()
				h, err = Open(tc.opts...)
			}()
			if err == nil {
				_ = h.Close()
				t.Fatal("Open accepted a nil Option")
			}
			if h != nil {
				t.Fatalf("a refused Open returned a handle alongside %v", err)
			}
			if !errors.Is(err, ErrBadArg) {
				t.Fatalf("a nil Option must match ErrBadArg, got %v", err)
			}
			if !strings.Contains(err.Error(), "nil Option") {
				t.Fatalf("the error does not name the problem: %v", err)
			}
		})
	}

	// The refusal is about the nil, not about Open: the same options without
	// it open.
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open without a nil Option: %v", err)
	}
	_ = h.Close()
}

func TestFFI_ArgumentRefusalsAreBadArg(t *testing.T) {
	wantBadArg := func(t *testing.T, what string, err error, msg string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: accepted", what)
		}
		if !errors.Is(err, ErrBadArg) {
			t.Fatalf("%s: want ErrBadArg, got %v", what, err)
		}
		if errors.Is(err, ErrGeneric) {
			t.Fatalf("%s: matches ErrGeneric as well as ErrBadArg: %v", what, err)
		}
		if !strings.Contains(err.Error(), msg) {
			t.Fatalf("%s: message %q does not contain %q", what, err, msg)
		}
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	wfd := int(w.Fd())

	t.Run("pool size over the ceiling", func(t *testing.T) {
		_, err := ffi.HandleOpen(MaxPoolSize+1, wfd)
		wantBadArg(t, "HandleOpen(MaxPoolSize+1)", err, "exceeds maximum")
		// A refusal leaves the descriptor the caller's.
		if _, err := syscall.Write(wfd, []byte{0}); err != nil {
			t.Fatalf("a refused open took the caller's descriptor: %v", err)
		}
	})

	t.Run("negative descriptor", func(t *testing.T) {
		_, err := ffi.HandleOpen(1, -1)
		wantBadArg(t, "HandleOpen(1, -1)", err, "non-negative")
	})

	t.Run("non-FIFO descriptor", func(t *testing.T) {
		null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer null.Close()
		_, err = ffi.HandleOpen(1, int(null.Fd()))
		wantBadArg(t, "HandleOpen on /dev/null", err, "not the write end")
	})

	t.Run("read end of a pipe", func(t *testing.T) {
		_, err := ffi.HandleOpen(1, int(r.Fd()))
		wantBadArg(t, "HandleOpen on a read end", err, "not the write end")
	})

	// The rest need a live handle.
	h, _, _ := rawRingHandle(t)

	t.Run("unknown flag bits", func(t *testing.T) {
		header := ffi.CallHeader{Flags: ffi.FlagDiagnosticEngine | 1<<31}
		_, err := ffi.Submit(h, header, []byte{0}, 0)
		wantBadArg(t, "Submit with an unknown flag bit", err, "unknown CallHeader flag")
	})

	t.Run("buffer length zero", func(t *testing.T) {
		_, _, err := ffi.BufAlloc(h, 0)
		wantBadArg(t, "BufAlloc(0)", err, "greater than zero")
	})

	t.Run("buffer length over the ceiling", func(t *testing.T) {
		_, _, err := ffi.BufAlloc(h, MaxBufferBytes+1)
		wantBadArg(t, "BufAlloc(MaxBufferBytes+1)", err, "exceeds maximum")
	})

	// The change is the enumerated refusals and nothing more. A refusal that
	// is not the caller's argument stays FFI_ERR: an input buffer id that does
	// not exist is neither a flag nor a length.
	t.Run("an unknown input buffer id stays ErrGeneric", func(t *testing.T) {
		header := ffi.CallHeader{Flags: ffi.FlagDiagnosticEngine}
		_, err := ffi.Submit(h, header, nil, 1<<62)
		if err == nil {
			t.Fatal("Submit accepted an unknown buffer id")
		}
		if !errors.Is(err, ErrGeneric) || errors.Is(err, ErrBadArg) {
			t.Fatalf("want ErrGeneric and not ErrBadArg, got %v", err)
		}
	})
}
