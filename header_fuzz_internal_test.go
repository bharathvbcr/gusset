package gusset

// Fuzzing of the CallHeader the Go side builds for every submission
// (callHeaderIDs and stampTimeout via extractCallHeader below,
// opcodeFromContext, readTraceCarrier).
//
// The opcode selects which Rust engine runs the call, so a value narrowed
// instead of refused routes work to the wrong engine (opcode 0 is the global
// handler); a carrier that panics on the caller's goroutine after submit has
// taken a pool permit leaks that permit (I4).

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

type (
	fzOp8   uint8
	fzOp16  uint16
	fzOp32  uint32
	fzOp64  uint64
	fzOpInt int
	fzOpI8  int8
	fzOpI64 int64
	fzOpPtr uintptr
	fzOpF   float64
	fzOpS   string
)

// opcodeTypes covers every reflect integer kind, named and unnamed, plus the
// non-integer kinds that must be refused.
var opcodeTypes = []reflect.Type{
	reflect.TypeFor[int](), reflect.TypeFor[int8](), reflect.TypeFor[int16](),
	reflect.TypeFor[int32](), reflect.TypeFor[int64](),
	reflect.TypeFor[uint](), reflect.TypeFor[uint8](), reflect.TypeFor[uint16](),
	reflect.TypeFor[uint32](), reflect.TypeFor[uint64](), reflect.TypeFor[uintptr](),
	reflect.TypeFor[fzOp8](), reflect.TypeFor[fzOp16](), reflect.TypeFor[fzOp32](),
	reflect.TypeFor[fzOp64](), reflect.TypeFor[fzOpInt](), reflect.TypeFor[fzOpI8](),
	reflect.TypeFor[fzOpI64](), reflect.TypeFor[fzOpPtr](),
	reflect.TypeFor[float32](), reflect.TypeFor[float64](), reflect.TypeFor[fzOpF](),
	reflect.TypeFor[string](), reflect.TypeFor[fzOpS](), reflect.TypeFor[bool](),
	reflect.TypeFor[*uint32](), reflect.TypeFor[[]byte](), reflect.TypeFor[complex128](),
}

// opcodeOracle is what opcodeFromContext must answer for rv, derived from
// the value's mathematical meaning rather than from the implementation.
func opcodeOracle(rv reflect.Value) (uint32, bool) {
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n := rv.Int()
		if n < 0 || n > math.MaxUint32 {
			return 0, false
		}
		return uint32(n), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n := rv.Uint()
		if n > math.MaxUint32 {
			return 0, false
		}
		return uint32(n), true
	}
	return 0, false
}

func makeOpcodeValue(kind uint8, raw uint64) any {
	typ := opcodeTypes[int(kind)%len(opcodeTypes)]
	rv := reflect.New(typ).Elem()
	switch typ.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		rv.SetInt(int64(raw) >> (64 - typ.Bits())) // sign-extended, full range of the type
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		rv.SetUint(raw >> (64 - typ.Bits()))
	case reflect.Float32, reflect.Float64:
		rv.SetFloat(float64(raw % 1000)) // integral floats are still not integers
	case reflect.String:
		rv.SetString(fmt.Sprint(raw))
	case reflect.Bool:
		rv.SetBool(raw&1 == 1)
	case reflect.Pointer:
		v := uint32(raw)
		rv.Set(reflect.ValueOf(&v))
	case reflect.Complex128:
		rv.SetComplex(complex(float64(raw%7), 0))
	}
	return rv.Interface()
}

// FuzzOpcodeFromContext: every integer kind within [0, MaxUint32] is accepted
// exactly; everything else is refused, never truncated.
func FuzzOpcodeFromContext(f *testing.F) {
	for k := range len(opcodeTypes) {
		for _, v := range []uint64{0, 1, 0xFFFFFFFF, 1 << 32, math.MaxUint64, 1 << 63} {
			f.Add(uint8(k), v, uint32(0))
		}
	}
	f.Fuzz(func(t *testing.T, kind uint8, raw uint64, def uint32) {
		v := makeOpcodeValue(kind, raw)
		got, err := opcodeFromContext(v)
		want, ok := opcodeOracle(reflect.ValueOf(v))
		if ok != (err == nil) || (ok && got != want) {
			t.Fatalf("opcodeFromContext(%T %v) = %d, %v; want %d ok=%v", v, v, got, err, want, ok)
		}
		if err != nil && got != 0 {
			t.Fatalf("refused %T %v but returned opcode %d", v, v, got)
		}

		// Through extractCallHeader: a refused opcode is an error, not the
		// handle default and not opcode 0.
		ctx := context.WithValue(context.Background(), OpcodeContextKey, v)
		h, herr := extractCallHeader(ctx, ffi.FlagInlineCompletion, def)
		if ok {
			if herr != nil || h.Reserved != want {
				t.Fatalf("header for %T %v: reserved %d err %v, want %d", v, v, h.Reserved, herr, want)
			}
		} else if herr == nil {
			t.Fatalf("header for %T %v accepted, reserved %d", v, v, h.Reserved)
		}
		if h.Flags != ffi.FlagInlineCompletion {
			t.Fatalf("flags %#x changed", h.Flags)
		}
	})
}

