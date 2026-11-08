package gusset

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// I6/R12: the ABI check is the only guard between a drifted header and a
// corrupted Go heap. init() runs it once, so this table drives checkABI with
// every kind of drift and requires an error for each; a check that was deleted
// or weakened fails here, not silently at the next struct write.
func TestCheckABI_RefusesEveryDrift(t *testing.T) {
	base := gatherABIReport()
	if err := checkABI(base); err != nil {
		t.Fatalf("the live report must pass its own check: %v", err)
	}

	type abiCase struct {
		name   string
		want   string
		mutate func(r *abiReport)
	}
	var cases []abiCase
	add := func(name, want string, mutate func(r *abiReport)) {
		cases = append(cases, abiCase{name, want, mutate})
	}

	add("version+1", "version mismatch", func(r *abiReport) { r.rust.Version++ })
	add("version-1", "version mismatch", func(r *abiReport) { r.rust.Version-- })
	for i, n := range ffi.AbiTypeNames {
		i, n := i, n
		add("rust size "+n, "size/alignment mismatch", func(r *abiReport) { r.rust.Sizes[i]++ })
		add("rust align "+n, "size/alignment mismatch", func(r *abiReport) { r.rust.Aligns[i] *= 2 })
		// Rust agrees with the Go constants but cgo's compiled header does not.
		add("cgo size "+n, "layout drift for "+n, func(r *abiReport) { r.localSizes[i]++ })
		add("cgo align "+n, "layout drift for "+n, func(r *abiReport) { r.localAlign[i] *= 2 })
	}
	add("field count short", "field count mismatch", func(r *abiReport) { r.rustCount = ffi.AbiFieldCount - 1 })
	add("field count long", "field count mismatch", func(r *abiReport) { r.rustCount = ffi.AbiFieldCount + 1 })

	// Two equal-width fields trade places: every struct size and alignment is
	// unchanged, only the offsets move. FfiStatus.msg and FfiStatus.file are
	// both pointers.
	add("equal-width field swap", "ABI field FfiStatus.", func(r *abiReport) {
		a, b := fieldIndex(t, "FfiStatus.msg"), fieldIndex(t, "FfiStatus.file")
		if r.rustFields[1][a] != r.rustFields[1][b] || r.rustFields[0][a] == r.rustFields[0][b] {
			t.Fatalf("test premise broken: fields must be equal width at different offsets")
		}
		r.rustFields[0][a], r.rustFields[0][b] = r.rustFields[0][b], r.rustFields[0][a]
	})
	for i, name := range ffi.AbiFieldNames {
		i, name := i, name
		add("field offset "+name, "ABI field "+name+" drifted", func(r *abiReport) { r.rustFields[0][i]++ })
		add("field size "+name, "ABI field "+name+" drifted", func(r *abiReport) { r.localFlds[1][i]++ })
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			err := checkABI(r)
			if err == nil {
				t.Fatalf("checkABI accepted a drifted report")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the drift (want substring %q)", err, tc.want)
			}
		})
	}
}

// init() must have run the check, not merely have it available to call: the
// report it verified is kept, and a package that never ran the check leaves it
// zero. This is what fails if the ABI checks are removed from init (I6/R12).
func TestInit_RanTheABICheckAgainstTheLiveLibrary(t *testing.T) {
	if abiAtInit.rust.Version != ExpectedAbiVersion {
		t.Fatalf("init did not verify an ABI report: version %d, want %d", abiAtInit.rust.Version, ExpectedAbiVersion)
	}
	if !abiAtInit.verified {
		t.Fatal("init stored an ABI report that mustCheckABI never passed: the load-time check did not run (I6/R12)")
	}
	fresh := gatherABIReport()
	fresh.verified = true
	if fresh != abiAtInit {
		t.Fatalf("the report init verified differs from the live library's now:\n init  %+v\n live  %+v", abiAtInit, fresh)
	}
}

// What init verified covers every layer the check compares: all four structs,
// all named fields, both Rust's and cgo's side.
func TestInit_VerifiedReportCoversEveryStructAndField(t *testing.T) {
	r := abiAtInit
	for i, n := range ffi.AbiTypeNames {
		if r.rust.Sizes[i] == 0 || r.rust.Aligns[i] == 0 || r.localSizes[i] == 0 || r.localAlign[i] == 0 {
			t.Errorf("init verified no layout for %s", n)
		}
	}
	if r.rustCount != ffi.AbiFieldCount {
		t.Errorf("init verified %d fields, want %d", r.rustCount, ffi.AbiFieldCount)
	}
	for i, n := range ffi.AbiFieldNames {
		if r.rustFields[1][i] == 0 || r.localFlds[1][i] == 0 {
			t.Errorf("init verified no size for field %s", n)
		}
	}
	if err := checkABI(r); err != nil {
		t.Errorf("the report init accepted fails the check now: %v", err)
	}
}

