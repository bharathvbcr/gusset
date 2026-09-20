package gusset

import (
	"context"
	"fmt"
	"runtime/metrics"
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
)

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
	// with Rust while the header disagreed with both, and nothing else would
	// notice until a struct write landed at the wrong offset in the Go heap.
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

	if err := ffi.Init(); err != nil {
		panic(fmt.Sprintf("gusset: failed to initialize runtime: %v", err))
	}
}

// Threads returns the current number of OS threads allocated by the Go scheduler.
// Reads the /sched/threads:threads runtime metric (R11).
func Threads() int64 {
	samples := []metrics.Sample{
		{Name: "/sched/threads:threads"},
		{Name: "/sched/threads/total:threads"},
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
func extractCallHeader(ctx context.Context, flags uint32, defaultOpcode uint32) ffi.CallHeader {
	header := ffi.CallHeader{
		Flags:    flags,
		Reserved: defaultOpcode,
	}

	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 {
			header.TimeoutNS = uint64(remaining.Nanoseconds())
		} else {
			header.TimeoutNS = 1 // Already expired
		}
	}

	if sc, ok := ctx.Value(SpanContextKey).(TraceCarrier); ok {
		header.TraceID = sc.TraceID()
		header.SpanID = sc.SpanID()
	}

	if op, ok := ctx.Value(OpcodeContextKey).(uint32); ok {
		header.Reserved = op
	}

	return header
}
