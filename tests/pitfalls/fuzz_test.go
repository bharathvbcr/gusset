package pitfalls_test

// Fuzz targets for the FFI boundary.
//
// AGENTS.md listed a `fuzz` job naming `cargo fuzz run header` and a Go
// `FuzzStatus`, and no fuzz target of either kind existed. These use Go's native
// fuzzing, which needs no dependency — `cargo fuzz` would have required adding
// `libfuzzer-sys` and a nightly-only build, and the boundary these targets cover
// is precisely where untrusted bytes actually enter Gusset.
//
// The invariant under test is never "the output is correct" — an arbitrary payload
// has no correct output. It is that *the process survives and the caller is always
// answered*: no abort, no hang, no silent success on input the engine rejected.
//
// Run the corpus (fast, every `go test`):
//
//	go test ./tests/pitfalls/
//
// Run actual fuzzing:
//
//	go test -run '^$' -fuzz FuzzCallRefusesWithoutEngine -fuzztime 60s ./tests/pitfalls/

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// fuzzCallTimeout bounds every fuzz iteration.
//
// A hang is a failure mode this target exists to catch, so the deadline has to be
// enforced by the call itself rather than by the fuzzing engine's watchdog: the
// watchdog kills the process and reports a crash without saying which invariant
// broke, while an expired context here returns an ordinary error we can assert on.
const fuzzCallTimeout = 10 * time.Second

// FuzzCallRefusesWithoutEngine drives arbitrary payloads at a handle with no
// registered engine and no diagnostic flag.
//
// Every one must be refused, and the refusal must not poison: a refusal is not a
// panic. This is the fuzz-scale version of the 256-byte regression test, and it
// adds the length dimension that test does not cover — empty, single-byte, and
// payloads either side of the 4 KiB inline/shared threshold.
func FuzzCallRefusesWithoutEngine(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0})
	f.Add([]byte{1})    // diagnostic engine's plain panic selector
	f.Add([]byte{2})    // embedded-NUL panic selector
	f.Add([]byte{3})    // panic_any selector
	f.Add([]byte{6})    // deep recursion selector
	f.Add([]byte{0xFF}) // unmapped selector
	f.Add(bytes.Repeat([]byte{7}, 4095))
	f.Add(bytes.Repeat([]byte{7}, 4096))
	f.Add(bytes.Repeat([]byte{7}, 4097))

	h, err := gusset.Open(gusset.WithPoolSize(2))
	if err != nil {
		f.Fatalf("Open failed: %v", err)
	}
	f.Cleanup(func() { _ = h.Close() })

	f.Fuzz(func(t *testing.T, payload []byte) {
		ctx, cancel := context.WithTimeout(context.Background(), fuzzCallTimeout)
		defer cancel()

		_, err := h.Call(ctx, payload)
		if err == nil {
			t.Fatalf("payload of %d bytes executed with no engine registered", len(payload))
		}
		if errors.Is(err, gusset.ErrPanic) {
			t.Fatalf("payload of %d bytes reached an engine and panicked (I2)", len(payload))
		}
		if errors.Is(err, gusset.ErrPoisoned) {
			t.Fatalf("refusals poisoned the handle after %d bytes", len(payload))
		}
		// R16 refuses []byte above 4 KiB before the engine question is asked.
		if len(payload) > 4096 && strings.Contains(err.Error(), "copy limit") {
			return
		}
		if !strings.Contains(err.Error(), "no engine handler registered") {
			t.Fatalf("payload of %d bytes: expected a refusal, got %v", len(payload), err)
		}
	})
}

// FuzzDiagnosticEngineNeverAborts drives arbitrary payloads at the built-in
// diagnostic engine — the one that deliberately panics on selected first bytes.
//
// I2 says no Rust panic crosses the boundary. The strongest evidence is a fuzzer
// pointed straight at the modes designed to panic: every outcome must be a
// returned error, never a SIGABRT, and after a caught panic the handle must be
// poisoned rather than half-working.
//
// A poisoned handle cannot serve the next input, so the target reopens. That is
// the documented recovery path (I2: close and reopen), which this exercises
// thousands of times in a way no single test does. The handle is carried across
// inputs by diagFuzzer, which never leaves a closed one behind.
func FuzzDiagnosticEngineNeverAborts(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3})
	f.Add([]byte{1})
	f.Add([]byte{2})
	f.Add([]byte{3})
	f.Add([]byte{4})
	f.Add([]byte{5, 2})
	f.Add([]byte{6, 0xFF, 0xFF, 0xFF, 0xFF}) // depth is capped, must not overflow
	f.Add([]byte{7})
	f.Add([]byte{8})
	f.Add([]byte{10, 9, 9, 9})

	d := &diagFuzzer{open: func() (diagHandle, error) {
		h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
		if err != nil {
			return nil, err
		}
		return h, nil
	}}
	h, err := d.open()
	if err != nil {
		f.Fatalf("Open failed: %v", err)
	}
	d.h = h
	f.Cleanup(d.close)

	f.Fuzz(func(t *testing.T, payload []byte) { d.run(t, payload) })
}

