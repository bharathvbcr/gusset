package uniffi_bindgen_go_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
	uniffi "github.com/bharathvbcr/gusset/examples/uniffi_bindgen_go"
)

// mockTransport allows injecting arbitrary raw payloads to test Client robustness.
type mockTransport struct {
	response []byte
	err      error
}

func (m *mockTransport) Call(ctx context.Context, payload []byte) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

func (m *mockTransport) Submit(ctx context.Context, payload []byte) (uint64, error) {
	return 1, m.err
}

func (m *mockTransport) Wait(ctx context.Context, ticket uint64) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

func (m *mockTransport) Close() error {
	return nil
}

// 1. Attack RustBufferReader with truncated buffers, underflow, and corrupt fields.
func TestAdversarial_RustBufferReaderMalformedPayloads(t *testing.T) {
	// Test Underflow on all primitive types with 0-byte buffer
	emptyReader := uniffi.NewReader([]byte{})
	if _, err := emptyReader.ReadInt8(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
		t.Fatalf("ReadInt8 on empty: got %v, want ErrBufferUnderflow", err)
	}
	if _, err := emptyReader.ReadUint8(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
		t.Fatalf("ReadUint8 on empty: got %v, want ErrBufferUnderflow", err)
	}
	if _, err := emptyReader.ReadInt16(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
		t.Fatalf("ReadInt16 on empty: got %v, want ErrBufferUnderflow", err)
	}
	if _, err := emptyReader.ReadInt32(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
		t.Fatalf("ReadInt32 on empty: got %v, want ErrBufferUnderflow", err)
	}
	if _, err := emptyReader.ReadUint32(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
		t.Fatalf("ReadUint32 on empty: got %v, want ErrBufferUnderflow", err)
	}
	if _, err := emptyReader.ReadInt64(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
		t.Fatalf("ReadInt64 on empty: got %v, want ErrBufferUnderflow", err)
	}
	if _, err := emptyReader.ReadFloat64(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
		t.Fatalf("ReadFloat64 on empty: got %v, want ErrBufferUnderflow", err)
	}
	if _, err := emptyReader.ReadBool(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
		t.Fatalf("ReadBool on empty: got %v, want ErrBufferUnderflow", err)
	}
	if _, err := emptyReader.ReadString(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
		t.Fatalf("ReadString on empty: got %v, want ErrBufferUnderflow", err)
	}
	if _, err := emptyReader.ReadBytes(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
		t.Fatalf("ReadBytes on empty: got %v, want ErrBufferUnderflow", err)
	}

	// Truncated buffers (partial reads)
	partial1 := []byte{0x01}
	r1 := uniffi.NewReader(partial1)
	if _, err := r1.ReadInt16(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
		t.Fatalf("ReadInt16 with 1 byte: got %v, want ErrBufferUnderflow", err)
	}

	partial3 := []byte{0x00, 0x01, 0x02}
	r3 := uniffi.NewReader(partial3)
	if _, err := r3.ReadInt32(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
		t.Fatalf("ReadInt32 with 3 bytes: got %v, want ErrBufferUnderflow", err)
	}

	partial7 := []byte{0, 1, 2, 3, 4, 5, 6}
	r7 := uniffi.NewReader(partial7)
	if _, err := r7.ReadInt64(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
		t.Fatalf("ReadInt64 with 7 bytes: got %v, want ErrBufferUnderflow", err)
	}
	r7b := uniffi.NewReader(partial7)
	if _, err := r7b.ReadFloat64(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
		t.Fatalf("ReadFloat64 with 7 bytes: got %v, want ErrBufferUnderflow", err)
	}

	// Negative length in ReadString & ReadBytes
	for _, neg := range []int32{-1, -100, math.MinInt32} {
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], uint32(neg))

		rNegStr := uniffi.NewReader(buf[:])
		if _, err := rNegStr.ReadString(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
			t.Fatalf("ReadString with negative length %d: got %v, want ErrBufferUnderflow", neg, err)
		}

		rNegBytes := uniffi.NewReader(buf[:])
		if _, err := rNegBytes.ReadBytes(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
			t.Fatalf("ReadBytes with negative length %d: got %v, want ErrBufferUnderflow", neg, err)
		}
	}

	// Oversized length claiming more bytes than remain
	for _, excessive := range []int32{10, 1000, math.MaxInt32} {
		var buf [8]byte
		binary.BigEndian.PutUint32(buf[:4], uint32(excessive))
		copy(buf[4:], []byte("abc")) // only 4 bytes of data follow

		rExcStr := uniffi.NewReader(buf[:])
		if _, err := rExcStr.ReadString(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
			t.Fatalf("ReadString with excessive length %d: got %v, want ErrBufferUnderflow", excessive, err)
		}

		rExcBytes := uniffi.NewReader(buf[:])
		if _, err := rExcBytes.ReadBytes(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
			t.Fatalf("ReadBytes with excessive length %d: got %v, want ErrBufferUnderflow", excessive, err)
		}
	}
}

