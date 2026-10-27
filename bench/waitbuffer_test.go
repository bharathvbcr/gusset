package bench_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/bharathvbcr/gusset"
)

// BenchmarkGussetWaitBuffer measures Submit + WaitBuffer + Free of a small,
// inline result: one the engine returns within the 4 KiB inline limit, so it
// is never a Rust take buffer.
//
// The diagnostic engine's mode 0 echoes its input, so an n-byte request is an
// n-byte result. n = 0 is the empty-output path (a Buffer with no memory at
// all); 4096 is the largest result that still travels inline.
//
// Each sub-benchmark also reports rustbacked/op, the share of results whose
// Buffer owns Rust memory (Buffer.ID() != 0). It is what tells two builds
// apart by path rather than only by time: a change to how WaitBuffer carries
// a small result moves it between 0 and 1, and a run that did not move it
// has not measured the change.
func BenchmarkGussetWaitBuffer(b *testing.B) {
	for _, n := range []int{0, 48, 256, 1024, 4096} {
		b.Run(fmt.Sprintf("%dB", n), func(b *testing.B) {
			h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
			if err != nil {
				b.Fatalf("Open failed: %v", err)
			}
			defer h.Close()

			payload := make([]byte, n)
			for i := range payload {
				payload[i] = byte(i)
			}
			if n > 0 {
				payload[0] = 0 // mode 0: echo
			}

			ctx := context.Background()
			var rustBacked int

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				ticket, err := h.Submit(ctx, payload)
				if err != nil {
					b.Fatalf("Submit failed: %v", err)
				}
				out, err := h.WaitBuffer(ctx, ticket)
				if err != nil {
					b.Fatalf("WaitBuffer failed: %v", err)
				}
				if got := len(out.Bytes()); got != n {
					b.Fatalf("WaitBuffer returned %d bytes, want %d", got, n)
				}
				if out.ID() != 0 {
					rustBacked++
				}
				if err := out.Free(); err != nil {
					b.Fatalf("Free failed: %v", err)
				}
			}

			b.StopTimer()
			b.ReportMetric(float64(rustBacked)/float64(b.N), "rustbacked/op")
		})
	}
}
