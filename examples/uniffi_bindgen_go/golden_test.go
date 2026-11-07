package uniffi_bindgen_go_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	uniffi "github.com/bharathvbcr/gusset/examples/uniffi_bindgen_go"
)

// goldenVector is one line of testdata/golden.txt. The Rust engine's own tests
// (rust/src/lib.rs, golden_vectors_match_engine_bytes) read the same file and
// compare the engine's response bytes to it, so a wire change on either side
// fails a test on that side.
type goldenVector struct {
	name     string
	request  []byte
	response []byte // nil when the file's response column is "-"
}

func loadGolden(t *testing.T) []goldenVector {
	t.Helper()
	raw, err := os.ReadFile("testdata/golden.txt")
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var out []goldenVector
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Fields(line)
		if len(cols) != 3 {
			t.Fatalf("malformed golden line %q", line)
		}
		req, err := hex.DecodeString(cols[1])
		if err != nil {
			t.Fatalf("golden %s request: %v", cols[0], err)
		}
		v := goldenVector{name: cols[0], request: req}
		if cols[2] != "-" {
			if v.response, err = hex.DecodeString(cols[2]); err != nil {
				t.Fatalf("golden %s response: %v", cols[0], err)
			}
		}
		out = append(out, v)
	}
	return out
}

// recordingTransport remembers the last request and the bytes the wrapped
// transport answered with, so a test can compare both with the golden file.
type recordingTransport struct {
	uniffi.Transport
	request  []byte
	response []byte
}

func (r *recordingTransport) Call(ctx context.Context, payload []byte) ([]byte, error) {
	r.request = append([]byte(nil), payload...)
	out, err := r.Transport.Call(ctx, payload)
	r.response = append([]byte(nil), out...)
	return out, err
}

