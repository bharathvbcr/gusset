package gusset

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// idCarrier carries a trace and span id whose every byte differs, so a copy
// that shifts, truncates, swaps or repeats a byte cannot read back as correct.
type idCarrier struct {
	trace [16]byte
	span  [8]byte
}

func (c idCarrier) TraceID() [16]byte { return c.trace }
func (c idCarrier) SpanID() [8]byte   { return c.span }

// Every byte of the trace and span id must land at its own offset in the header
// Rust reads. Diagnostic mode 7 echoes the header's trace_id then span_id, so
// this reads back what crossed the boundary, all 24 bytes of it.
func TestHeader_TraceAndSpanBytesLandAtTheirOffsets(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	var c idCarrier
	for i := range c.trace {
		c.trace[i] = byte(0x10 + i)
	}
	for i := range c.span {
		c.span[i] = byte(0xA0 + i)
	}
	want := append(append([]byte{}, c.trace[:]...), c.span[:]...)

	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), SpanContextKey, c), 5*time.Second)
	defer cancel()
	out, err := h.Call(ctx, []byte{7})
	if err != nil {
		t.Fatalf("call with a trace carrier failed: %v", err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("header ids on the Rust side:\n got  % x\n want % x", out, want)
	}

	// A call with no carrier after one with a full carrier reads all zeros: the
	// header is built fresh per submit, nothing carries over.
	bare, cancelBare := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelBare()
	out, err = h.Call(bare, []byte{7})
	if err != nil {
		t.Fatalf("call without a carrier failed: %v", err)
	}
	if !bytes.Equal(out, make([]byte, 24)) {
		t.Fatalf("stale ids on a carrier-less call: % x", out)
	}
}
