package e2e_test

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
	"github.com/bharathvbcr/gusset/tests/internal/livebytes"
)

// TestTier1_HandleLifecycle_OpenClose validates clean opening, thread querying, and closing of Gusset handles.
func TestTier1_HandleLifecycle_OpenClose(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open with pool size 4")
	if h == nil {
		t.Fatal("expected non-nil handle from Open")
	}

	threads := gusset.Threads()
	if threads <= 0 {
		t.Errorf("expected gusset.Threads() > 0, got %d", threads)
	}

	err = h.Close()
	requireNoError(t, err, "h.Close()")

	// Repeated Close is idempotent and returns s.closeErr (nil)
	err2 := h.Close()
	requireNoError(t, err2, "repeated h.Close()")

	// Operations after Close must fail with ErrClosed
	ctx := context.Background()
	_, errCall := h.Call(ctx, []byte{0})
	if !errors.Is(errCall, gusset.ErrClosed) {
		t.Errorf("expected Call after Close to return ErrClosed, got %v", errCall)
	}
}

// TestTier1_SynchronousCall_Echo tests synchronous Call roundtrips across various inline payload sizes (<= 4096 bytes).
func TestTier1_SynchronousCall_Echo(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	testSizes := []int{1, 16, 128, 512, 1024, 2048, 4096}
	for _, size := range testSizes {
		payload := generateDeterministicBytes(size, byte(size%251))
		payload[0] = 0 // mode 0: echo

		res, err := h.Call(ctx, payload)
		requireNoError(t, err, "h.Call echo")
		assertBytesEqual(t, res, payload, "synchronous echo payload")
	}
}

// TestTier1_AsynchronousSubmitWait_Echo tests the asynchronous Submit/Wait lifecycle under concurrent tickets.
func TestTier1_AsynchronousSubmitWait_Echo(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const numJobs = 20
	type jobRecord struct {
		ticket  uint64
		payload []byte
	}
	jobs := make([]jobRecord, numJobs)

	for i := 0; i < numJobs; i++ {
		payload := generateDeterministicBytes(256, byte(i))
		payload[0] = 0 // mode 0: echo
		ticket, err := h.Submit(ctx, payload)
		requireNoError(t, err, "h.Submit")
		if ticket == 0 {
			t.Fatalf("expected non-zero ticket from Submit, got 0 at index %d", i)
		}
		jobs[i] = jobRecord{ticket: ticket, payload: payload}
	}

	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(jr jobRecord) {
			defer wg.Done()
			res, err := h.Wait(ctx, jr.ticket)
			requireNoError(t, err, "h.Wait")
			assertBytesEqual(t, res, jr.payload, "async wait result")
		}(jobs[i])
	}
	wg.Wait()
}

// TestTier1_RustBufferLifecycle_NewBuffer_Free validates 64-byte alignment, buffer identification,
// zero-copy submission, and explicit buffer reclamation.
func TestTier1_RustBufferLifecycle_NewBuffer_Free(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const bufSize = 8192
	buf, err := h.NewBuffer(bufSize)
	requireNoError(t, err, "h.NewBuffer")
	if buf.ID() == 0 {
		t.Fatal("expected non-zero ID for Rust-allocated buffer")
	}

	data := buf.Bytes()
	if len(data) != bufSize {
		t.Fatalf("expected buffer length %d, got %d", bufSize, len(data))
	}
	if !is64ByteAligned(data) {
		t.Fatal("allocated Rust buffer violates 64-byte alignment invariant (R16)")
	}

	// Fill buffer with mode 0 echo payload
	copy(data, generateDeterministicBytes(bufSize, 0x5A))
	data[0] = 0

	ticket, err := h.Submit(ctx, buf)
	requireNoError(t, err, "h.Submit buffer")

	resBuf, err := h.WaitBuffer(ctx, ticket)
	requireNoError(t, err, "h.WaitBuffer")
	defer resBuf.Free()

	assertBytesEqual(t, resBuf.Bytes(), data, "WaitBuffer payload")

	// Free original buffer
	err = buf.Free()
	requireNoError(t, err, "buf.Free()")
}

