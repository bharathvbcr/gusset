package bench_test

import (
	"runtime"
	"testing"

	"github.com/bharathvbcr/gusset"
)

// BenchmarkBufferAllocParallel allocates and frees a small Rust-owned buffer from
// GOMAXPROCS goroutines at once, each on a handle of its own.
//
// It exists to put the process-wide allocator counters (LIVE_BYTES, PEAK_BYTES and
// ALLOC_COUNT in crates/gusset/src/ffi/alloc.rs) under parallel load. Every
// NewBuffer adds to the live total and the allocation count and every Free takes
// the bytes back, whichever handle made them, so goroutines that share nothing
// else still share those atomics. No other benchmark does that: CallParallel
// copies a 1-byte payload and never allocates a buffer, and BufferLarge allocates
// once.
//
// One handle per goroutine, opened before the timer starts, keeps the handle's own
// buffer registry out of the measurement; what the goroutines still share is the
// counters and the system allocator. The counter update is a small fraction of a
// cgo round trip plus a 64-byte aligned malloc and free, so this reads the effect
// on a path an adopter runs, not the counters in isolation. The Rust bench
// `alloc_counters` (crates/gusset/benches) isolates them, but cargo arms cannot be
// recorded as RECORD_VARIANTS, so this one is the arm to A/B.
func BenchmarkBufferAllocParallel(b *testing.B) {
	workers := runtime.GOMAXPROCS(0)
	handles := make(chan *gusset.Handle, workers)
	for i := 0; i < workers; i++ {
		h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
		if err != nil {
			b.Fatalf("Open failed: %v", err)
		}
		defer h.Close()
		handles <- h
	}

	const bufSize = 64

	b.ReportAllocs()
	b.ResetTimer()

	// RunParallel starts exactly GOMAXPROCS goroutines (parallelism 1), so each
	// takes one of the handles opened above and none waits for another.
	b.RunParallel(func(pb *testing.PB) {
		h := <-handles
		for pb.Next() {
			buf, err := h.NewBuffer(bufSize)
			if err != nil {
				b.Errorf("NewBuffer failed: %v", err)
				return
			}
			if err := buf.Free(); err != nil {
				b.Errorf("Free failed: %v", err)
				return
			}
		}
	})
}