// panicCarrier panics from TraceID or SpanID with a fuzz-chosen value.
type panicCarrier struct {
	where int // 0: TraceID, 1: SpanID, 2: neither
	val   any
	trace [16]byte
	span  [8]byte
}

func (c *panicCarrier) TraceID() [16]byte {
	if c.where == 0 {
		panic(c.val)
	}
	return c.trace
}

func (c *panicCarrier) SpanID() [8]byte {
	if c.where == 1 {
		panic(c.val)
	}
	return c.span
}

// errorPanics is a panic value whose Error method itself panics, so formatting
// the recovered value is a second panic inside the recover path.
type errorPanics struct{}

func (errorPanics) Error() string { panic("Error() panicked") }

type stringerPanics struct{}

func (stringerPanics) String() string { panic(errors.New("String() panicked")) }

func panicValue(sel uint8, s string) any {
	switch sel % 9 {
	case 0:
		return s
	case 1:
		return len(s)
	case 2:
		return errors.New(s)
	case 3:
		return errorPanics{}
	case 4:
		return stringerPanics{}
	case 5:
		return nil // Go >= 1.21: *runtime.PanicNilError, recover() is non-nil
	case 6:
		return &errorPanics{}
	case 7:
		return []byte(s)
	default:
		return fmt.Errorf("wrapped: %w", context.Canceled)
	}
}

// FuzzReadTraceCarrier: a carrier that panics, with any panic value, becomes
// an error from extractCallHeader and never escapes; a well-behaved carrier's
// ids are copied exactly; a typed-nil carrier is an error, not a crash.
func FuzzReadTraceCarrier(f *testing.F) {
	for w := range 3 {
		for sel := range 9 {
			f.Add(uint8(w), uint8(sel), "boom", []byte{1, 2, 3}, uint32(7))
		}
	}
	f.Add(uint8(3), uint8(0), "", []byte{}, uint32(0)) // typed-nil carrier

	f.Fuzz(func(t *testing.T, where, sel uint8, msg string, ids []byte, op uint32) {
		var c *panicCarrier
		if where%4 != 3 {
			c = &panicCarrier{where: int(where % 4), val: panicValue(sel, msg)}
			copy(c.trace[:], ids)
			if len(ids) > 16 {
				copy(c.span[:], ids[16:])
			}
		}
		ctx := context.WithValue(context.Background(), SpanContextKey, TraceCarrier(c))
		ctx = ContextWithOpcode(ctx, op)
		var h ffi.CallHeader
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic escaped extractCallHeader: %v", r)
				}
			}()
			h, err = extractCallHeader(ctx, 0, 0)
		}()
		switch {
		case c == nil || c.where < 2:
			if err == nil || !strings.Contains(err.Error(), "trace carrier") {
				t.Fatalf("panicking carrier: err %v", err)
			}
		default:
			if err != nil {
				t.Fatalf("well-behaved carrier: %v", err)
			}
			if h.TraceID != c.trace || h.SpanID != c.span || h.Reserved != op {
				t.Fatalf("header %+v does not carry trace %x span %x op %d", h, c.trace, c.span, op)
			}
		}
	})
}

// FuzzExtractCallHeaderDeadline: a live deadline becomes a positive relative
// timeout no longer than what remains; an expired or cancelled context is
// timeout 1 (expire at once), never 0 (no deadline, I3).
func FuzzExtractCallHeaderDeadline(f *testing.F) {
	f.Add(int64(0), false)
	f.Add(int64(-1), false)
	f.Add(int64(1), false)
	f.Add(int64(math.MaxInt64), false)
	f.Add(int64(math.MinInt64), false)
	f.Add(int64(int64(time.Hour)), true)
	f.Fuzz(func(t *testing.T, offset int64, cancel bool) {
		base := context.Background()
		before := time.Now()
		ctx, stop := context.WithDeadline(base, before.Add(time.Duration(offset)))
		defer stop()
		if cancel {
			stop()
		}
		h, err := extractCallHeader(ctx, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if h.TimeoutNS == 0 {
			t.Fatalf("deadline offset %d (cancel=%v) produced timeout 0 = no deadline", offset, cancel)
		}
		if ctx.Err() != nil && h.TimeoutNS != 1 {
			t.Fatalf("done context produced timeout %d, want 1", h.TimeoutNS)
		}
		if dl, _ := ctx.Deadline(); ctx.Err() == nil && h.TimeoutNS > uint64(dl.Sub(before)) {
			t.Fatalf("timeout %d exceeds the %v remaining", h.TimeoutNS, dl.Sub(before))
		}
	})
}

// extractCallHeader is the header submitInput builds, composed the way it
// now builds it: ids and opcode before the permit (callHeaderIDs, so a
// carrier runs while no permit is held), the relative timeout after it
// (stampTimeout). The targets above were written against the single
// function this replaced; they check the composed result.
func extractCallHeader(ctx context.Context, flags, defaultOpcode uint32) (ffi.CallHeader, error) {
	h, err := callHeaderIDs(ctx, flags, defaultOpcode)
	if err != nil {
		return h, err
	}
	stampTimeout(ctx, &h)
	return h, nil
}
