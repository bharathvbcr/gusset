package gusset

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"runtime/metrics"
	"testing"
	"time"
)

func goThreadCount(t *testing.T) int64 {
	t.Helper()
	sample := []metrics.Sample{{Name: "/sched/threads/total:threads"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindUint64 {
		t.Skip("/sched/threads/total:threads is not available in this Go")
	}
	return int64(sample[0].Value.Uint64())
}

// A burst of handles dropped to the GC backstop must not leave a thread per
// handle behind. Each close below blocks in cgo for ~300 ms joining a worker
// that runs a non-cooperative job, and Go never returns the Ms such calls
// used; closing them all at once grew the process by one thread per handle
// (46 of 48 measured). Meaningful once per process: Ms retained by an earlier
// run are already in a later run's baseline, which is the leak itself.
func TestBackstop_ABurstOfDroppedHandlesRetainsBoundedThreads(t *testing.T) {
	if testing.Short() {
		t.Skip("backstop burst skipped in -short mode")
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)

	const burst = 48
	// Warm up the Ms an ordinary open/call/close uses, so the baseline holds
	// them already.
	for i := 0; i < 8; i++ {
		h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Call(context.Background(), []byte{0, 1}); err != nil {
			t.Fatal(err)
		}
		_ = h.Close()
	}
	runtime.GC()
	before := goThreadCount(t)

	states := make([]*handleState, 0, burst)
	func() {
		for i := 0; i < burst; i++ {
			h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
			if err != nil {
				t.Fatal(err)
			}
			// Mode 9: sleep 300 ms without checking for cancellation, so the
			// close has a worker to wait for.
			if _, err := h.Submit(context.Background(), []byte{9, 30}); err != nil {
				t.Fatal(err)
			}
			states = append(states, h.state)
		}
	}()
	runtime.GC()
	runtime.GC()
	for _, s := range states {
		select {
		case <-s.closeDone:
		case <-time.After(60 * time.Second):
			t.Fatal("a dropped handle was not closed by its backstop within 60 s")
		}
	}
	grown := goThreadCount(t) - before
	t.Logf("%d dropped handles grew Go's threads by %d", burst, grown)
	// cap(backstopClosers) closes in cgo at once, plus slack for the
	// runtime's own Ms (GC workers, sysmon, the cleanup goroutine).
	if limit := int64(cap(backstopClosers)) + 12; grown > limit {
		t.Fatalf("%d dropped handles grew Go's threads by %d, over %d", burst, grown, limit)
	}
}