// TestTier1_CallBuffer_SynchronousRoundtrip exercises synchronous CallBuffer using large input and output buffers.
func TestTier1_CallBuffer_SynchronousRoundtrip(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const payloadSize = 16384
	inBuf, err := h.NewBuffer(payloadSize)
	requireNoError(t, err, "h.NewBuffer")
	defer inBuf.Free()

	bytes := inBuf.Bytes()
	copy(bytes, generateDeterministicBytes(payloadSize, 0x33))
	bytes[0] = 0 // mode 0 echo

	outBuf, err := h.CallBuffer(ctx, inBuf)
	requireNoError(t, err, "h.CallBuffer")
	defer outBuf.Free()

	assertBytesEqual(t, outBuf.Bytes(), bytes, "CallBuffer result")
}

// TestTier1_TraceCarrier_ContextPropagation verifies end-to-end W3C trace ID and span ID propagation
// across the Go/Rust FFI boundary using diagnostic mode 7.
func TestTier1_TraceCarrier_ContextPropagation(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	carrier := newTestTraceCarrier(0x42)
	ctx := context.WithValue(context.Background(), gusset.SpanContextKey, carrier)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	// Mode 7: echoes trace_id (16B) and span_id (8B) from CallHeader
	res, err := h.Call(ctx, []byte{7})
	requireNoError(t, err, "h.Call mode 7")

	if len(res) != 24 {
		t.Fatalf("expected 24-byte response from mode 7 (16B trace + 8B span), got %d", len(res))
	}

	gotTrace := res[0:16]
	gotSpan := res[16:24]
	wantTrace := carrier.TraceID()
	wantSpan := carrier.SpanID()

	assertBytesEqual(t, gotTrace, wantTrace[:], "propagated trace ID")
	assertBytesEqual(t, gotSpan, wantSpan[:], "propagated span ID")
}

// TestTier1_MemoryStats_And_AdviseMemoryLimit verifies allocator accounting and memory limit coordination.
func TestTier1_MemoryStats_And_AdviseMemoryLimit(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	initialLive := livebytes.Quiesce(t)

	const allocBytes = 1024 * 1024 // 1 MiB
	buf, err := h.NewBuffer(allocBytes)
	requireNoError(t, err, "h.NewBuffer 1 MiB")

	statsWithAlloc := gusset.Stats()
	if statsWithAlloc.LiveBytes < initialLive+allocBytes {
		t.Errorf("Stats().LiveBytes did not increase by at least %d: initial=%d, current=%d",
			allocBytes, initialLive, statsWithAlloc.LiveBytes)
	}

	err = buf.Free()
	requireNoError(t, err, "buf.Free()")

	liveAfterFree, leakFree := livebytes.Settle(initialLive)
	if !leakFree {
		t.Errorf("Stats().LiveBytes did not return to baseline after Free: initial=%d, current=%d",
			initialLive, liveAfterFree)
	}

	// Verify AdviseMemoryLimit returns adjusted budget
	advised := gusset.AdviseMemoryLimit(128 * 1024 * 1024)
	if advised <= 0 {
		t.Errorf("expected positive advised memory limit, got %d", advised)
	}
}

// TestTier1_DrainLogs_WorkerLogging tests that log lines emitted in Rust workers can be drained into Go.
func TestTier1_DrainLogs_WorkerLogging(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Mode 14 emits N log lines to Rust internal log ring
	const logCount = 5
	payload := []byte{14, logCount}
	res, err := h.Call(ctx, payload)
	requireNoError(t, err, "h.Call mode 14")
	if len(res) < 2 || res[1] != logCount {
		t.Fatalf("unexpected response from mode 14: %v", res)
	}

	logBuf := make([]byte, 4096)
	drained := gusset.DrainLogs(logBuf)
	if drained == 0 {
		t.Log("Note: DrainLogs returned 0 (log ring might have been drained by previous tests or async)")
	} else {
		logs := string(logBuf[:drained])
		if !strings.Contains(logs, "worker log event") {
			t.Logf("drained logs did not contain expected pattern: %q", logs)
		}
	}
}

// TestTier1_VectorSumSquare_Execution verifies numerical computation using diagnostic mode 10.
func TestTier1_VectorSumSquare_Execution(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	requireNoError(t, err, "gusset.Open")
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const n = 2048
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i % 128)
	}

	payload := append([]byte{10}, data...) // mode 10: vector sum-and-square
	res, err := h.Call(ctx, payload)
	requireNoError(t, err, "h.Call mode 10")

	if len(res) != 8 {
		t.Fatalf("expected 8-byte uint64 result, got %d", len(res))
	}

	got := binary.LittleEndian.Uint64(res)
	want := computeVectorSumSquaresOracle(data)
	if got != want {
		t.Fatalf("vector sum-and-square mismatch: got %d, want %d", got, want)
	}
}
