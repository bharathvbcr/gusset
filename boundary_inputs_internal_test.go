package gusset

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

// The budget crosses the ABI as a uint32 of milliseconds. A value past MaxDrain
// must clamp, not wrap: 2^32 ms + 5 ms would otherwise cross as a 5 ms drain,
// and a caller that asked for "long" gets a drain that gives up at once.
func TestDrainMillis_ClampsInsteadOfWrapping(t *testing.T) {
	maxMS := uint32(MaxDrain / time.Millisecond)
	if maxMS != math.MaxUint32 {
		t.Fatalf("MaxDrain is %d ms, want the full uint32 range", maxMS)
	}
	cases := []struct {
		name string
		in   time.Duration
		want uint32
	}{
		{"zero", 0, 0},
		{"negative one ns", -1, 0},
		{"negative hour", -time.Hour, 0},
		{"min duration", time.Duration(math.MinInt64), 0},
		{"sub-millisecond", 999 * time.Microsecond, 0},
		{"one second", time.Second, 1000},
		{"exactly MaxDrain", MaxDrain, math.MaxUint32},
		{"MaxDrain minus 1ms", MaxDrain - time.Millisecond, math.MaxUint32 - 1},
		{"MaxDrain plus 1ms wraps to 0 unclamped", MaxDrain + time.Millisecond, math.MaxUint32},
		{"2^32 ms plus 5 ms wraps to 5 unclamped", MaxDrain + 6*time.Millisecond, math.MaxUint32},
		{"max duration", time.Duration(math.MaxInt64), math.MaxUint32},
	}
	for _, tc := range cases {
		if got := drainMillis(tc.in); got != tc.want {
			t.Errorf("%s: drainMillis(%v) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
}

// Submit takes []byte, *Buffer or nil. Anything else must be refused with an
// error that names the accepted types, before a permit is taken or Rust is
// entered, and the handle must keep working.
func TestSubmit_RefusesUnsupportedInputTypes(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type namedBytes []byte
	b := []byte{0, 1}
	inputs := map[string]any{
		"int":               42,
		"string":            "hello",
		"[]int":             []int{1, 2},
		"array":             [2]byte{0, 1},
		"*[]byte":           &b,
		"named []byte type": namedBytes{0, 1},
		"struct":            struct{ X int }{1},
		"func":              func() {},
		"Buffer by value":   Buffer{},
	}
	for name, in := range inputs {
		ticket, err := h.Submit(ctx, in)
		if err == nil {
			t.Errorf("%s: Submit accepted an unsupported input (ticket %d)", name, ticket)
			continue
		}
		if ticket != 0 {
			t.Errorf("%s: refused Submit returned ticket %d", name, ticket)
		}
		if !strings.Contains(err.Error(), "[]byte or *Buffer") {
			t.Errorf("%s: error does not name the accepted types: %v", name, err)
		}
	}
	if n := len(h.state.sem); n != 0 {
		t.Fatalf("refused submits leaked %d pool permit(s)", n)
	}

	// A typed nil *Buffer is its own refusal, not the type error.
	var nilBuf *Buffer
	if _, err := h.Submit(ctx, nilBuf); err == nil || err != ErrNilBuffer {
		t.Fatalf("nil *Buffer: want ErrNilBuffer, got %v", err)
	}

	// The handle is not poisoned or wedged by the refusals.
	out, err := h.Call(ctx, []byte{0, 9})
	if err != nil || len(out) != 2 || out[1] != 9 {
		t.Fatalf("echo after refused inputs: %v %v", out, err)
	}
}
