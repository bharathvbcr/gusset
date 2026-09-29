package pitfalls_test

import (
	"context"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// nilSafeSpan is a pointer carrier whose methods dereference the receiver, the
// way any struct-backed span context does.
type nilSafeSpan struct{ trace [16]byte }

func (s *nilSafeSpan) TraceID() [16]byte { return s.trace }
func (s *nilSafeSpan) SpanID() [8]byte   { return [8]byte{s.trace[0]} }

// TestPitfall_TypedNilTraceCarrierDoesNotPanicOrLeakPermit pins a typed-nil
// pointer stored under SpanContextKey.
//
// `ctx.Value(key).(TraceCarrier)` succeeds for a (*T)(nil), and `sc != nil` is
// true for an interface holding one, so TraceID() ran on a nil receiver and
// panicked on the caller's goroutine. It ran after the pool permit was taken,
// so a caller that recovered had also lost a permit for good: pool-size such
// calls and every later Submit parked forever (I4).
func TestPitfall_TypedNilTraceCarrierDoesNotPanicOrLeakPermit(t *testing.T) {
	const pool = 2
	h, err := gusset.Open(gusset.WithPoolSize(pool), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	var carrier *nilSafeSpan
	base, cancelBase := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelBase()
	ctx := context.WithValue(base, gusset.SpanContextKey, carrier)
	for i := 0; i < pool+1; i++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("call %d: a typed-nil carrier panicked the caller: %v", i, r)
				}
			}()
			if _, err := h.Call(ctx, []byte{0, 1}); err == nil {
				t.Errorf("call %d: a typed-nil carrier must be refused, got nil error", i)
			}
		}()
	}

	// Every permit must still be there: pool concurrent submits get in at once.
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < pool; i++ {
		tk, err := h.Submit(sctx, []byte{0, byte(i)})
		if err != nil {
			t.Fatalf("submit %d after the carrier failures: %v (a permit leaked)", i, err)
		}
		if _, err := h.Wait(sctx, tk); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}

	// A working carrier on the same handle still propagates.
	ok := &nilSafeSpan{trace: [16]byte{0xAB}}
	out, err := h.Call(context.WithValue(sctx, gusset.SpanContextKey, ok), []byte{7})
	if err != nil || len(out) != 24 || out[0] != 0xAB || out[16] != 0xAB {
		t.Fatalf("carrier propagation broke: %v %v", out, err)
	}

	// The guard is on the hot path of every traced call: it must not allocate.
	traced := context.WithValue(context.Background(), gusset.SpanContextKey, ok)
	bare := context.Background()
	in := []byte{0, 1}
	for i := 0; i < 50; i++ { // warm both paths
		_, _ = h.Call(traced, in)
		_, _ = h.Call(bare, in)
	}
	withCarrier := testing.AllocsPerRun(200, func() { _, _ = h.Call(traced, in) })
	without := testing.AllocsPerRun(200, func() { _, _ = h.Call(bare, in) })
	if withCarrier > without {
		t.Fatalf("a trace carrier costs %.1f allocs/call, a bare context %.1f", withCarrier, without)
	}
}
