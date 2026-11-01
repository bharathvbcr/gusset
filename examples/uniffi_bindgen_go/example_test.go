package uniffi_bindgen_go_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
	uniffi "github.com/bharathvbcr/gusset/examples/uniffi_bindgen_go"
)

// In-memory reference dispatcher executing the exact logic of the Rust engine.
// This allows testing the wire serialization, typed client, and error handling
// in pure Go alongside the Gusset transport integration tests.
type referenceEngineTransport struct {
	mu        sync.Mutex
	opsCount  int64
	poisoned  bool
	isClosed  bool
	sleepHook func(ctx context.Context, d time.Duration) error
}

func newReferenceEngineTransport() *referenceEngineTransport {
	return &referenceEngineTransport{}
}

func (t *referenceEngineTransport) Call(ctx context.Context, payload []byte) ([]byte, error) {
	t.mu.Lock()
	if t.isClosed {
		t.mu.Unlock()
		return nil, uniffi.ErrTransportClosed
	}
	if t.poisoned {
		t.mu.Unlock()
		return nil, uniffi.ErrHandlePoisoned
	}
	t.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if len(payload) == 0 {
		return nil, errors.New("empty payload")
	}

	fnID := payload[0]
	r := uniffi.NewReader(payload[1:])
	w := uniffi.NewWriter(32)

	switch fnID {
	case uniffi.FnCalculate:
		op, err := r.ReadString()
		if err != nil {
			return nil, err
		}
		a, err := r.ReadFloat64()
		if err != nil {
			return nil, err
		}
		b, err := r.ReadFloat64()
		if err != nil {
			return nil, err
		}

		switch op {
		case string(uniffi.OpAdd):
			t.mu.Lock()
			t.opsCount++
			cnt := t.opsCount
			t.mu.Unlock()

			w.WriteUint8(uniffi.StatusSuccess)
			w.WriteFloat64(a + b)
			w.WriteInt64(cnt)
		case string(uniffi.OpSubtract):
			t.mu.Lock()
			t.opsCount++
			cnt := t.opsCount
			t.mu.Unlock()

			w.WriteUint8(uniffi.StatusSuccess)
			w.WriteFloat64(a - b)
			w.WriteInt64(cnt)
		case string(uniffi.OpMultiply):
			t.mu.Lock()
			t.opsCount++
			cnt := t.opsCount
			t.mu.Unlock()

			w.WriteUint8(uniffi.StatusSuccess)
			w.WriteFloat64(a * b)
			w.WriteInt64(cnt)
		case string(uniffi.OpDivide):
			if b == 0.0 {
				w.WriteUint8(uniffi.StatusError)
				w.WriteInt32(int32(uniffi.MathErrDivisionByZero))
				w.WriteString("division by zero")
			} else {
				t.mu.Lock()
				t.opsCount++
				cnt := t.opsCount
				t.mu.Unlock()

				w.WriteUint8(uniffi.StatusSuccess)
				w.WriteFloat64(a / b)
				w.WriteInt64(cnt)
			}
		case string(uniffi.OpSqrt):
			if a < 0.0 {
				w.WriteUint8(uniffi.StatusError)
				w.WriteInt32(int32(uniffi.MathErrNegativeRoot))
				w.WriteString("cannot compute square root of negative number")
			} else {
				t.mu.Lock()
				t.opsCount++
				cnt := t.opsCount
				t.mu.Unlock()

				w.WriteUint8(uniffi.StatusSuccess)
				w.WriteFloat64(math.Sqrt(a))
				w.WriteInt64(cnt)
			}
		default:
			w.WriteUint8(uniffi.StatusError)
			w.WriteInt32(int32(uniffi.MathErrUnknownOp))
			w.WriteString("unsupported operation: " + op)
		}
		return w.Bytes(), nil

	case uniffi.FnTransformText:
		text, err := r.ReadString()
		if err != nil {
			return nil, err
		}
		toUpper, err := r.ReadBool()
		if err != nil {
			return nil, err
		}
		reverse, err := r.ReadBool()
		if err != nil {
			return nil, err
		}

		res := text
		if toUpper {
			res = makeUpper(res)
		}
		if reverse {
			res = makeReverse(res)
		}

		w.WriteUint8(uniffi.StatusSuccess)
		w.WriteString(res)
		w.WriteInt32(int32(len(res)))
		return w.Bytes(), nil

	case uniffi.FnExecuteSlowJob:
		delayMs, err := r.ReadInt32()
		if err != nil {
			return nil, err
		}
		delay := time.Duration(delayMs) * time.Millisecond
		if t.sleepHook != nil {
			if err := t.sleepHook(ctx, delay); err != nil {
				return nil, err
			}
		} else {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		w.WriteUint8(uniffi.StatusSuccess)
		w.WriteInt64(int64(delayMs))
		w.WriteString("completed")
		return w.Bytes(), nil

	case uniffi.FnTriggerPanic:
		t.mu.Lock()
		t.poisoned = true
		t.mu.Unlock()
		return nil, uniffi.ErrEnginePanic

	default:
		return nil, errors.New("unsupported function opcode")
	}
}

func (t *referenceEngineTransport) Submit(ctx context.Context, payload []byte) (uint64, error) {
	return 1, nil
}

func (t *referenceEngineTransport) Wait(ctx context.Context, ticket uint64) ([]byte, error) {
	return nil, nil
}

func (t *referenceEngineTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.isClosed = true
	return nil
}

func makeUpper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 32
		}
	}
	return string(b)
}

