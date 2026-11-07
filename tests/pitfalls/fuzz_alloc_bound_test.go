package pitfalls_test

import (
	"context"
	"encoding/binary"
	"testing"
)

// lengthRecorder is a diagHandle that keeps the payloads it was called with.
type lengthRecorder struct{ payloads [][]byte }

func (r *lengthRecorder) Call(_ context.Context, in []byte) ([]byte, error) {
	r.payloads = append(r.payloads, append([]byte(nil), in...))
	return in, nil
}

func (r *lengthRecorder) Close() error { return nil }

// A fuzz input that asks diagnostic mode 16 for a gigabyte must not reach the
// engine as one. Nightly 41825244733 died on exactly this input: one call
// peaked at about 3 GiB resident, times every fuzz worker, on a small runner.
func TestFuzzDiagnosticTarget_BoundsModeSixteenAllocation(t *testing.T) {
	// The committed regression input, 6617663891ad3163: length 0x3f148480.
	huge := []byte("\x10\x80\x84\x14?\xe5\xe6\x04\xee ")
	if got := binary.LittleEndian.Uint32(huge[1:5]); got != 0x3f148480 {
		t.Fatalf("fixture length is %#x, not the nightly's", got)
	}
	exact := []byte{16, 0, 0, 0x10, 0, 7} // length exactly fuzzAllocatedMax
	under := []byte{16, 0xFF, 0xFF, 0x0F, 0, 7}
	short := []byte{16, 1, 2}
	other := []byte{0, 0xFF, 0xFF, 0xFF, 0xFF}
	// One byte over the engine's cap: refused before any allocation, so the
	// refusal path must still be reachable from the fuzzer.
	overCap := []byte{16, 0x01, 0x00, 0x00, 0x40, 7}

	rec := &lengthRecorder{}
	d := &diagFuzzer{h: rec, open: func() (diagHandle, error) { return rec, nil }}
	for _, in := range [][]byte{huge, exact, under, short, other, overCap} {
		before := append([]byte(nil), in...)
		if msgs := iterate(d, in); len(msgs) != 0 {
			t.Fatalf("input % x reported %q", in, msgs)
		}
		if string(in) != string(before) {
			t.Errorf("input % x was edited in place to % x", before, in)
		}
	}
	if len(rec.payloads) != 6 {
		t.Fatalf("%d calls reached the handle, want 6", len(rec.payloads))
	}
	if string(rec.payloads[5]) != string(overCap) {
		t.Errorf("a length over MaxBufferBytes was altered to % x; the engine's refusal path is no longer fuzzed", rec.payloads[5])
	}
	for i, p := range rec.payloads[:2] {
		if n := binary.LittleEndian.Uint32(p[1:5]); n >= fuzzAllocatedMax {
			t.Errorf("input %d reached the engine asking for %d bytes; the cap is %d", i, n, fuzzAllocatedMax)
		}
	}
	if string(rec.payloads[2]) != string(under) || string(rec.payloads[3]) != string(short) || string(rec.payloads[4]) != string(other) {
		t.Errorf("payloads below the cap or in other modes were altered: % x", rec.payloads[2:])
	}
}
