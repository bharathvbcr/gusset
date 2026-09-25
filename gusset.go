package gusset

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime/metrics"
	"sync/atomic"
	"time"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// Re-exported error types and sentinels for errors.Is matching.
type Error = ffi.Error

var (
	ErrGeneric  = ffi.ErrGeneric
	ErrPanic    = ffi.ErrPanic
	ErrPoisoned = ffi.ErrPoisoned
	ErrBadArg   = ffi.ErrBadArg

	// ErrShutdown matches work cancelled by Shutdown or refused after it. It
	// is not context.Canceled: the caller's own context was still live.
	ErrShutdown = ffi.ErrShutdown
	// ErrShutdownIncomplete is returned by Shutdown when its drain budget
	// expired with work still running (an engine that never checks its
	// JobContext).
	ErrShutdownIncomplete = ffi.ErrShutdownIncomplete
)

// errHandlePoisoned is ErrPoisoned with a message; errors.Is matches by code.
// The bare sentinel printed as "gusset error [3]: ".
var errHandlePoisoned = &ffi.Error{
	Code: ffi.ErrPoisoned.Code,
	Msg:  "handle is poisoned: a job panicked; Close it and open a new handle",
}

// Expected ABI constants compiled into Go (I6).
//
// AllocStats joined the verified set in ABI version 2. gusset_alloc_stats writes it
// directly into Go memory, so a size or alignment disagreement corrupts the Go heap;
// before version 2 nothing checked it. internal/ffi/gusset.h is hand-maintained
// rather than generated, which makes this runtime check the only guard against the
// header drifting from the Rust structs.
const (
	ExpectedAbiVersion = 2
	ExpectedHeaderSize = 40
	ExpectedStatusSize = 48
	ExpectedLayoutSize = 36
	ExpectedStatsSize  = 24

	ExpectedHeaderAlign = 8
	ExpectedStatusAlign = 8
	ExpectedLayoutAlign = 4
	ExpectedStatsAlign  = 8
)

func init() {
	// I6: Validate ABI layout at initialization time.
	layout := ffi.GetAbiLayout()
	if layout.Version != ExpectedAbiVersion {
		panic(fmt.Sprintf(
			"gusset: ABI version mismatch: Go expected version %d, Rust library reported version %d",
			ExpectedAbiVersion,
			layout.Version,
		))
	}

	expectedSizes := [ffi.AbiTypeCount]uint32{
		ExpectedHeaderSize, ExpectedStatusSize, ExpectedLayoutSize, ExpectedStatsSize,
	}
	expectedAligns := [ffi.AbiTypeCount]uint32{
		ExpectedHeaderAlign, ExpectedStatusAlign, ExpectedLayoutAlign, ExpectedStatsAlign,
	}

	if layout.Sizes != expectedSizes || layout.Aligns != expectedAligns {
		panic(fmt.Sprintf(
			"gusset: ABI struct size/alignment mismatch: expected sizes %v aligns %v, got sizes %v aligns %v",
			expectedSizes,
			expectedAligns,
			layout.Sizes,
			layout.Aligns,
		))
	}

	// Second, independent check: what Rust reports against what cgo actually
	// compiled from the hand-maintained header. The constants above would agree
	// with Rust while the header disagreed with both. Size and alignment still
	// miss an equal-width field swap; the named-field check below is that case.
	localSizes, localAligns := ffi.LocalLayout()
	for i := 0; i < ffi.AbiTypeCount; i++ {
		if localSizes[i] != layout.Sizes[i] || localAligns[i] != layout.Aligns[i] {
			panic(fmt.Sprintf(
				"gusset: ABI layout drift for %s: Rust reports size %d align %d, "+
					"cgo compiled size %d align %d from internal/ffi/gusset.h",
				ffi.AbiTypeNames[i],
				layout.Sizes[i], layout.Aligns[i],
				localSizes[i], localAligns[i],
			))
		}
	}

	// Size and alignment stay equal when two fields of equal width trade places.
	// Rust and cgo each report the offset and size of every named field; a
	// disagreement means the hand-maintained header and the archive do not
	// describe the same layout, and a later struct write would land on the
	// wrong slot.
	rustOff, rustSz, nfields := ffi.RustFieldLayout()
	if nfields != ffi.AbiFieldCount {
		panic(fmt.Sprintf(
			"gusset: ABI field count mismatch: Go expected %d, Rust reported %d",
			ffi.AbiFieldCount, nfields,
		))
	}
	localOff, localSz := ffi.LocalFieldLayout()
	for i := 0; i < ffi.AbiFieldCount; i++ {
		if rustOff[i] != localOff[i] || rustSz[i] != localSz[i] {
			panic(fmt.Sprintf(
				"gusset: ABI field %s drifted: Rust offset %d size %d, "+
					"cgo offset %d size %d from internal/ffi/gusset.h",
				ffi.AbiFieldNames[i],
				rustOff[i], rustSz[i],
				localOff[i], localSz[i],
			))
		}
	}

	if err := ffi.Init(); err != nil {
		panic(fmt.Sprintf("gusset: failed to initialize runtime: %v", err))
	}
}

// Threads returns the current number of OS threads allocated by the Go scheduler.
// Reads `/sched/threads/total:threads` (R11; Go 1.27 catalogue name).
func Threads() int64 {
	// Go 1.26 release notes named `/sched/threads:threads`. The 1.27
	// runtime/metrics catalogue publishes `/sched/threads/total:threads` and
	// does not list the shorter name. Read the live name first so a KindBad
	// on the historical alias cannot hide a real reading.
	samples := []metrics.Sample{
		{Name: "/sched/threads/total:threads"},
		{Name: "/sched/threads:threads"},
	}
	metrics.Read(samples)
	for _, s := range samples {
		if s.Value.Kind() == metrics.KindUint64 {
			return int64(s.Value.Uint64())
		}
	}
	return -1
}