func makeReverse(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

func TestRustBuffer_BinarySerialization(t *testing.T) {
	w := uniffi.NewWriter(64)
	w.WriteInt8(-42)
	w.WriteUint8(250)
	w.WriteInt16(-1234)
	w.WriteInt32(9876543)
	w.WriteUint32(4294967290)
	w.WriteInt64(-9223372036854775800)
	w.WriteFloat64(3.141592653589793)
	w.WriteBool(true)
	w.WriteBool(false)
	w.WriteString("hello from gusset uniffi")
	w.WriteBytes([]byte{0xDE, 0xAD, 0xBE, 0xEF})

	r := uniffi.NewReader(w.Bytes())

	if v, err := r.ReadInt8(); err != nil || v != -42 {
		t.Fatalf("ReadInt8: got %v, err %v", v, err)
	}
	if v, err := r.ReadUint8(); err != nil || v != 250 {
		t.Fatalf("ReadUint8: got %v, err %v", v, err)
	}
	if v, err := r.ReadInt16(); err != nil || v != -1234 {
		t.Fatalf("ReadInt16: got %v, err %v", v, err)
	}
	if v, err := r.ReadInt32(); err != nil || v != 9876543 {
		t.Fatalf("ReadInt32: got %v, err %v", v, err)
	}
	if v, err := r.ReadUint32(); err != nil || v != 4294967290 {
		t.Fatalf("ReadUint32: got %v, err %v", v, err)
	}
	if v, err := r.ReadInt64(); err != nil || v != -9223372036854775800 {
		t.Fatalf("ReadInt64: got %v, err %v", v, err)
	}
	if v, err := r.ReadFloat64(); err != nil || math.Abs(v-3.141592653589793) > 1e-12 {
		t.Fatalf("ReadFloat64: got %v, err %v", v, err)
	}
	if v, err := r.ReadBool(); err != nil || !v {
		t.Fatalf("ReadBool(true): got %v, err %v", v, err)
	}
	if v, err := r.ReadBool(); err != nil || v {
		t.Fatalf("ReadBool(false): got %v, err %v", v, err)
	}
	if v, err := r.ReadString(); err != nil || v != "hello from gusset uniffi" {
		t.Fatalf("ReadString: got %q, err %v", v, err)
	}
	b, err := r.ReadBytes()
	if err != nil || len(b) != 4 || b[0] != 0xDE || b[1] != 0xAD || b[2] != 0xBE || b[3] != 0xEF {
		t.Fatalf("ReadBytes: got %x, err %v", b, err)
	}

	if r.Remaining() != 0 {
		t.Fatalf("expected 0 remaining bytes, got %d", r.Remaining())
	}

	// Underflow assertion
	if _, err := r.ReadInt32(); !errors.Is(err, uniffi.ErrBufferUnderflow) {
		t.Fatalf("expected ErrBufferUnderflow, got %v", err)
	}
}

func TestGussetUniFFI_Calculate_Success(t *testing.T) {
	transport := newReferenceEngineTransport()
	client := uniffi.NewClient(transport)
	ctx := context.Background()

	// 1. Add
	resp, err := client.Calculate(ctx, uniffi.CalculateRequest{
		Op: uniffi.OpAdd,
		A:  10.5,
		B:  2.5,
	})
	if err != nil {
		t.Fatalf("Calculate(Add): %v", err)
	}
	if resp.Result != 13.0 || resp.OperationsTotal != 1 {
		t.Fatalf("Calculate(Add): unexpected result %+v", resp)
	}

	// 2. Multiply
	resp, err = client.Calculate(ctx, uniffi.CalculateRequest{
		Op: uniffi.OpMultiply,
		A:  7.0,
		B:  6.0,
	})
	if err != nil {
		t.Fatalf("Calculate(Multiply): %v", err)
	}
	if resp.Result != 42.0 || resp.OperationsTotal != 2 {
		t.Fatalf("Calculate(Multiply): unexpected result %+v", resp)
	}

	// 3. Sqrt
	resp, err = client.Calculate(ctx, uniffi.CalculateRequest{
		Op: uniffi.OpSqrt,
		A:  144.0,
	})
	if err != nil {
		t.Fatalf("Calculate(Sqrt): %v", err)
	}
	if resp.Result != 12.0 || resp.OperationsTotal != 3 {
		t.Fatalf("Calculate(Sqrt): unexpected result %+v", resp)
	}
}

func TestGussetUniFFI_Calculate_TypedErrors(t *testing.T) {
	transport := newReferenceEngineTransport()
	client := uniffi.NewClient(transport)
	ctx := context.Background()

	// 1. Division by zero
	_, err := client.Calculate(ctx, uniffi.CalculateRequest{
		Op: uniffi.OpDivide,
		A:  100.0,
		B:  0.0,
	})
	if err == nil {
		t.Fatal("expected error on division by zero, got nil")
	}
	var mathErr *uniffi.MathError
	if !errors.As(err, &mathErr) {
		t.Fatalf("expected *MathError, got %T: %v", err, err)
	}
	if mathErr.Code != uniffi.MathErrDivisionByZero {
		t.Fatalf("expected MathErrDivisionByZero (%d), got %d", uniffi.MathErrDivisionByZero, mathErr.Code)
	}

	// 2. Square root of negative number
	_, err = client.Calculate(ctx, uniffi.CalculateRequest{
		Op: uniffi.OpSqrt,
		A:  -49.0,
	})
	if err == nil {
		t.Fatal("expected error on negative sqrt, got nil")
	}
	if !errors.As(err, &mathErr) {
		t.Fatalf("expected *MathError, got %T: %v", err, err)
	}
	if mathErr.Code != uniffi.MathErrNegativeRoot {
		t.Fatalf("expected MathErrNegativeRoot (%d), got %d", uniffi.MathErrNegativeRoot, mathErr.Code)
	}
}

func TestGussetUniFFI_TransformText(t *testing.T) {
	transport := newReferenceEngineTransport()
	client := uniffi.NewClient(transport)
	ctx := context.Background()

	resp, err := client.TransformText(ctx, uniffi.TextTransformRequest{
		Text:        "gusset-uniffi",
		ToUppercase: true,
		Reverse:     true,
	})
	if err != nil {
		t.Fatalf("TransformText: %v", err)
	}
	if resp.Transformed != "IFFINU-TESSUG" || resp.Length != 13 {
		t.Fatalf("TransformText: got %q (len %d), want %q (len 13)", resp.Transformed, resp.Length, "IFFINU-TESSUG")
	}
}

func TestGussetUniFFI_ContextDeadlineCancellation(t *testing.T) {
	transport := newReferenceEngineTransport()
	client := uniffi.NewClient(transport)

	// Set short deadline
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	// Request job holding 200ms
	_, err := client.ExecuteSlowJob(ctx, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected context deadline error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestGussetUniFFI_PanicFirewallContainment(t *testing.T) {
	transport := newReferenceEngineTransport()
	client := uniffi.NewClient(transport)
	ctx := context.Background()

	// Trigger deliberate panic
	err := client.TriggerPanic(ctx)
	if err == nil {
		t.Fatal("expected panic error, got nil")
	}
	if !errors.Is(err, uniffi.ErrEnginePanic) {
		t.Fatalf("expected ErrEnginePanic, got %v", err)
	}

	// Subsequent operations on the poisoned handle must be refused
	_, err = client.Calculate(ctx, uniffi.CalculateRequest{Op: uniffi.OpAdd, A: 1, B: 2})
	if err == nil {
		t.Fatal("expected ErrHandlePoisoned on poisoned transport, got nil")
	}
	if !errors.Is(err, uniffi.ErrHandlePoisoned) {
		t.Fatalf("expected ErrHandlePoisoned, got %v", err)
	}
}

func TestGussetUniFFI_GussetHandle_RealIntegration(t *testing.T) {
	// Initialize a genuine Gusset runtime handle with 4 workers and diagnostic engine
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	transport := uniffi.NewGussetTransport(h)
	defer transport.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Diagnostic Echo test (diagnostic mode 0 echoes input back)
	echoPayload := []byte{0, 'u', 'n', 'i', 'f', 'f', 'i'}
	res, err := transport.Call(ctx, echoPayload)
	if err != nil {
		t.Fatalf("GussetTransport.Call(echo): %v", err)
	}
	if string(res[1:]) != "uniffi" {
		t.Fatalf("GussetTransport.Call(echo): got %q, want %q", string(res[1:]), "uniffi")
	}

	// 2. Async Submit and Wait test
	ticket, err := transport.Submit(ctx, echoPayload)
	if err != nil {
		t.Fatalf("GussetTransport.Submit: %v", err)
	}
	waitRes, err := transport.Wait(ctx, ticket)
	if err != nil {
		t.Fatalf("GussetTransport.Wait: %v", err)
	}
	if string(waitRes[1:]) != "uniffi" {
		t.Fatalf("GussetTransport.Wait: got %q, want %q", string(waitRes[1:]), "uniffi")
	}

	// 3. Deadline Enforcement in real Gusset runtime:
	// Diagnostic mode 5 holds work and cooperatively checks cancellation.
	// Mode 5 with argument 20 holds 200 ms.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer shortCancel()

	holdPayload := []byte{5, 20}
	_, err = transport.Call(shortCtx, holdPayload)
	if err == nil {
		t.Fatal("expected deadline exceeded from Gusset, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected DeadlineExceeded / Canceled, got %v", err)
	}

	// 4. Panic Firewall Containment in real Gusset runtime:
	// Open a separate handle for panic testing because panic poisions the handle.
	panicHandle, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open panicHandle: %v", err)
	}
	defer panicHandle.Close()

	panicTransport := uniffi.NewGussetTransport(panicHandle)
	defer panicTransport.Close()

	// Mode 1 triggers a deliberate panic in Rust ("plain panic")
	_, err = panicTransport.Call(ctx, []byte{1})
	if err == nil {
		t.Fatal("expected panic error from Gusset, got nil")
	}
	if !errors.Is(err, uniffi.ErrEnginePanic) && !errors.Is(err, gusset.ErrPanic) {
		t.Fatalf("expected ErrEnginePanic / ErrPanic, got %v", err)
	}

	// Verify handle poisoning on next call
	_, err = panicTransport.Call(ctx, []byte{0, 'h', 'i'})
	if err == nil {
		t.Fatal("expected poisoned error, got nil")
	}
	if !errors.Is(err, uniffi.ErrHandlePoisoned) && !errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("expected ErrHandlePoisoned / ErrPoisoned, got %v", err)
	}

	// 5. Recovery pattern: Close poisoned handle, open fresh handle
	_ = panicTransport.Close()
	recoveredHandle, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open recoveredHandle: %v", err)
	}
	defer recoveredHandle.Close()

	recoveredTransport := uniffi.NewGussetTransport(recoveredHandle)
	defer recoveredTransport.Close()

	recRes, err := recoveredTransport.Call(ctx, []byte{0, 'o', 'k'})
	if err != nil {
		t.Fatalf("Call on recovered transport failed: %v", err)
	}
	if string(recRes[1:]) != "ok" {
		t.Fatalf("unexpected recovered result: %q", string(recRes[1:]))
	}
}

func TestGussetUniFFI_HighConcurrency(t *testing.T) {
	// Verify that 100 concurrent goroutines executing UniFFI calls through Gusset
	// operate smoothly with bounded worker threads (no thread exhaustion).
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	transport := uniffi.NewGussetTransport(h)
	defer transport.Close()

	const concurrency = 100
	var wg sync.WaitGroup
	wg.Add(concurrency)

	errCh := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(id int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			payload := []byte{0, byte('A' + (id % 26))}
			res, err := transport.Call(ctx, payload)
			if err != nil {
				errCh <- err
				return
			}
			if len(res) != 2 || res[1] != byte('A'+(id%26)) {
				errCh <- errors.New("mismatched response in concurrent call")
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent execution failure: %v", err)
	}
}
