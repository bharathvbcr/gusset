package gusset

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestWaitBuffer_CloseCannotFreeTheResultBeingFilled pins the window between
// WaitBuffer deciding how to carry an inline result and copying the result
// into its Buffer.
//
// The 2026-09-30 write-after-free (CHANGELOG 0.0.3, I1): waitBuffer allocated a
// Rust buffer for an inline result and copied into it with no lock held. Close
// takes cgoMu exclusively and frees every Rust buffer of the handle, so a Close
// landing in that gap finished first and the copy wrote the result into freed
// allocator memory: a write of up to inlineResultBytes into the C heap, on a
// size class the next posix_memalign of the same size walks (the hammer SIGSEGV
// was a 973-byte request in the 992-byte class finding a corrupted tcache next
// pointer).
//
// The fix first held cgoMu from the allocation to the end of the copy, and this
// test asserted that Close could not finish while the fill was pending. An
// inline result no longer touches Rust at all: drainPipe has already copied it
// into Go memory, and WaitBuffer carries it in a Go-heap Buffer with id 0. There
// is no Rust buffer for Close to free and no lock to hold, so "Close cannot
// finish first" stopped being true and stopped being needed. The invariant under
// it is the one asserted here, with the race still forced by the hook: a Close
// that starts and finishes before the copy lands does not touch the result, the
// Buffer holds Go memory (nothing of the handle's was written), the bytes are
// intact, and the Buffer can still be freed.
func TestWaitBuffer_CloseCannotFreeTheResultBeingFilled(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	in := make([]byte, 973) // inline (<= inlineResultBytes): the size class of the original SIGSEGV
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
	closeFinishedBeforeFill := false
	testHookBeforeResultFill = func(s *handleState) {
		hooked = true
		go func() { _ = h.Close() }()
		select {
		case <-s.closeDone:
			closeFinishedBeforeFill = true
		case <-time.After(5 * time.Second):
		}
	}
	defer func() { testHookBeforeResultFill = nil }()

	buf, werr := h.WaitBuffer(ctx, tk)
	testHookBeforeResultFill = nil
	if !hooked {
		t.Fatalf("the fill hook never ran (WaitBuffer: %v); the result did not take the inline path", werr)
	}
	if !closeFinishedBeforeFill {
		t.Fatal("Close did not finish while the fill was pending: the inline fill holds a lock it has no need of, or Close is blocked behind WaitBuffer")
	}
	if werr != nil {
		t.Fatalf("WaitBuffer: %v", werr)
	}
	if buf.ID() != 0 {
		t.Fatalf("an inline result has Rust buffer id %d: Close frees those, and the fill would race it", buf.ID())
	}
	// Bytes is nil once the handle is closed (its documented contract), so the
	// test reads the Go slice the fill wrote, which is what a write into freed
	// memory would have corrupted or never landed in.
	if !bytes.Equal(buf.data, in) {
		t.Fatalf("result corrupted by the Close that finished during the fill: len %d, want %d", len(buf.data), len(in))
	}
	if b := buf.Bytes(); b != nil {
		t.Fatalf("Bytes after Close = %d bytes, want nil", len(b))
	}
	if err := buf.Free(); err != nil {
		t.Fatalf("Free of a result carried across Close: %v", err)
	}
}
