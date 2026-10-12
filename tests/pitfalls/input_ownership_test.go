package pitfalls_test

// R6: Go pointers passed to Rust are valid only for the duration of the call.
//
// The enforcer AGENTS.md names is "the cgocheck2 job; the pinner test". The
// cgocheck2 job catches a Go pointer stored into non-Go memory; it cannot
// catch Rust keeping the address of the caller's slice in its own structures,
// because the pointer is a plain integer there. What does catch it is the
// caller giving the slice back while the job is still queued or running: if
// Rust kept the address, the engine reads whatever the Go heap holds there
// afterwards. No runtime.Pinner is used, so nothing keeps the slice in place
// for Rust, and the contract is that submit has copied it before it returns.
//
// The sizes cover each way the runtime stores an input: inline in the work
// unit (up to 56 bytes in all), a heap copy (up to 4 KiB), both edges of each.

import (
	"bytes"
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

func TestR6_SubmitCopiesTheInputBeforeItReturns(t *testing.T) {
	for _, n := range []int{1, 54, 55, 1024, 4094} {
		h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}

		// Mode 9 sleeps without looking at the input again, then dispatches on
		// the rest of it: the engine reads its bytes 150 ms after Submit
		// returned. The remainder is mode 0, which echoes it.
		want := make([]byte, n)
		want[0] = 0
		for i := 1; i < n; i++ {
			want[i] = byte(i*7 + 1)
		}
		in := append([]byte{9, 15}, want...)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ticket, err := h.Submit(ctx, in)
		if err != nil {
			cancel()
			h.Close()
			t.Fatalf("n=%d: Submit: %v", n, err)
		}

		// Give the slice back, and make its memory somebody else's: overwrite
		// it, drop it, collect, and fill the freed space with other data.
		for i := range in {
			in[i] = 0xEE
		}
		in = nil
		for i := 0; i < 3; i++ {
			runtime.GC()
		}
		junk := make([][]byte, 64)
		for i := range junk {
			junk[i] = bytes.Repeat([]byte{0xDD}, n+2)
		}
		runtime.KeepAlive(junk)

		out, err := h.Wait(ctx, ticket)
		cancel()
		h.Close()
		if err != nil {
			t.Fatalf("n=%d: Wait: %v", n, err)
		}
		if !bytes.Equal(out, want) {
			t.Fatalf("n=%d: the engine saw %d bytes that differ from what was submitted: "+
				"Rust kept the caller's pointer instead of copying the input (R6)", n, len(out))
		}
	}
}
