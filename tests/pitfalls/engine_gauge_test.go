package pitfalls_test

// The engine-side in-flight gauge (diagnostic modes 17 and 18).
//
// I4 says in-flight calls per handle never exceed the pool size. A Go test that
// counts its own goroutines or reads the thread total after the run cannot see
// two jobs executing at once: both are consequences of the semaphore, not
// observations of the engine. Mode 17 is a non-cooperative job that raises a
// process-wide atomic on entry and lowers it on exit; mode 18 reads the highest
// value it reached since the last read. That is the engine reporting how many
// jobs it ran concurrently.
//
// The gauge is process-wide, so a test that reads it must not run alongside
// another that submits mode 17. None here run in parallel.

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// holdInEngine is a mode 17 payload: count in, sleep ms without ever checking
// for cancellation, count out.
func holdInEngine(ms byte) []byte { return []byte{17, ms} }

// peakInEngine reads and resets the engine's peak concurrent mode 17 jobs.
func peakInEngine(t *testing.T, h *gusset.Handle) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := h.Call(ctx, []byte{18})
	if err != nil {
		t.Fatalf("reading the engine gauge: %v", err)
	}
	if len(out) != 8 {
		t.Fatalf("engine gauge returned %d bytes, want 8", len(out))
	}
	return binary.LittleEndian.Uint64(out)
}

// The gauge has to be able to see concurrency, or "peak <= pool size" in the
// tests that use it proves nothing. Four callers on a four-worker pool, each
// holding the engine for 100 ms, are all inside it together.
func TestEngineGauge_SeesJobsRunningAtOnce(t *testing.T) {
	const poolSize = 4
	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()
	_ = peakInEngine(t, h) // reset

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tickets := make([]uint64, poolSize)
	for i := range tickets {
		if tickets[i], err = h.Submit(ctx, holdInEngine(100)); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}
	for i, tk := range tickets {
		if _, err := h.Wait(ctx, tk); err != nil {
			t.Fatalf("Wait %d: %v", i, err)
		}
	}
	if peak := peakInEngine(t, h); peak != poolSize {
		t.Fatalf("engine saw a peak of %d concurrent jobs, want exactly the pool size %d", peak, poolSize)
	}
}
