package e2e_test

import (
	"bytes"
	"testing"
	"unsafe"
)

// testTraceCarrier implements gusset.TraceCarrier for end-to-end trace propagation tests.
type testTraceCarrier struct {
	traceID [16]byte
	spanID  [8]byte
}

func (c *testTraceCarrier) TraceID() [16]byte {
	return c.traceID
}

func (c *testTraceCarrier) SpanID() [8]byte {
	return c.spanID
}

// newTestTraceCarrier generates a deterministic or pseudo-random trace carrier.
func newTestTraceCarrier(seed byte) *testTraceCarrier {
	c := &testTraceCarrier{}
	for i := range c.traceID {
		c.traceID[i] = seed + byte(i)
	}
	for i := range c.spanID {
		c.spanID[i] = seed + byte(i*2)
	}
	return c
}

// is64ByteAligned returns true if the underlying slice pointer is aligned to 64 bytes.
func is64ByteAligned(buf []byte) bool {
	if len(buf) == 0 {
		return true
	}
	ptr := uintptr(unsafe.Pointer(&buf[0]))
	return ptr%64 == 0
}

// computeVectorSumSquaresOracle computes the expected uint64 sum of squares for vector inputs
// matching the diagnostic engine mode 10 vector computation.
func computeVectorSumSquaresOracle(data []byte) uint64 {
	var acc uint64
	for _, b := range data {
		v := uint64(b)
		acc += v * v
	}
	return acc
}

// generateDeterministicBytes fills a slice of length n with deterministic patterned bytes.
func generateDeterministicBytes(n int, seed byte) []byte {
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = byte(i) ^ seed
	}
	return out
}

// assertBytesEqual asserts that got and want match byte-for-byte.
func assertBytesEqual(t *testing.T, got, want []byte, label string) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("%s mismatch: len(got)=%d, len(want)=%d", label, len(got), len(want))
	}
}

// requireNoError asserts err is nil.
func requireNoError(t *testing.T, err error, action string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s failed: %v", action, err)
	}
}
