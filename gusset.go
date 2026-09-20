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
const (
	ExpectedAbiVersion = 1
	ExpectedHeaderSize = 40
	ExpectedStatusSize = 48
	ExpectedLayoutSize = 28

	ExpectedHeaderAlign = 8
	ExpectedStatusAlign = 8
	ExpectedLayoutAlign = 4
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

	expectedSizes := [3]uint32{ExpectedHeaderSize, ExpectedStatusSize, ExpectedLayoutSize}
	expectedAligns := [3]uint32{ExpectedHeaderAlign, ExpectedStatusAlign, ExpectedLayoutAlign}

	if layout.Sizes != expectedSizes || layout.Aligns != expectedAligns {
		panic(fmt.Sprintf(
			"gusset: ABI struct size/alignment mismatch: expected sizes %v aligns %v, got sizes %v aligns %v",
			expectedSizes,
			expectedAligns,
			layout.Sizes,
			layout.Aligns,
		))
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

// extractCallHeader extracts timeout and OpenTelemetry trace/span context if present.
func extractCallHeader(ctx context.Context) ffi.CallHeader {
	var header ffi.CallHeader

	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 {
			header.TimeoutNS = uint64(remaining.Nanoseconds())
		} else {
			header.TimeoutNS = 1 // Already expired
		}
	}

	// Extract trace_id and span_id if OpenTelemetry span context is in ctx
	type traceContextGetter interface {
		TraceID() [16]byte
		SpanID() [8]byte
	}

	if sc, ok := ctx.Value("spanContext").(traceContextGetter); ok {
		header.TraceID = sc.TraceID()
		header.SpanID = sc.SpanID()
	}

	return header
}
