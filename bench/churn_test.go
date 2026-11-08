package bench_test

import (
	"testing"

	"github.com/bharathvbcr/gusset"
)

// BenchmarkHandleChurn is the cost of one Open followed by Close on a fresh
// handle: pool construction, worker thread start-up, and the drain and join on
// the way out. The other benchmarks open once and amortise that across b.N, so
// none of them can see a change to it.
//
// Pool size 4 matches BenchmarkGussetCallNoop, so the per-worker share of the
// figure can be read against that one. No call is made: the figure is the
// handle's lifecycle and nothing of dispatch.
func BenchmarkHandleChurn(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
		if err != nil {
			b.Fatalf("Open failed: %v", err)
		}
		if err := h.Close(); err != nil {
			b.Fatalf("Close failed: %v", err)
		}
	}
}
