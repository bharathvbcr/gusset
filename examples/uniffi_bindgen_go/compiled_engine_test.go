//go:build gusset_uniffi_engine

package uniffi_bindgen_go_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
	uniffi "github.com/bharathvbcr/gusset/examples/uniffi_bindgen_go"
)

// compiledEngineOpcode is the opcode the test registers the compiled engine
// under. Nonzero, so the engine never takes the global slot the other tests'
// WithDiagnosticEngine handles fall back from.
const compiledEngineOpcode = 0x55

// compiledEngineRegistered guards the one registration this process may make.
var compiledEngineRegistered atomic.Bool

// The typed client against the compiled Rust engine, through a real Gusset
// handle with no diagnostic engine: Go -> Client -> GussetTransport ->
// gusset_submit -> the engine init_uniffi_engine registered -> back. Run by
// `make test-uniffi-link`, which builds the umbrella archive and links this
// binary against it.
func TestCompiledEngine_TypedClientMatchesGolden(t *testing.T) {
	if !compiledEngineRegistered.CompareAndSwap(false, true) {
		t.Fatal("the engine registry and its operations counter are process-wide, and the golden replay needs a fresh counter; run this test with -count=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithOpcode(compiledEngineOpcode))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()
	tr := uniffi.NewGussetTransport(h)
	client := uniffi.NewClient(tr)
	upper := uniffi.TextTransformRequest{Text: "straße", ToUppercase: true}

	// Before registration the opcode has no engine, and Gusset refuses the
	// call without poisoning the handle. So every answer below comes from the
	// engine init_uniffi_engine registered, not from a fallback.
	_, err = client.TransformText(ctx, upper)
	refusal := fmt.Sprintf("no engine handler registered for opcode %d", compiledEngineOpcode)
	if err == nil || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("before registration: got %v, want an error containing %q", err, refusal)
	}

	if err := uniffi.RegisterCompiledEngine(compiledEngineOpcode); err != nil {
		t.Fatal(err)
	}
	if err := uniffi.RegisterCompiledEngine(compiledEngineOpcode); err == nil {
		t.Fatal("a second registration of the same opcode was accepted")
	}

	// Rust's str::to_uppercase applies Unicode's special casing, ß -> SS. Go's
	// strings.ToUpper, which the Go reference engine uses, leaves ß alone and
	// answers STRAßE. Only the compiled engine gives this answer.
	res, err := client.TransformText(ctx, upper)
	if err != nil || res.Transformed != "STRASSE" || res.Length != 7 {
		t.Fatalf("TransformText(straße, upper): got %+v, %v; want STRASSE, length 7", res, err)
	}

	// Every golden vector, opcodes 1-4 in file order, byte for byte. The
	// counter is still fresh: TransformText does not count operations.
	replayGolden(t, "compiled Rust engine", tr)

	// The last vector panicked inside Rust. Gusset contained it, and the
	// handle that ran it is poisoned.
	if _, err := client.TransformText(ctx, upper); !errors.Is(err, uniffi.ErrHandlePoisoned) {
		t.Fatalf("after the panic vector: got %v, want ErrHandlePoisoned", err)
	}

	// A fresh handle reaches the same registered engine: its process-wide
	// counter carries on from the five golden operations.
	h2, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithOpcode(compiledEngineOpcode))
	if err != nil {
		t.Fatalf("Open after the panic: %v", err)
	}
	defer h2.Close()
	calc, err := uniffi.NewClient(uniffi.NewGussetTransport(h2)).Calculate(ctx,
		uniffi.CalculateRequest{Op: uniffi.OpAdd, A: 2, B: 3})
	if err != nil || calc.Result != 5 || calc.OperationsTotal != 6 {
		t.Fatalf("Calculate on a fresh handle: got %+v, %v; want 5 with OperationsTotal 6", calc, err)
	}
}