// goldenCalls maps each vector to the typed-client call that must produce its
// request bytes, plus a check of what the client decodes from the response.
var goldenCalls = map[string]func(t *testing.T, c *uniffi.Client){
	"calc_add": func(t *testing.T, c *uniffi.Client) {
		res, err := c.Calculate(context.Background(), uniffi.CalculateRequest{Op: uniffi.OpAdd, A: 1.5, B: 2.25})
		if err != nil || res.Result != 3.75 || res.OperationsTotal != 1 {
			t.Fatalf("got %+v, %v", res, err)
		}
	},
	"calc_multiply": func(t *testing.T, c *uniffi.Client) {
		res, err := c.Calculate(context.Background(), uniffi.CalculateRequest{Op: uniffi.OpMultiply, A: 3, B: 4})
		if err != nil || res.Result != 12 || res.OperationsTotal != 2 {
			t.Fatalf("got %+v, %v", res, err)
		}
	},
	"calc_divide_by_zero": func(t *testing.T, c *uniffi.Client) {
		_, err := c.Calculate(context.Background(), uniffi.CalculateRequest{Op: uniffi.OpDivide, A: 1, B: 0})
		wantMathError(t, err, uniffi.MathErrDivisionByZero, "division by zero")
	},
	"calc_sqrt_negative": func(t *testing.T, c *uniffi.Client) {
		_, err := c.Calculate(context.Background(), uniffi.CalculateRequest{Op: uniffi.OpSqrt, A: -1})
		wantMathError(t, err, uniffi.MathErrNegativeRoot, "cannot compute square root of negative number")
	},
	"calc_unknown_op": func(t *testing.T, c *uniffi.Client) {
		_, err := c.Calculate(context.Background(), uniffi.CalculateRequest{Op: uniffi.OpPower, A: 2, B: 3})
		wantMathError(t, err, uniffi.MathErrUnknownOp, "unsupported operation: Power")
	},
	"calc_subtract": func(t *testing.T, c *uniffi.Client) {
		res, err := c.Calculate(context.Background(), uniffi.CalculateRequest{Op: uniffi.OpSubtract, A: 10, B: 4})
		if err != nil || res.Result != 6 || res.OperationsTotal != 3 {
			t.Fatalf("got %+v, %v", res, err)
		}
	},
	"calc_divide": func(t *testing.T, c *uniffi.Client) {
		res, err := c.Calculate(context.Background(), uniffi.CalculateRequest{Op: uniffi.OpDivide, A: 9, B: 3})
		if err != nil || res.Result != 3 || res.OperationsTotal != 4 {
			t.Fatalf("got %+v, %v", res, err)
		}
	},
	"calc_sqrt": func(t *testing.T, c *uniffi.Client) {
		res, err := c.Calculate(context.Background(), uniffi.CalculateRequest{Op: uniffi.OpSqrt, A: 16})
		if err != nil || math.Abs(res.Result-4) > 0 || res.OperationsTotal != 5 {
			t.Fatalf("got %+v, %v", res, err)
		}
	},
	"transform_upper_reverse_unicode": func(t *testing.T, c *uniffi.Client) {
		res, err := c.TransformText(context.Background(), uniffi.TextTransformRequest{Text: "héllo", ToUppercase: true, Reverse: true})
		if err != nil || res.Transformed != "OLLÉH" || res.Length != 6 {
			t.Fatalf("got %+v, %v", res, err)
		}
	},
	"transform_plain": func(t *testing.T, c *uniffi.Client) {
		res, err := c.TransformText(context.Background(), uniffi.TextTransformRequest{Text: "abc"})
		if err != nil || res.Transformed != "abc" || res.Length != 3 {
			t.Fatalf("got %+v, %v", res, err)
		}
	},
	"transform_empty": func(t *testing.T, c *uniffi.Client) {
		res, err := c.TransformText(context.Background(), uniffi.TextTransformRequest{ToUppercase: true, Reverse: true})
		if err != nil || res.Transformed != "" || res.Length != 0 {
			t.Fatalf("got %+v, %v", res, err)
		}
	},
	"slow_job_request": func(t *testing.T, c *uniffi.Client) {
		if _, err := c.ExecuteSlowJob(context.Background(), 250*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	},
	"panic_request": func(t *testing.T, c *uniffi.Client) {
		if err := c.TriggerPanic(context.Background()); !errors.Is(err, uniffi.ErrEnginePanic) {
			t.Fatalf("got %v, want ErrEnginePanic", err)
		}
	},
}

func wantMathError(t *testing.T, err error, code uniffi.MathErrorCode, msg string) {
	t.Helper()
	var me *uniffi.MathError
	if !errors.As(err, &me) || me.Code != code || me.Message != msg {
		t.Fatalf("got %v, want MathError{%d, %q}", err, code, msg)
	}
}

// The typed client must produce the golden request bytes, and the Go reference
// engine must answer them with the golden response bytes, which the Rust engine
// is tested against. Vectors run in file order against a fresh engine because
// the OperationsTotal counter is cumulative.
func TestGolden_ClientAndReferenceEngineMatchRustBytes(t *testing.T) {
	vectors := loadGolden(t)
	if len(vectors) != len(goldenCalls) {
		t.Fatalf("golden file has %d vectors, test table has %d", len(vectors), len(goldenCalls))
	}

	ref := newReferenceEngineTransport()
	ref.sleepHook = func(context.Context, time.Duration) error { return nil }
	rec := &recordingTransport{Transport: ref}
	client := uniffi.NewClient(rec)

	compared := 0
	for _, v := range vectors {
		call, ok := goldenCalls[v.name]
		if !ok {
			t.Fatalf("golden vector %q has no entry in goldenCalls", v.name)
		}
		call(t, client)
		if !bytes.Equal(rec.request, v.request) {
			t.Fatalf("%s: client request %x, golden %x", v.name, rec.request, v.request)
		}
		if v.response != nil {
			if !bytes.Equal(rec.response, v.response) {
				t.Fatalf("%s: reference engine response %x, golden %x", v.name, rec.response, v.response)
			}
			compared++
		}
	}
	if compared < 10 {
		t.Fatalf("only %d response vectors compared", compared)
	}
}
