package gusset

import (
	"bytes"
	"context"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// rawRingHandle opens a one-worker handle with a completion ring, outside
// the Handle machinery, so a test can hold completions unread.
func rawRingHandle(t *testing.T) (unsafe.Pointer, ffi.Ring, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	wfd, err := syscall.Dup(int(w.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	_ = syscall.SetNonblock(wfd, true)
	h, err := ffi.HandleOpen(1, wfd)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := ffi.HandleRing(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ffi.HandleRing(h); err == nil {
		t.Fatal("a second ring was attached")
	}
	t.Cleanup(func() {
		_ = ffi.HandleClose(h)
		ffi.RingRelease(ring.Owner)
		_ = r.Close()
	})
	return h, ring, r
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Microsecond)
	}
}

var inlineEcho = ffi.CallHeader{Flags: ffi.FlagDiagnosticEngine | ffi.FlagInlineCompletion}

// A ring of two slots holding two unread completions sends the third
// through the pipe; the reader returns all three, the spilled one included.
func TestRingReaderTakesOverflowFromThePipe(t *testing.T) {
	h, ring, r := rawRingHandle(t)
	if ring.Capacity != 2 {
		t.Fatalf("capacity %d, want 2 (pool 1 rounded up)", ring.Capacity)
	}
	overflow := (*uint64)(unsafe.Add(ring.Shared, ffi.RingOffOverflow))
	slotSeq := func(i uintptr) uint64 {
		return atomic.LoadUint64((*uint64)(unsafe.Add(ring.Slots, i*ffi.RingSlotBytes)))
	}
	var want []uint64
	for i := 0; i < 3; i++ {
		tk, err := ffi.Submit(h, inlineEcho, []byte{0, byte(i)}, 0)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, tk)
		switch i {
		case 0, 1:
			eventually(t, "ring publish", func() bool { return slotSeq(uintptr(i)) == uint64(i)+1 })
		case 2:
			eventually(t, "overflow", func() bool { return atomic.LoadUint64(overflow) == 1 })
		}
	}
	tr := newTicketReader(r)
	tr.attachRing(ring)
	for i, tk := range want {
		got, data, inline, err := tr.next()
		if err != nil {
			t.Fatal(err)
		}
		if got != tk || !inline || !bytes.Equal(data, []byte{0, byte(i)}) {
			t.Fatalf("completion %d: got (%d, %v, %v), want (%d, [0 %d], true)", i, got, data, inline, tk, i)
		}
	}
	if tr.overflowPending() {
		t.Fatal("the spilled record was not accounted for")
	}
}

// A reader with nothing to poll for parks on the pipe behind the waiting
// flag; the next publish writes one wake token, and the reader delivers the
// completion and pays the token off.
func TestRingReaderParksAndIsWokenByAToken(t *testing.T) {
	h, ring, r := rawRingHandle(t)
	tr := newTicketReader(r)
	tr.attachRing(ring)
	for round := 0; round < 3; round++ {
		type res struct {
			t   uint64
			err error
		}
		done := make(chan res, 1)
		tr.lastTicket = time.Time{} // idle: no spin, park at once
		go func() {
			tk, _, _, err := tr.next()
			done <- res{tk, err}
		}()
		waiting := (*uint32)(unsafe.Add(ring.Shared, ffi.RingOffWaiting))
		eventually(t, "reader to announce it is parking", func() bool { return atomic.LoadUint32(waiting) == 1 })
		time.Sleep(time.Millisecond) // let it reach the netpoller
		tk, err := ffi.Submit(h, inlineEcho, []byte{0}, 0)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-done:
			if got.err != nil || got.t != tk {
				t.Fatalf("round %d: got (%d, %v), want %d", round, got.t, got.err, tk)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: parked reader was never woken", round)
		}
	}
	// Every token is read eventually: none may pile up in the pipe.
	tk, err := ffi.Submit(h, inlineEcho, []byte{0}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _, err := tr.next(); err != nil || got != tk {
		t.Fatalf("got (%d, %v), want %d", got, err, tk)
	}
	eventually(t, "tokens to be paid off", func() bool {
		if tr.tokensOwed == 0 && tr.end == tr.start {
			return true
		}
		_ = tr.fillMode(readNow)
		for {
			tk, _, in, size, err := tr.parseBuffered()
			if err != nil || size == 0 {
				break
			}
			tr.start += size
			if tk != 0 || in {
				t.Fatalf("unexpected completion %d", tk)
			}
			tr.tokensOwed--
		}
		return tr.tokensOwed == 0 && tr.end == tr.start
	})
}

// The pipe-only transport (what a host without a ring sees) round-trips the
// same results as the ring, serially and under concurrency.
func TestPipeOnlyAndRingTransportsAgree(t *testing.T) {
	for _, opts := range [][]Option{nil, {withPipeOnly()}} {
		h, err := Open(append([]Option{WithPoolSize(4), WithDiagnosticEngine()}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		if (h.state.ring.Owner != nil) != (len(opts) == 0) {
			t.Fatal("withPipeOnly did not control the ring")
		}
		ctx := context.Background()
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < 300; i++ {
					n := 1 + (g*31+i)%80 // both sides of the 48-byte inline limit
					in := bytes.Repeat([]byte{byte(g + 1)}, n)
					in[0] = 0
					out, err := h.Call(ctx, in)
					if err != nil || !bytes.Equal(out, in) {
						errs <- err
						return
					}
				}
			}(g)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round trip failed: %v", err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