// 2. Attack Client methods with malformed, truncated, and corrupt server responses.
func TestAdversarial_ClientMalformedResponses(t *testing.T) {
	ctx := context.Background()

	malformedResponses := [][]byte{
		{},                                      // completely empty
		{0},                                     // status success but missing fields
		{1},                                     // status error but missing code and message
		{1, 0, 0, 0, 2},                         // status error + code, but missing message length
		{1, 0, 0, 0, 2, 0, 0, 0, 5},             // status error + code + length 5, but missing message bytes
		{1, 0, 0, 0, 2, 0xFF, 0xFF, 0xFF, 0xFF}, // status error + code + negative length
		{0, 0, 0, 0, 0},                         // status success + 4 bytes (needs 8 for float64)
		{0, 0, 0, 0, 0, 0, 0, 0},                // status success + empty string (4 bytes len) + only 3 bytes for int32/int64
		{0, 0, 0, 0, 5},                         // status success + string length 5, but 0 bytes available
		{99},                                    // completely unrecognized status code
		{255, 255, 255, 255},                    // all 0xFF bytes
	}

	for i, badResp := range malformedResponses {
		t.Run(fmt.Sprintf("Calculate_case_%d", i), func(t *testing.T) {
			mock := &mockTransport{response: badResp}
			client := uniffi.NewClient(mock)
			_, err := client.Calculate(ctx, uniffi.CalculateRequest{Op: uniffi.OpAdd, A: 1, B: 2})
			if err == nil {
				t.Fatalf("Calculate should have failed on malformed response #%d, got success", i)
			}
		})

		t.Run(fmt.Sprintf("TransformText_case_%d", i), func(t *testing.T) {
			mock := &mockTransport{response: badResp}
			client := uniffi.NewClient(mock)
			_, err := client.TransformText(ctx, uniffi.TextTransformRequest{Text: "test"})
			if err == nil {
				t.Fatalf("TransformText should have failed on malformed response #%d, got success", i)
			}
		})

		t.Run(fmt.Sprintf("ExecuteSlowJob_case_%d", i), func(t *testing.T) {
			mock := &mockTransport{response: badResp}
			client := uniffi.NewClient(mock)
			_, err := client.ExecuteSlowJob(ctx, 10*time.Millisecond)
			if err == nil {
				t.Fatalf("ExecuteSlowJob should have failed on malformed response #%d, got success", i)
			}
		})
	}
}

// 3. Attack real Gusset runtime handle via GussetTransport with adversarial payloads.
func TestAdversarial_RealGussetTransportFuzz(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	transport := uniffi.NewGussetTransport(h)
	defer transport.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	adversarialPayloads := [][]byte{
		{},                        // empty payload
		{255},                     // non-existent diagnostic opcode
		{254, 1, 2, 3, 4},         // another unknown opcode
		{0},                       // diagnostic echo with 0 bytes to echo
		{0, 0xFF, 0xFE, 0xFD},     // echo with arbitrary binary bytes
		make([]byte, 1024*1024),   // 1 MiB zero buffer
		make([]byte, 2*1024*1024), // 2 MiB zero buffer
	}

	for i, payload := range adversarialPayloads {
		res, err := transport.Call(ctx, payload)
		if err != nil {
			// Gusset returning an error for invalid input is completely safe and expected.
			continue
		}
		// If echo mode (mode 0), verify it returned safely
		if len(payload) > 0 && payload[0] == 0 {
			if len(res) != len(payload) {
				t.Fatalf("payload #%d: expected echo len %d, got %d", i, len(payload), len(res))
			}
		}
	}
}

