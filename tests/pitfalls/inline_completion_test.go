package pitfalls_test

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/bharathvbcr/gusset"
)

// Results at, below and above the inline-record limit (48 bytes) come back
// identical whichever path carried them, serially and under concurrency,
// through Call and through Submit+Wait.
func TestInlineCompletionBoundaryRoundTrips(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()

	// Mode 0 echoes its whole input, mode byte included.
	echo := func(n int) []byte {
		in := make([]byte, n)
		for i := 1; i < n; i++ {
			in[i] = byte(i * 7)
		}
		return in
	}
	sizes := []int{1, 2, 8, 9, 40, 47, 48, 49, 56, 64, 4096}

	for _, n := range sizes {
		in := echo(n)
		out, err := h.Call(ctx, in)
		if err != nil || !bytes.Equal(out, in) {
			t.Fatalf("Call %d bytes: got %d bytes, err %v", n, len(out), err)
		}
		ticket, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		out, err = h.Wait(ctx, ticket)
		if err != nil || !bytes.Equal(out, in) {
			t.Fatalf("Submit/Wait %d bytes: got %d bytes, err %v", n, len(out), err)
		}
	}

	// A result slice is the caller's: it must not alias the reader's buffer,
	// which the next completion overwrites.
	first, err := h.Call(ctx, echo(48))
	if err != nil {
		t.Fatal(err)
	}
	keep := append([]byte(nil), first...)
	for i := 0; i < 64; i++ {
		if _, err := h.Call(ctx, []byte{0, 0xFF, 0xFF, 0xFF}); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(first, keep) {
		t.Fatal("an inline result was overwritten by a later completion")
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				n := sizes[(g+i)%len(sizes)]
				in := echo(n)
				if n > 1 {
					in[n-1] = byte(g)
				}
				out, err := h.Call(ctx, in)
				if err != nil || !bytes.Equal(out, in) {
					errs <- fmt.Errorf("g%d i%d n%d: got %d bytes, err %v", g, i, n, len(out), err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
