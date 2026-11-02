package pitfalls_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
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

// TestPitfall_BytesAndFreeDoNotRace is the generational-reference check on the
// Go side of a buffer: Free invalidates the view, and Bytes must not observe
// the slice header concurrently with that invalidation. The race detector is
// the assertion. A passing run under -race is the result; a data race is a failure.
func TestPitfall_BytesAndFreeDoNotRace(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	const readers = 8
	const spins = 4000
	for round := 0; round < 40; round++ {
		buf, err := h.NewBuffer(256)
		if err != nil {
			t.Fatalf("round %d: NewBuffer: %v", round, err)
		}
		// Touch the view once so Free and Bytes contend on a live slice.
		if b := buf.Bytes(); len(b) != 256 {
			t.Fatalf("round %d: Bytes before the race: len %d", round, len(b))
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < readers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for j := 0; j < spins; j++ {
					// Length only. Indexing the slice would use Rust memory
					// Free may already have released, which is a crash rather
					// than the data race this test exists to catch.
					if len(buf.Bytes()) == 0 {
						return
					}
					_ = j
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = buf.Free()
		}()
		close(start)
		wg.Wait()

		if buf.Bytes() != nil {
			t.Fatalf("round %d: Bytes after Free returned a view", round)
		}
		if err := buf.Free(); err != nil {
			t.Fatalf("round %d: second Free: %v", round, err)
		}
	}
}

// TestPitfall_BoundaryRefusalsAreSentinels: nil-handle, nil-context,
// nil-buffer and over-the-copy-limit refusals were built inline with
// errors.New, so a caller could tell "input too large, use NewBuffer" from any
// other failure only by matching text — the problem the ErrClosed
// consolidation already fixed once. Each is now an exported sentinel, and the
// texts callers may already match are unchanged.
func TestPitfall_BoundaryRefusalsAreSentinels(t *testing.T) {
	ctx := context.Background()
	is := func(what string, err, want error, text string) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("%s: %v does not match %v", what, err, want)
		}
		if err.Error() != text {
			t.Fatalf("%s: text changed to %q, want %q", what, err.Error(), text)
		}
	}

	var nilHandle *gusset.Handle
	_, err := nilHandle.Call(ctx, nil)
	is("nil handle Call", err, gusset.ErrNilHandle, "gusset: handle is nil")
	_, err = nilHandle.Submit(ctx, nil)
	is("nil handle Submit", err, gusset.ErrNilHandle, "gusset: handle is nil")
	_, err = nilHandle.Wait(ctx, 1)
	is("nil handle Wait", err, gusset.ErrNilHandle, "gusset: handle is nil")
	_, err = nilHandle.NewBuffer(8)
	is("nil handle NewBuffer", err, gusset.ErrNilHandle, "gusset: handle is nil")

	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	//lint:ignore SA1012 a nil context is the input under test
	_, err = h.Call(nil, []byte{0})
	is("nil context Call", err, gusset.ErrNilContext, "gusset: nil context")
	//lint:ignore SA1012 a nil context is the input under test
	_, err = h.WaitBuffer(nil, 1)
	is("nil context WaitBuffer", err, gusset.ErrNilContext, "gusset: nil context")

	_, err = h.CallBuffer(ctx, nil)
	is("nil CallBuffer input", err, gusset.ErrNilBuffer, "gusset: buffer is nil")
	_, err = h.Submit(ctx, (*gusset.Buffer)(nil))
	is("nil Submit buffer", err, gusset.ErrNilBuffer, "gusset: buffer is nil")

	_, err = h.Call(ctx, make([]byte, 4097))
	is("4097-byte Call", err, gusset.ErrInputTooLarge,
		"gusset: []byte input exceeds 4096-byte copy limit; use NewBuffer")
	if errors.Is(err, gusset.ErrNilBuffer) || errors.Is(err, gusset.ErrClosed) {
		t.Fatalf("over-limit input matches an unrelated sentinel: %v", err)
	}

	_, err = h.Wait(ctx, 1<<62)
	is("unknown ticket", err, gusset.ErrUnknownTicket, "gusset: unknown or already-awaited ticket")
}