// 4. Extreme Concurrency Stress Test: 300 goroutines pounding real Gusset runtime
// with a mix of synchronous Call, asynchronous Submit, and Wait.
func TestAdversarial_ExtremeConcurrency_RealGusset(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(8), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	transport := uniffi.NewGussetTransport(h)
	defer transport.Close()

	const numGoroutines = 300
	const opsPerGoroutine = 10

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	errCh := make(chan error, numGoroutines*opsPerGoroutine)

	for g := 0; g < numGoroutines; g++ {
		go func(gid int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			for op := 0; op < opsPerGoroutine; op++ {
				switch op % 3 {
				case 0:
					// Sync Call echo
					input := []byte{0, byte(gid % 256), byte(op % 256)}
					out, err := transport.Call(ctx, input)
					if err != nil {
						errCh <- fmt.Errorf("sync call gid %d op %d: %w", gid, op, err)
						return
					}
					if len(out) != 3 || out[1] != byte(gid%256) || out[2] != byte(op%256) {
						errCh <- fmt.Errorf("sync call gid %d op %d: mismatch %v vs %v", gid, op, out, input)
						return
					}
				case 1:
					// Async Submit and Wait
					input := []byte{0, byte((gid + 1) % 256), byte((op + 1) % 256)}
					ticket, err := transport.Submit(ctx, input)
					if err != nil {
						errCh <- fmt.Errorf("submit gid %d op %d: %w", gid, op, err)
						return
					}
					out, err := transport.Wait(ctx, ticket)
					if err != nil {
						errCh <- fmt.Errorf("wait gid %d op %d: %w", gid, op, err)
						return
					}
					if len(out) != 3 || out[1] != byte((gid+1)%256) || out[2] != byte((op+1)%256) {
						errCh <- fmt.Errorf("wait gid %d op %d: mismatch %v vs %v", gid, op, out, input)
						return
					}
				case 2:
					// Cooperative sleep hold (mode 5, 2 ms)
					input := []byte{5, 2}
					_, err := transport.Call(ctx, input)
					if err != nil {
						errCh <- fmt.Errorf("hold call gid %d op %d: %w", gid, op, err)
						return
					}
				}
			}
		}(g)
	}

	wg.Wait()
	close(errCh)

	var errCount int
	for err := range errCh {
		t.Errorf("concurrency error: %v", err)
		errCount++
		if errCount > 10 {
			t.Fatalf("too many concurrency errors (>10), aborting")
		}
	}
}

// 5. Context Cancellation and Deadline Stress under Heavy Contention.
func TestAdversarial_ContextCancellationUnderHeavyContention(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	transport := uniffi.NewGussetTransport(h)
	defer transport.Close()

	const workers = 50
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()

			if id%2 == 0 {
				// Immediate cancellation
				ctx, cancel := context.WithCancel(context.Background())
				cancel() // already cancelled before Call
				_, err := transport.Call(ctx, []byte{5, 10})
				if err == nil || (!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)) {
					t.Errorf("id %d: expected Canceled/DeadlineExceeded, got %v", id, err)
				}
			} else {
				// Very tight deadline (1ms on a 50ms hold)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
				defer cancel()
				_, err := transport.Call(ctx, []byte{5, 5}) // 50ms hold
				if err == nil || (!errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled)) {
					t.Errorf("id %d: expected DeadlineExceeded/Canceled, got %v", id, err)
				}
			}
		}(i)
	}

	wg.Wait()
}

// 6. Panic Firewall & Poison Recovery Stress under High Churn.
func TestAdversarial_PanicFirewallAndPoisonRecoveryStress(t *testing.T) {
	for cycle := 0; cycle < 5; cycle++ {
		h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("cycle %d: Open failed: %v", cycle, err)
		}

		tr := uniffi.NewGussetTransport(h)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)

		// 1. Trigger deliberate panic (mode 1)
		_, err = tr.Call(ctx, []byte{1})
		if err == nil {
			cancel()
			h.Close()
			t.Fatalf("cycle %d: expected panic error, got nil", cycle)
		}
		if !errors.Is(err, uniffi.ErrEnginePanic) && !errors.Is(err, gusset.ErrPanic) {
			cancel()
			h.Close()
			t.Fatalf("cycle %d: expected ErrEnginePanic / ErrPanic, got %v", cycle, err)
		}

		// 2. Subsequent call must report poisoned handle
		_, err = tr.Call(ctx, []byte{0, 1, 2})
		if err == nil {
			cancel()
			h.Close()
			t.Fatalf("cycle %d: expected poisoned error, got nil", cycle)
		}
		if !errors.Is(err, uniffi.ErrHandlePoisoned) && !errors.Is(err, gusset.ErrPoisoned) {
			cancel()
			h.Close()
			t.Fatalf("cycle %d: expected ErrHandlePoisoned / ErrPoisoned, got %v", cycle, err)
		}

		cancel()
		// 3. Clean close of poisoned handle
		if err := tr.Close(); err != nil {
			t.Logf("cycle %d: close poisoned handle returned: %v (expected)", cycle, err)
		}
	}
}