// diagHandle is what the diagnostic fuzz target needs from a handle, so a test
// can plant a Close or an Open that fails.
type diagHandle interface {
	Call(ctx context.Context, in []byte) ([]byte, error)
	Close() error
}

// fatalReporter is the part of *testing.T one fuzz iteration reports through.
type fatalReporter interface {
	Helper()
	Fatalf(format string, args ...any)
}

// diagFuzzer carries FuzzDiagnosticEngineNeverAborts' handle from one input to
// the next. The target used to keep the handle in a closure variable and call
// Close, then Fatalf if Close or the reopen failed, so the variable kept the
// closed handle. The fuzzing engine then minimized against it, every input
// reported "gusset: handle is closed", and the first failure's own message was
// never printed (nightly 39112666661, 2026-10-18).
//
// A poisoned handle is now taken out of h before anything about it is checked,
// and the replacement is opened before anything is reported. h is nil only when
// that Open failed. The next input then opens first and reports Open's own
// error.
type diagFuzzer struct {
	open func() (diagHandle, error)
	h    diagHandle
}

func (d *diagFuzzer) close() {
	if d.h != nil {
		_ = d.h.Close()
		d.h = nil
	}
}

func (d *diagFuzzer) run(t fatalReporter, payload []byte) {
	t.Helper()
	if d.h == nil {
		h, err := d.open()
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		d.h = h
	}
	ctx, cancel := context.WithTimeout(context.Background(), fuzzCallTimeout)
	defer cancel()

	// Reaching this line at all is most of the assertion: the previous
	// iteration's panic was caught rather than aborting the process.
	_, err := d.h.Call(ctx, payload)

	if errors.Is(err, gusset.ErrPoisoned) || errors.Is(err, gusset.ErrPanic) {
		old := d.h
		d.h = nil
		// A caught panic must poison; a poisoned handle must stay refused.
		_, again := old.Call(ctx, []byte{0})
		closeErr := old.Close()
		next, openErr := d.open()
		if openErr == nil {
			d.h = next
		}
		if !errors.Is(again, gusset.ErrPoisoned) {
			t.Fatalf("handle served work after a caught panic; expected ErrPoisoned, got %v", again)
		}
		if closeErr != nil {
			t.Fatalf("Close after poisoning failed: %v", closeErr)
		}
		if openErr != nil {
			t.Fatalf("reopen after poisoning failed: %v", openErr)
		}
		return
	}
	// Engine errors and cancellations are legitimate outcomes for arbitrary
	// input. A copy-limit refusal is R16, not a malformed FFI call.
	// ErrBadArg is not: it would mean the Go side built a malformed call
	// out of a well-formed payload that was otherwise legal to submit.
	if err != nil {
		if strings.Contains(err.Error(), "copy limit") {
			if len(payload) <= 4096 {
				t.Fatalf("copy-limit refusal for %d-byte payload (ceiling is 4096)", len(payload))
			}
			return
		}
		if errors.Is(err, gusset.ErrBadArg) {
			t.Fatalf("payload of %d bytes produced ErrBadArg: %v", len(payload), err)
		}
		if !errors.Is(err, context.DeadlineExceeded) &&
			!errors.Is(err, gusset.ErrGeneric) &&
			!strings.Contains(err.Error(), "cancel") {
			t.Fatalf("payload of %d bytes produced an unexpected error: %v", len(payload), err)
		}
	}
}

// plantedHandle stands in for a diagnostic handle: payload [1] panics and
// poisons it, a closed one refuses with ErrClosed, and Close returns closeErr.
type plantedHandle struct {
	poisoned, closed bool
	closeErr         error
}

func (p *plantedHandle) Call(_ context.Context, in []byte) ([]byte, error) {
	switch {
	case p.closed:
		return nil, gusset.ErrClosed
	case p.poisoned:
		return nil, gusset.ErrPoisoned
	case len(in) > 0 && in[0] == 1:
		p.poisoned = true
		return nil, gusset.ErrPanic
	}
	return in, nil
}

func (p *plantedHandle) Close() error {
	p.closed = true
	return p.closeErr
}

// recordingReporter keeps what an iteration reported. Fatalf ends the
// iteration's goroutine as testing.T's does.
type recordingReporter struct{ msgs []string }

func (r *recordingReporter) Helper() {}

func (r *recordingReporter) Fatalf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
	runtime.Goexit()
}

func iterate(d *diagFuzzer, payload []byte) []string {
	rec := &recordingReporter{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.run(rec, payload)
	}()
	<-done
	return rec.msgs
}

