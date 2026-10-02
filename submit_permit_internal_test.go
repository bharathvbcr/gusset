package gusset

import (
	"context"
	"testing"
	"time"
)

// TestPitfall_SubmitWithoutWaitDoesNotWedgeThePool is the regression for a
// Submit whose result the reader has already stored and that nobody has
// Waited on. The permit used to stay in semTickets until Wait or Close, so
// pool_size of those Submits filled the semaphore and the next Submit blocked
// until its context died, with every worker idle.
func TestPitfall_SubmitWithoutWaitDoesNotWedgeThePool(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	first, err := h.Submit(ctx, []byte{0})
	if err != nil {
		t.Fatalf("first Submit: %v", err)
	}

	started := time.Now()
	second, err := h.Submit(ctx, []byte{0, 1})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Submit after an un-waited Submit failed in %s: %v (permit still held after the reader stored the result?)", elapsed, err)
	}
	if elapsed > time.Second {
		t.Fatalf("second Submit took %s; the idle worker's permit was not returned", elapsed)
	}

	if _, err := h.Wait(ctx, first); err != nil {
		t.Fatalf("Wait of the fire-and-forget ticket: %v", err)
	}
	if _, err := h.Wait(ctx, second); err != nil {
		t.Fatalf("Wait of the second ticket: %v", err)
	}
}
