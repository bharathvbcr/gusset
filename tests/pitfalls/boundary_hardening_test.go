package pitfalls_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestHardening_OversizedOpcodeDoesNotCollapseToDiagnostic proves that a context
// opcode wider than uint32 is refused. Narrowing it with uint32(v) turns
// 1<<32 into 0, and opcode 0 is the diagnostic engine: payload byte 1 is a
// plain panic, so the truncation poisons the handle.
func TestHardening_OversizedOpcodeDoesNotCollapseToDiagnostic(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("uint is 32 bits wide; this narrowing only exists on 64-bit targets")
	}

	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	// uint(1<<32) does not fit in the header's u32. A truncating cast stores 0.
	huge := uint(uint64(1) << 32)
	ctx := context.WithValue(context.Background(), gusset.OpcodeContextKey, huge)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	_, err = h.Call(ctx, []byte{1})
	if errors.Is(err, gusset.ErrPanic) || errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("oversized opcode collapsed onto the diagnostic engine and panicked: %v", err)
	}
	if err == nil {
		t.Fatal("expected the opcode to be refused")
	}
	if !strings.Contains(err.Error(), "opcode") {
		t.Fatalf("expected an opcode refusal, got: %v", err)
	}

	echoCtx, echoCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer echoCancel()
	out, err := h.Call(echoCtx, []byte{0, 7})
	if err != nil {
		t.Fatalf("handle must stay usable after a refused opcode, got: %v", err)
	}
	if len(out) != 2 || out[1] != 7 {
		t.Fatalf("echo corrupted: %v", out)
	}
}

// TestPitfall_BytesAndFreeDoNotRace is the generational-reference check on the
// Go side of a buffer: Free invalidates the view, and Bytes must not observe
// the slice header concurrently with that invalidation. The race detector is
// the assertion. A passing run under -race is the result; a data race is a failure.
func TestPitfall_BytesAndFreeDoNotRace(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	const readers = 8
	const spins = 4000
	for round := 0; round < 40; round++ {
		buf, err := h.NewBuffer(256)
		if err != nil {
			t.Fatalf("round %d: NewBuffer: %v", round, err)
		}
		// Touch the view once so Free and Bytes contend on a live slice.
		if b := buf.Bytes(); len(b) != 256 {
			t.Fatalf("round %d: Bytes before the race: len %d", round, len(b))
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < readers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for j := 0; j < spins; j++ {
					// Length only. Indexing the slice would use Rust memory
					// Free may already have released, which is a crash rather
					// than the data race this test exists to catch.
					if len(buf.Bytes()) == 0 {
						return
					}
					_ = j
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = buf.Free()
		}()
		close(start)
		wg.Wait()

		if buf.Bytes() != nil {
			t.Fatalf("round %d: Bytes after Free returned a view", round)
		}
		if err := buf.Free(); err != nil {
			t.Fatalf("round %d: second Free: %v", round, err)
		}
	}
}
