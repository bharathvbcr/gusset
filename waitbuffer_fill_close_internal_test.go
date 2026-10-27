package gusset

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestWaitBuffer_CloseCannotFreeTheResultBeingFilled pins the window between
// WaitBuffer allocating a Rust buffer for an inline result and copying the
// result into it.
//
// waitBuffer used to take the view with Bytes, which drops cgoMu on return,
// and then copy into it with no lock held. Close takes cgoMu exclusively and
// frees every Rust buffer of the handle, so a Close landing in that gap
// finished first and the copy wrote the result into freed allocator memory: a
// write-after-free into the C heap, on a size class the next posix_memalign of
// the same size walks (the 2026-09-30 hammer SIGSEGV was a 973-byte request in
// the 992-byte class finding a corrupted tcache next pointer). The fill path
// then covered every result up to inlineResultBytes. The worker now promotes
// every result over the 48-byte record limit onto a take buffer, which
// WaitBuffer wraps and never fills, so only a result carried whole in a
// completion record reaches the fill: this test sends 40 bytes, not 973. The
// take-buffer window against Close is TestPitfall_WaitBufferOfLargeResultDoesNotDangleAcrossClose
// (tests/pitfalls). The hook stands in for a preemption at that point: it
// starts Close and waits for it. Close must not be able to finish while the
// fill is pending.
func TestWaitBuffer_CloseCannotFreeTheResultBeingFilled(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	in := make([]byte, 40) // inline record (<= ffi.InlineResultMax): WaitBuffer allocates and fills
	for i := 1; i < len(in); i++ {
		in[i] = byte(i*7 + 3)
	}
	in[0] = 0 // diagnostic echo
	ctx := context.Background()
	tk, err := h.Submit(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	hooked := false
	closeFinishedFirst := false
	testHookBeforeResultFill = func(s *handleState) {
		hooked = true
		go func() { _ = h.Close() }()
		select {
		case <-s.closeDone:
			closeFinishedFirst = true
		case <-time.After(300 * time.Millisecond):
		}
	}
	defer func() { testHookBeforeResultFill = nil }()

	buf, werr := h.WaitBuffer(ctx, tk)
	testHookBeforeResultFill = nil
	if !hooked {
		t.Fatalf("the fill hook never ran (WaitBuffer: %v); the result did not take the inline path", werr)
	}
	if closeFinishedFirst {
		t.Fatal("Close freed the handle's Rust buffers while WaitBuffer had yet to copy the result into one: the copy writes freed memory")
	}
	if werr != nil {
		t.Fatalf("WaitBuffer: %v", werr)
	}
	// The fill completed before Close could free anything. Close is now free
	// to proceed; until it does, the buffer holds the result.
	if b := buf.Bytes(); b != nil && !bytes.Equal(b, in) {
		t.Fatalf("result corrupted before Close: first byte %d", b[0])
	}
	_ = h.Close()
	if b := buf.Bytes(); b != nil {
		t.Fatalf("Bytes after Close = %d bytes, want nil", len(b))
	}
}
