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
