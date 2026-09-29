package gusset

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// goexitSpan is a trace carrier that ends its goroutine with runtime.Goexit,
// which is what t.Fatal and t.FailNow do inside an adopter's test carrier.
type goexitSpan struct{}

func (goexitSpan) TraceID() [16]byte {
	runtime.Goexit()
	return [16]byte{}
}
func (goexitSpan) SpanID() [8]byte { return [8]byte{} }

// A carrier that calls runtime.Goexit must not take a pool permit with it.
//
// readTraceCarrier recovers panics, but recover returns nil during Goexit: the
// goroutine keeps unwinding through submitInput, which read the carrier after
// taking a permit, so the `<-s.sem` on its error path never ran. Every such
// call removed one permit from the pool for the life of the handle; pool-size
// of them and every later Submit parked forever (I4).
func TestTraceCarrier_GoexitDoesNotLeakAPermit(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := context.WithValue(context.Background(), SpanContextKey, goexitSpan{})
		_, _ = h.Submit(ctx, []byte{0, 1})
		t.Error("Submit returned; the carrier's Goexit should have ended the goroutine")
	}()
	<-done

	if n := len(h.state.sem); n != 0 {
		t.Errorf("%d pool permit(s) held after the submitting goroutine exited (I4)", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := h.Call(ctx, []byte{0, 7})
	if err != nil || len(out) != 2 || out[1] != 7 {
		t.Fatalf("Call after a Goexiting carrier on a pool of 1: %v %v (the permit leaked)", out, err)
	}
}

type testSpanCarrier struct{ trace [16]byte }

func (s testSpanCarrier) TraceID() [16]byte { return s.trace }
func (s testSpanCarrier) SpanID() [8]byte   { return [8]byte{s.trace[0]} }

var benchSink []byte

func benchmarkCallNoop(b *testing.B, ctx context.Context) {
	h, err := Open(WithDiagnosticEngine())
	if err != nil {
		b.Fatal(err)
	}
	defer h.Close()
	in := []byte{0, 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := h.Call(ctx, in)
		if err != nil {
			b.Fatal(err)
		}
		benchSink = out
	}
}

func BenchmarkCallNoopBare(b *testing.B) { benchmarkCallNoop(b, context.Background()) }

func BenchmarkCallNoopTraced(b *testing.B) {
	benchmarkCallNoop(b, context.WithValue(context.Background(), SpanContextKey, testSpanCarrier{trace: [16]byte{1}}))
}
