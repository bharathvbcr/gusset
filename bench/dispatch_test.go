package bench_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// BenchmarkDeadlineExpiry is the already-expired Call path: the context's
// deadline is in the past, so Call returns context.DeadlineExceeded without
// waiting on a worker.
//
// It measures the Go early return in submitInput and nothing of the engine: it
// never submits a job, so no deadline is ever delivered to Rust. The name is
// kept because the CI benchmark gate compares by name and a rename would read
// as a dropped benchmark plus a new one; BenchmarkDeadlineInFlight is the one
// that reaches the engine.
func BenchmarkDeadlineExpiry(b *testing.B) {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	payload := []byte{0}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := h.Call(ctx, payload)
		if !errors.Is(err, context.DeadlineExceeded) {
			b.Fatalf("Call: %v", err)
		}
	}
}

// BenchmarkDeadlineInFlight is a deadline that expires while the job is running
// in the engine, the path BenchmarkDeadlineExpiry never reaches.
//
// The deadline travels in the call header (submitInput stamps it from the
// Submit context), and the engine finds it expired at its next ctx.check() and
// posts a Cancelled completion. Diagnostic mode 5 is the cooperative engine for
// that: it checks, sleeps 10 ms, and repeats, so the job outlives the deadline
// by one sleep at most. Mode 9 would not do, because it never checks.
//
// What makes this the engine's deadline rather than Go's: the Wait context has
// no deadline. A DeadlineExceeded out of Wait can then only be the Cancelled
// completion Rust posted, and the loop fails on anything else, so a run that
// reports a figure has delivered the deadline to the engine on every iteration.
// engine-cancel/op carries that count, and reads 1.00 whenever the figure is
// meaningful. A Submit that fails is a different outcome (the deadline expired
// before the job was admitted, or the pool refused it) and fails the run rather
// than being counted as an in-flight expiry.
//
// ns/op is the whole cycle: submit, the engine's remaining run to its next
// check, and the completion's return. It is dominated by mode 5's 10 ms sleep
// and is a stability figure for the path, not a latency to optimise.
func BenchmarkDeadlineInFlight(b *testing.B) {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const deadline = 20 * time.Millisecond
	// 20 iterations of 10 ms: a run to completion would take 200 ms, ten times
	// the deadline, so an engine that ignored the deadline cannot pass for one
	// that enforced it.
	payload := []byte{5, 20}
	var cancelled int

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		ticket, err := h.Submit(ctx, payload)
		cancel()
		if err != nil {
			b.Fatalf("Submit before the deadline: %v", err)
		}
		_, err = h.Wait(context.Background(), ticket)
		if !errors.Is(err, context.DeadlineExceeded) {
			b.Fatalf("Wait with no deadline returned %v, want the engine's Cancelled completion as context.DeadlineExceeded", err)
		}
		cancelled++
	}

	b.ReportMetric(float64(cancelled)/float64(b.N), "engine-cancel/op")
}

// BenchmarkDiscard is Submit followed by Discard, the path that drops a
// ticket's result instead of waiting for it.
func BenchmarkDiscard(b *testing.B) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx := context.Background()
	payload := []byte{0}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		ticket, err := h.Submit(ctx, payload)
		if err != nil {
			b.Fatalf("Submit failed: %v", err)
		}
		if err := h.Discard(ticket); err != nil {
			b.Fatalf("Discard failed: %v", err)
		}
	}
}

// BenchmarkStats is the public allocator snapshot. Open keeps the bench on a
// live handle, matching the other dispatch benches; Stats itself is process-wide.
func BenchmarkStats(b *testing.B) {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	b.ReportAllocs()
	b.ResetTimer()

	var sink gusset.AllocStats
	for i := 0; i < b.N; i++ {
		sink = gusset.Stats()
	}
	if sink.LiveBytes == ^uint64(0) && sink.PeakBytes == ^uint64(0) && sink.AllocCount == ^uint64(0) {
		b.Fatal("stats sentinel")
	}
}