// A failure on the poison path reports its own message, and the next input,
// which is also what the minimizer reruns, gets a live handle. Before
// diagFuzzer, each of these cases reported the planted failure once and then
// "gusset: handle is closed" for every later input.
func TestFuzzDiagnosticTarget_FailureDoesNotStrandAClosedHandle(t *testing.T) {
	plantedClose := errors.New("planted close failure")
	plantedOpen := errors.New("planted open failure")
	cases := []struct {
		name  string
		first *plantedHandle
		opens []error // the result of each Open after the first handle, in order
		want  string
	}{
		{"close fails", &plantedHandle{closeErr: plantedClose}, []error{nil, nil}, "Close after poisoning failed: planted close failure"},
		{"reopen fails", &plantedHandle{}, []error{plantedOpen, nil, nil}, "reopen after poisoning failed: planted open failure"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := 0
			d := &diagFuzzer{h: c.first, open: func() (diagHandle, error) {
				if n >= len(c.opens) {
					return nil, fmt.Errorf("Open #%d was not planned", n+2)
				}
				err := c.opens[n]
				n++
				if err != nil {
					return nil, err
				}
				return &plantedHandle{}, nil
			}}
			if got := iterate(d, []byte{1}); len(got) != 1 || got[0] != c.want {
				t.Fatalf("poisoning input reported %q; want only %q", got, c.want)
			}
			if !c.first.closed {
				t.Error("the poisoned handle was not closed")
			}
			// A later input, the minimizer's rerun of the poisoning one, and
			// another later input: none may meet a closed handle.
			for _, in := range [][]byte{{0}, {1}, {0}} {
				if got := iterate(d, in); len(got) != 0 {
					t.Errorf("input % x after the failure reported %q; want a live handle", in, got)
				}
			}
			if n != len(c.opens) {
				t.Errorf("%d reopens ran; want %d", n, len(c.opens))
			}
		})
	}

	// An Open that keeps failing is reported as itself on every input.
	d := &diagFuzzer{open: func() (diagHandle, error) { return nil, plantedOpen }}
	for range 2 {
		if got := iterate(d, []byte{0}); len(got) != 1 || got[0] != "Open failed: planted open failure" {
			t.Fatalf("with Open failing, an input reported %q; want only the Open failure", got)
		}
	}
}

// FuzzBufferLifecycle fuzzes the Rust-owned buffer path (I1, R4, R16).
//
// Lengths come from the fuzzer, so this covers zero, one, and allocations either
// side of the alignment and inline thresholds. Every buffer must be freed by the
// allocator that made it, and a slice must not outlive its Buffer.
func FuzzBufferLifecycle(f *testing.F) {
	f.Add(uint32(0))
	f.Add(uint32(1))
	f.Add(uint32(63))
	f.Add(uint32(64))
	f.Add(uint32(65))
	f.Add(uint32(4096))
	f.Add(uint32(1 << 20))

	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		f.Fatalf("Open failed: %v", err)
	}
	f.Cleanup(func() { _ = h.Close() })

	f.Fuzz(func(t *testing.T, size uint32) {
		// Bound the request: the fuzzer will happily ask for 4 GiB, and an OOM
		// kill proves nothing about Gusset.
		n := int(size % (1 << 20))

		buf, err := h.NewBuffer(n)
		if err != nil {
			// A refusal is a valid outcome (zero length is rejected by design);
			// it just must not be a panic or a poisoned handle.
			if errors.Is(err, gusset.ErrPanic) || errors.Is(err, gusset.ErrPoisoned) {
				t.Fatalf("NewBuffer(%d) panicked or poisoned: %v", n, err)
			}
			return
		}

		b := buf.Bytes()
		if len(b) != n {
			t.Fatalf("NewBuffer(%d) returned a %d byte slice", n, len(b))
		}
		if n > 0 {
			// Touch both ends: a wrong length or a short allocation shows up here
			// under the race detector and ASan rather than as silent corruption.
			// n == 1 aliases the two ends onto one byte, so the last write wins and
			// checking for 0xA5 there would fail against correct code.
			b[0] = 0xA5
			b[n-1] = 0x5A
			if b[n-1] != 0x5A {
				t.Fatalf("buffer of %d bytes did not retain the write at its last byte", n)
			}
			if n > 1 && b[0] != 0xA5 {
				t.Fatalf("buffer of %d bytes did not retain the write at its first byte", n)
			}
		}

		if err := buf.Free(); err != nil {
			t.Fatalf("Free(%d) failed: %v", n, err)
		}
		// R4/I1: after Free the slice must not be handed back out.
		if got := buf.Bytes(); got != nil {
			t.Fatalf("Bytes() returned %d bytes after Free; freed Rust memory must not be reachable", len(got))
		}
		// Free is idempotent by design: `freed.Swap(true)` returns before reaching
		// Rust, so `defer buf.Free()` alongside an explicit Free is safe rather
		// than a double free. The invariant is that the second call is a no-op —
		// it must neither error nor release the same buffer id twice.
		if err := buf.Free(); err != nil {
			t.Fatalf("second Free(%d) returned %v; Free must be idempotent so that "+
				"defer-plus-explicit-Free is safe", n, err)
		}
		if got := buf.Bytes(); got != nil {
			t.Fatalf("Bytes() returned %d bytes after a second Free", len(got))
		}
	})
}
