package isolate

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// scripted is a worker that reads real requests and answers them wrongly in
// one way: dup answers the first twice, unknown answers an id nobody issued,
// reorder holds the first of two requests back and answers the second ahead of
// it (which is legal: ids, not order, pair a result with its caller). It stays
// alive afterwards, so only the host can end it.
func scripted(mode string, out *os.File) int {
	br := bufio.NewReader(os.Stdin)
	next := func() (request, bool) {
		for {
			var req request
			if err := readFrame(br, maxRequestFrame, &req); err != nil {
				return req, false
			}
			if req.Op == opSubmit {
				return req, true
			}
		}
	}
	answer := func(id uint64, body []byte) bool {
		return writeFrame(out, &response{ID: id, Out: body}, maxResponseFrame) == nil
	}
	first, ok := next()
	if !ok {
		return 1
	}
	switch mode {
	case "dup":
		if !answer(first.ID, first.In) || !answer(first.ID, first.In) {
			return 1
		}
	case "unknown":
		if !answer(first.ID+1000, first.In) {
			return 1
		}
	case "reorder":
		second, ok := next()
		if !ok || !answer(second.ID, second.In) || !answer(first.ID, first.In) {
			return 1
		}
	}
	// Alive until the host ends it, as a worker with nothing left to do would
	// be, but leaving is prompt: stdin closing is the host's Close, so the
	// worker goes with it instead of holding Close to its whole budget.
	_, _ = io.Copy(io.Discard, br)
	return 0
}

// latched waits for the reader to finish: the worker is dead to the host.
func latched(t *testing.T, p *Proc) {
	t.Helper()
	select {
	case <-p.readerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the host never latched the worker dead")
	}
}

// A frame whose declared length is over the limit ends the worker at once,
// whatever follows, and says why.
func TestProc_OversizeFrameLatchesTheWorkerDead(t *testing.T) {
	p := startWorkerMode(t, "oversize")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	var err error
	within(t, 3*time.Second, "Call", func() { _, err = p.Call(ctx, []byte{0}) })
	if !errors.Is(err, gusset.ErrPoisoned) || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Call = %v; want ErrPoisoned naming the oversize frame", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waiter released after %v, near its 5s deadline", elapsed)
	}
	latched(t, p)
}

// A stream that ends inside a frame is io.ErrUnexpectedEOF, not a clean end,
// and the waiter hears it.
func TestProc_EOFMidFrameLatchesTheWorkerDead(t *testing.T) {
	p := startWorkerMode(t, "truncated")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	var err error
	within(t, 3*time.Second, "Call", func() { _, err = p.Call(ctx, []byte{0}) })
	if !errors.Is(err, gusset.ErrPoisoned) || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("Call = %v; want ErrPoisoned naming the unexpected EOF", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waiter released after %v, near its 5s deadline", elapsed)
	}
	latched(t, p)
}

// A live worker stalled inside a frame leaves the reader blocked, which no
// read error can end: the caller's own deadline must, and so must Close.
func TestProc_StalledMidFrameIsBoundedByTheDeadlineAndClose(t *testing.T) {
	p := startWorkerMode(t, "stall")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	var err error
	within(t, 3*time.Second, "Call", func() { _, err = p.Call(ctx, []byte{0}) })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call = %v; want its own deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Call took %v against a 300ms deadline", elapsed)
	}
	within(t, 3*time.Second, "close", func() { _ = p.close(100 * time.Millisecond) })
	latched(t, p)
}

// A second answer for one id, or an answer for an id nobody issued, is a
// protocol violation: the worker is dead to the host, and a caller never
// receives someone else's bytes.
func TestProc_DuplicateAndUnknownIDsLatchTheWorkerDead(t *testing.T) {
	for _, mode := range []string{"dup", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			p := startWorkerMode(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			in := []byte{0, 4, 2}
			var out []byte
			var err error
			within(t, 3*time.Second, "Call", func() { out, err = p.Call(ctx, in) })
			if mode == "dup" {
				// The first answer is genuine; the worker's death can beat the
				// waiter to it, so either that echo or the poison is right.
				if err == nil && !bytes.Equal(out, in) {
					t.Fatalf("Call = %v; want the echo of %v", out, in)
				}
				if err != nil && !errors.Is(err, gusset.ErrPoisoned) {
					t.Fatalf("Call = %v; want the echo or ErrPoisoned", err)
				}
			} else if !errors.Is(err, gusset.ErrPoisoned) || out != nil {
				t.Fatalf("Call = %v, %v; want ErrPoisoned and no bytes", out, err)
			}
			latched(t, p)
			if _, err := p.Submit(ctx, []byte{0}); !errors.Is(err, gusset.ErrPoisoned) || !strings.Contains(err.Error(), "unknown id") {
				t.Fatalf("Submit after the violation = %v; want ErrPoisoned naming the unknown id", err)
			}
		})
	}
}

// Results arriving in the opposite order to submission still reach the caller
// that asked: ids pair them.
func TestProc_OutOfOrderResultsReachTheirOwnCallers(t *testing.T) {
	p := startWorkerMode(t, "reorder")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := p.Submit(ctx, []byte{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Submit(ctx, []byte{0, 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id   uint64
		want []byte
	}{{a, []byte{0, 1}}, {b, []byte{0, 2}}} {
		out, err := p.Wait(ctx, c.id)
		if err != nil || !bytes.Equal(out, c.want) {
			t.Fatalf("Wait(%d) = %v, %v; want %v", c.id, out, err, c.want)
		}
	}
}
