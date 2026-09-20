package pitfalls_test

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// Phase 2 Validation:
// Validates adoption of Gusset by a second engine shape (CPU-bound vector arithmetic)
// with zero modifications to the 14-export Rust ABI and 10-entry Go public API.
func TestPhase2_CpuBoundEngine(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Prepare 10,000 element vector for sum-and-square (opcode 10)
	const n = 10_000
	payload := make([]byte, n+1)
	payload[0] = 10 // opcode 10: sum and square
	var expectedAcc uint64
	for i := 1; i <= n; i++ {
		val := byte(i % 127)
		payload[i] = val
		v64 := uint64(val)
		expectedAcc += v64 * v64
	}

	res, err := h.Call(ctx, payload)
	if err != nil {
		t.Fatalf("CPU-bound Call failed: %v", err)
	}

	if len(res) != 8 {
		t.Fatalf("expected 8-byte uint64 result, got %d bytes", len(res))
	}

	actualAcc := binary.LittleEndian.Uint64(res)
	if actualAcc != expectedAcc {
		t.Fatalf("calculation mismatch: expected %d, got %d", expectedAcc, actualAcc)
	}

	t.Logf("Phase 2 CPU-bound engine calculation verified successfully: sum-sq = %d", actualAcc)
}