// init panics on drift rather than logging and carrying on.
func TestMustCheckABI_PanicsOnDriftAndNamesIt(t *testing.T) {
	mustCheckABI(abiAtInit) // a clean report does not panic

	drifted := abiAtInit
	drifted.rust.Version++
	defer func() {
		v := recover()
		msg, _ := v.(string)
		if !strings.Contains(msg, "ABI version mismatch") {
			t.Fatalf("want a panic naming the version mismatch, got %v", v)
		}
	}()
	mustCheckABI(drifted)
	t.Fatal("mustCheckABI accepted a drifted report")
}

func fieldIndex(t *testing.T, name string) int {
	t.Helper()
	for i, n := range ffi.AbiFieldNames {
		if n == name {
			return i
		}
	}
	t.Fatalf("no ABI field named %q", name)
	return -1
}

// Argument refusals, from both sides of the boundary.
//
// DECISIONS.md 2026-10-24: the pool-level refusals of the caller's own
// arguments reach a C host as FFI_BAD_ARG rather than FFI_ERR, and Go reads
// FFI_BAD_ARG as errors.Is(err, ErrBadArg). Go validates the pool size and the
// buffer length before it calls in and never builds a bad descriptor or flag
// word, so the public API cannot reach the Rust refusals; these tests call the
// internal/ffi bindings directly, as a host with its own arguments would.
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

// The budget crosses the ABI as a uint32 of milliseconds. A value past MaxDrain
// must clamp, not wrap: 2^32 ms + 5 ms would otherwise cross as a 5 ms drain,
// and a caller that asked for "long" gets a drain that gives up at once.
func TestDrainMillis_ClampsInsteadOfWrapping(t *testing.T) {
	maxMS := uint32(MaxDrain / time.Millisecond)
	if maxMS != math.MaxUint32 {
		t.Fatalf("MaxDrain is %d ms, want the full uint32 range", maxMS)
	}
	cases := []struct {
		name string
		in   time.Duration
		want uint32
	}{
		{"zero", 0, 0},
		{"negative one ns", -1, 0},
		{"negative hour", -time.Hour, 0},
		{"min duration", time.Duration(math.MinInt64), 0},
		{"sub-millisecond", 999 * time.Microsecond, 0},
		{"one second", time.Second, 1000},
		{"exactly MaxDrain", MaxDrain, math.MaxUint32},
		{"MaxDrain minus 1ms", MaxDrain - time.Millisecond, math.MaxUint32 - 1},
		{"MaxDrain plus 1ms wraps to 0 unclamped", MaxDrain + time.Millisecond, math.MaxUint32},
		{"2^32 ms plus 5 ms wraps to 5 unclamped", MaxDrain + 6*time.Millisecond, math.MaxUint32},
		{"max duration", time.Duration(math.MaxInt64), math.MaxUint32},
	}
	for _, tc := range cases {
		if got := drainMillis(tc.in); got != tc.want {
			t.Errorf("%s: drainMillis(%v) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
}

// Submit takes []byte, *Buffer or nil. Anything else must be refused with an
// error that names the accepted types, before a permit is taken or Rust is
// entered, and the handle must keep working.
func TestSubmit_RefusesUnsupportedInputTypes(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type namedBytes []byte
	b := []byte{0, 1}
	inputs := map[string]any{
		"int":               42,
		"string":            "hello",
		"[]int":             []int{1, 2},
		"array":             [2]byte{0, 1},
		"*[]byte":           &b,
		"named []byte type": namedBytes{0, 1},
		"struct":            struct{ X int }{1},
		"func":              func() {},
		"Buffer by value":   Buffer{},
	}
	for name, in := range inputs {
		ticket, err := h.Submit(ctx, in)
		if err == nil {
			t.Errorf("%s: Submit accepted an unsupported input (ticket %d)", name, ticket)
			continue
		}
		if ticket != 0 {
			t.Errorf("%s: refused Submit returned ticket %d", name, ticket)
		}
		if !strings.Contains(err.Error(), "[]byte or *Buffer") {
			t.Errorf("%s: error does not name the accepted types: %v", name, err)
		}
	}
	if n := len(h.state.sem); n != 0 {
		t.Fatalf("refused submits leaked %d pool permit(s)", n)
	}

	// A typed nil *Buffer is its own refusal, not the type error.
	var nilBuf *Buffer
	if _, err := h.Submit(ctx, nilBuf); err == nil || err != ErrNilBuffer {
		t.Fatalf("nil *Buffer: want ErrNilBuffer, got %v", err)
	}

	// The handle is not poisoned or wedged by the refusals.
	out, err := h.Call(ctx, []byte{0, 9})
	if err != nil || len(out) != 2 || out[1] != 9 {
		t.Fatalf("echo after refused inputs: %v %v", out, err)
	}
}