// DrainLogs drains logs from the internal Rust log ring into the provided buffer.
func DrainLogs(buf []byte) int {
	return ffi.DrainLogs(buf)
}

// Shutdown drains the whole runtime within a budget: it refuses further
// submissions, cancels every job on every live handle, and waits up to drain for
// the work already running to finish.
//
// This is the bounded half of shutdown, and it is what [Handle.Close] is not.
// Close joins its worker threads, so its latency is whatever the engine still
// has left to do — the right trade, since detaching those threads would leave
// them running against Rust memory that is about to be freed, but it means
// Close alone gives an operator no way to cap shutdown. Shutdown first, with a
// budget, then Close each handle: the cancel has already landed, so the join
// is short.
//
// Returns nil when every work unit finished inside the budget. A non-nil error
// means work was still in flight at the deadline and says so rather than
// reporting a success the caller cannot rely on. Cancellation is cooperative:
// an engine that never calls JobContext::check cannot be drained at all, so
// that error is the expected outcome for one, not a malfunction.
//
// Process-wide and one-way. The refusal latch is global to the process and is
// cleared only by re-initialising the runtime, which package init does once. A
// process that has called Shutdown will refuse every later submission on every
// handle, which is the intent — this is for shutting the process down, not for
// quiescing one handle. To drain a single handle, close it.
//
// A drain longer than [MaxDrain] is capped to it; a negative drain is treated
// as zero, which cancels everything and reports immediately.
func Shutdown(drain time.Duration) error {
	ms := drain.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	if ms > int64(MaxDrain/time.Millisecond) {
		ms = int64(MaxDrain / time.Millisecond)
	}
	shutdownStarted.Store(true)
	return ffi.Shutdown(uint32(ms))
}

// shutdownStarted is set once Shutdown has run; see shutdownCause.
var shutdownStarted atomic.Bool

// MaxDrain caps [Shutdown]'s budget.
//
// The budget crosses the ABI as a uint32 of milliseconds, so it is bounded by
// construction; capping here means a caller passing, say, a duration built from
// a misparsed config never silently wraps into a short drain.
const MaxDrain = time.Duration(^uint32(0)) * time.Millisecond

// SpanContextKey is the context key Gusset reads trace correlation from.
//
// It is an unexported-type key rather than the bare string "spanContext": a string
// key collides with any other package using the same literal, and go vet flags it.
// Callers attach a value implementing TraceID() [16]byte and SpanID() [8]byte:
//
//	ctx = context.WithValue(ctx, gusset.SpanContextKey, mySpanContext)
//
// OpenTelemetry's own span context is stored under OTel's private key, which no
// third party can read, so propagation is explicit by design rather than magic that
// silently never fires.
var SpanContextKey = spanContextKey{}

type spanContextKey struct{}

// TraceCarrier is the shape Gusset reads trace correlation from (R9).
type TraceCarrier interface {
	TraceID() [16]byte
	SpanID() [8]byte
}

// OpcodeContextKey is the context key for attaching an engine dispatch opcode (R9).
var OpcodeContextKey = opcodeContextKey{}

type opcodeContextKey struct{}

// ContextWithOpcode attaches an engine dispatch opcode to the context.
// Overrides the default opcode configured on the Handle.
func ContextWithOpcode(ctx context.Context, opcode uint32) context.Context {
	return context.WithValue(ctx, OpcodeContextKey, opcode)
}

// extractCallHeader extracts timeout, trace/span context, and opcode if present.
//
// An opcode that does not fit in the header's u32 is an error. Narrowing it
// used to store the low 32 bits, and `1<<32` became 0 — the diagnostic engine.
func extractCallHeader(ctx context.Context, flags uint32, defaultOpcode uint32) (ffi.CallHeader, error) {
	header := ffi.CallHeader{
		Flags:    flags,
		Reserved: defaultOpcode,
	}

	if ctx == nil {
		return header, nil
	}

	if err := ctx.Err(); err != nil {
		header.TimeoutNS = 1 // Already expired or cancelled
	} else if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 {
			header.TimeoutNS = uint64(remaining.Nanoseconds())
		} else {
			header.TimeoutNS = 1 // Already expired
		}
	}

	if sc, ok := ctx.Value(SpanContextKey).(TraceCarrier); ok && sc != nil {
		header.TraceID = sc.TraceID()
		header.SpanID = sc.SpanID()
	}

	if opVal := ctx.Value(OpcodeContextKey); opVal != nil {
		op, err := opcodeFromContext(opVal)
		if err != nil {
			return header, err
		}
		header.Reserved = op
	}

	return header, nil
}

func opcodeFromContext(opVal any) (uint32, error) {
	const reject = "gusset: opcode does not fit in uint32"
	if v, ok := opVal.(uint32); ok { // ContextWithOpcode's type: no reflection
		return v, nil
	}
	// Any integer kind, named types included (`type Op uint16`). The explicit
	// type switch this replaced accepted int/int32/int64/uint/uint64 only, so
	// a uint8, uint16 or a named opcode type was refused with an error that
	// said "must be a 32-bit unsigned integer" about a value that was one.
	rv := reflect.ValueOf(opVal)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n := rv.Int()
		if n < 0 || n > math.MaxUint32 {
			return 0, errors.New(reject)
		}
		return uint32(n), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n := rv.Uint()
		if n > math.MaxUint32 {
			return 0, errors.New(reject)
		}
		return uint32(n), nil
	default:
		return 0, fmt.Errorf("gusset: opcode context value must be an integer, got %T", opVal)
	}
}