// TestPitfall_GoSideArgumentRefusalsAreBadArg pins the class of the refusals Go
// makes itself.
//
// The same caller mistake used to have two classes. A pool size over the
// ceiling or a buffer length of zero reached through the C ABI returned
// FFI_BAD_ARG, and the identical value passed through WithPoolSize or NewBuffer
// was refused by Go with a bare fmt.Errorf that matched nothing: a caller who
// wrote errors.Is(err, gusset.ErrBadArg) met the refusal only if the mistake was
// a nil Option. Each of the five Go-side refusals now matches ErrBadArg, is not
// also ErrGeneric, and keeps the text that names the problem (the substring
// tests TestPitfall_PoolSizeRefusalNamesTheActualProblem and
// TestPitfall_NegativeBufferBudgetIsRefused pin the wording).
func TestPitfall_GoSideArgumentRefusalsAreBadArg(t *testing.T) {
	check := func(t *testing.T, what string, err error, msg string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: accepted", what)
		}
		if !errors.Is(err, gusset.ErrBadArg) {
			t.Fatalf("%s: want errors.Is(err, ErrBadArg), got %T %v", what, err, err)
		}
		if errors.Is(err, gusset.ErrGeneric) {
			t.Fatalf("%s: matches ErrGeneric as well as ErrBadArg: %v", what, err)
		}
		var ge *gusset.Error
		if !errors.As(err, &ge) {
			t.Fatalf("%s: want an *Error, got %T", what, err)
		}
		if want := fmt.Sprintf("gusset error [%d]: ", gusset.ErrBadArg.Code); !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("%s: %q does not start with %q", what, err, want)
		}
		if !strings.Contains(err.Error(), msg) {
			t.Fatalf("%s: %q does not say %q", what, err, msg)
		}
	}

	t.Run("WithPoolSize below one", func(t *testing.T) {
		for _, n := range []int{0, -1} {
			h, err := gusset.Open(gusset.WithPoolSize(n), gusset.WithDiagnosticEngine())
			if h != nil {
				_ = h.Close()
			}
			check(t, fmt.Sprintf("WithPoolSize(%d)", n), err, "pool_size must be at least 1")
		}
	})

	t.Run("WithPoolSize over the ceiling", func(t *testing.T) {
		for _, n := range []int{gusset.MaxPoolSize + 1, 1 << 40} {
			h, err := gusset.Open(gusset.WithPoolSize(n), gusset.WithDiagnosticEngine())
			if h != nil {
				_ = h.Close()
			}
			check(t, fmt.Sprintf("WithPoolSize(%d)", n), err, "exceeds maximum")
		}
	})

	t.Run("WithBufferBudget negative", func(t *testing.T) {
		for _, n := range []int64{-1, math.MinInt64} {
			h, err := gusset.Open(gusset.WithBufferBudget(n), gusset.WithDiagnosticEngine())
			if h != nil {
				_ = h.Close()
			}
			check(t, fmt.Sprintf("WithBufferBudget(%d)", n), err, "buffer budget must not be negative")
		}
	})

	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	t.Run("NewBuffer of zero or less", func(t *testing.T) {
		for _, n := range []int{0, -1} {
			b, err := h.NewBuffer(n)
			if b != nil {
				_ = b.Free()
			}
			check(t, fmt.Sprintf("NewBuffer(%d)", n), err, "buffer size must be greater than zero")
		}
	})

	t.Run("NewBuffer over the ceiling", func(t *testing.T) {
		b, err := h.NewBuffer(gusset.MaxBufferBytes + 1)
		if b != nil {
			_ = b.Free()
		}
		check(t, "NewBuffer(MaxBufferBytes+1)", err, "exceeds maximum")
	})

	// The refusal is about the argument, not the handle: it neither poisons nor
	// closes it, and the next valid allocation works.
	b, err := h.NewBuffer(64)
	if err != nil {
		t.Fatalf("a valid NewBuffer after the refusals: %v", err)
	}
	_ = b.Free()
}

