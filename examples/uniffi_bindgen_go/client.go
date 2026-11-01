package uniffi_bindgen_go

import (
	"context"
	"fmt"
	"time"
)

// Function IDs / opcodes for UniFFI dispatch
const (
	FnCalculate      uint8 = 1
	FnTransformText  uint8 = 2
	FnExecuteSlowJob uint8 = 3
	FnTriggerPanic   uint8 = 4
)

// Status codes returned by the UniFFI engine
const (
	StatusSuccess uint8 = 0
	StatusError   uint8 = 1
)

// Client implements MathServiceClient by marshalling requests to RustBuffer format
// and delivering them across the Gusset transport.
type Client struct {
	transport Transport
}

// NewClient returns a new Client configured with transport.
func NewClient(transport Transport) *Client {
	return &Client{transport: transport}
}

// Calculate sends an arithmetic request to the engine.
func (c *Client) Calculate(ctx context.Context, req CalculateRequest) (*CalculateResponse, error) {
	w := NewWriter(32)
	w.WriteUint8(FnCalculate)
	w.WriteString(string(req.Op))
	w.WriteFloat64(req.A)
	w.WriteFloat64(req.B)

	out, err := c.transport.Call(ctx, w.Bytes())
	if err != nil {
		return nil, err
	}

	r := NewReader(out)
	status, err := r.ReadUint8()
	if err != nil {
		return nil, err
	}

	if status == StatusError {
		code, err := r.ReadInt32()
		if err != nil {
			return nil, err
		}
		msg, err := r.ReadString()
		if err != nil {
			return nil, err
		}
		return nil, &MathError{Code: MathErrorCode(code), Message: msg}
	}

	result, err := r.ReadFloat64()
	if err != nil {
		return nil, err
	}
	opsTotal, err := r.ReadInt64()
	if err != nil {
		return nil, err
	}

	return &CalculateResponse{
		Result:          result,
		OperationsTotal: opsTotal,
	}, nil
}

// TransformText sends a string manipulation request to the engine.
func (c *Client) TransformText(ctx context.Context, req TextTransformRequest) (*TextTransformResponse, error) {
	w := NewWriter(16 + len(req.Text))
	w.WriteUint8(FnTransformText)
	w.WriteString(req.Text)
	w.WriteBool(req.ToUppercase)
	w.WriteBool(req.Reverse)

	out, err := c.transport.Call(ctx, w.Bytes())
	if err != nil {
		return nil, err
	}

	r := NewReader(out)
	status, err := r.ReadUint8()
	if err != nil {
		return nil, err
	}

	if status == StatusError {
		msg, err := r.ReadString()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("uniffi transform error: %s", msg)
	}

	transformed, err := r.ReadString()
	if err != nil {
		return nil, err
	}
	length, err := r.ReadInt32()
	if err != nil {
		return nil, err
	}

	return &TextTransformResponse{
		Transformed: transformed,
		Length:      length,
	}, nil
}

// ExecuteSlowJob tests cooperative cancellation and deadlines.
func (c *Client) ExecuteSlowJob(ctx context.Context, delay time.Duration) (*SlowJobResponse, error) {
	w := NewWriter(8)
	w.WriteUint8(FnExecuteSlowJob)
	w.WriteInt32(int32(delay.Milliseconds()))

	out, err := c.transport.Call(ctx, w.Bytes())
	if err != nil {
		return nil, err
	}

	r := NewReader(out)
	status, err := r.ReadUint8()
	if err != nil {
		return nil, err
	}

	if status == StatusError {
		msg, err := r.ReadString()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("uniffi slow job error: %s", msg)
	}

	elapsed, err := r.ReadInt64()
	if err != nil {
		return nil, err
	}
	st, err := r.ReadString()
	if err != nil {
		return nil, err
	}

	return &SlowJobResponse{
		ElapsedMillis: elapsed,
		Status:        st,
	}, nil
}

// TriggerPanic induces a deliberate panic in the engine to test the Gusset panic firewall.
func (c *Client) TriggerPanic(ctx context.Context) error {
	w := NewWriter(4)
	w.WriteUint8(FnTriggerPanic)

	_, err := c.transport.Call(ctx, w.Bytes())
	return err
}