// TestPitfall_OpenNilOptionIsRefusedNotPanicked pins the nil-Option refusal.
//
// Open takes ...Option and Option is a func type, so Open(nil) compiles.
// handle.go called every Option unchecked, and a nil one panicked with a nil
// dereference before anything was allocated. It now returns an error matching
// ErrBadArg: not a skip, because an Option the caller meant to apply (a failed
// constructor's zero value) would then be dropped without a word.
func TestPitfall_OpenNilOptionIsRefusedNotPanicked(t *testing.T) {
	var zero gusset.Option
	cases := []struct {
		name string
		opts []gusset.Option
	}{
		{"Open(nil)", []gusset.Option{nil}},
		{"Open(WithPoolSize(2), nil)", []gusset.Option{gusset.WithPoolSize(2), nil}},
		{"Open(nil, WithPoolSize(2))", []gusset.Option{nil, gusset.WithPoolSize(2)}},
		{"a zero Option variable", []gusset.Option{zero}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				h   *gusset.Handle
				err error
			)
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("Open panicked on a nil Option: %v", r)
					}
				}()
				h, err = gusset.Open(tc.opts...)
			}()
			if err == nil {
				_ = h.Close()
				t.Fatal("Open accepted a nil Option")
			}
			if h != nil {
				t.Fatalf("a refused Open returned a handle alongside %v", err)
			}
			if !errors.Is(err, gusset.ErrBadArg) {
				t.Fatalf("a nil Option must match ErrBadArg, got %v", err)
			}
			if !strings.Contains(err.Error(), "nil Option") {
				t.Fatalf("the error does not name the problem: %v", err)
			}
		})
	}

	// The refusal is about the nil, not about Open: the same options without
	// it open.
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open without a nil Option: %v", err)
	}
	_ = h.Close()
}

// R6: Go pointers passed to Rust are valid only for the duration of the call.
//
// The enforcer AGENTS.md names is "the cgocheck2 job; the pinner test". The
// cgocheck2 job catches a Go pointer stored into non-Go memory; it cannot
// catch Rust keeping the address of the caller's slice in its own structures,
// because the pointer is a plain integer there. What does catch it is the
// caller giving the slice back while the job is still queued or running: if
// Rust kept the address, the engine reads whatever the Go heap holds there
// afterwards. No runtime.Pinner is used, so nothing keeps the slice in place
// for Rust, and the contract is that submit has copied it before it returns.
//
// The sizes cover each way the runtime stores an input: inline in the work
// unit (up to 56 bytes in all), a heap copy (up to 4 KiB), both edges of each.
func TestR6_SubmitCopiesTheInputBeforeItReturns(t *testing.T) {
	for _, n := range []int{1, 54, 55, 1024, 4094} {
		h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}

		// Mode 9 sleeps without looking at the input again, then dispatches on
		// the rest of it: the engine reads its bytes 150 ms after Submit
		// returned. The remainder is mode 0, which echoes it.
		want := make([]byte, n)
		want[0] = 0
		for i := 1; i < n; i++ {
			want[i] = byte(i*7 + 1)
		}
		in := append([]byte{9, 15}, want...)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ticket, err := h.Submit(ctx, in)
		if err != nil {
			cancel()
			h.Close()
			t.Fatalf("n=%d: Submit: %v", n, err)
		}

		// Give the slice back, and make its memory somebody else's: overwrite
		// it, drop it, collect, and fill the freed space with other data.
		for i := range in {
			in[i] = 0xEE
		}
		in = nil
		for i := 0; i < 3; i++ {
			runtime.GC()
		}
		junk := make([][]byte, 64)
		for i := range junk {
			junk[i] = bytes.Repeat([]byte{0xDD}, n+2)
		}
		runtime.KeepAlive(junk)

		out, err := h.Wait(ctx, ticket)
		cancel()
		h.Close()
		if err != nil {
			t.Fatalf("n=%d: Wait: %v", n, err)
		}
		if !bytes.Equal(out, want) {
			t.Fatalf("n=%d: the engine saw %d bytes that differ from what was submitted: "+
				"Rust kept the caller's pointer instead of copying the input (R6)", n, len(out))
		}
	}
}
