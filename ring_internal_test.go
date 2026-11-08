package gusset

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
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

// A lone job's poll is twice the recent gap between completions, clamped to
// [ticketReaderSpin, ticketReaderSpinBusy]; idle gaps do not count, and with
// more than one job in flight the short window applies.
func TestSpinBudgetFollowsRecentCompletionGaps(t *testing.T) {
	lone := true
	tr := &ticketReader{inFlight: func() bool { return lone }}
	for _, c := range []struct {
		ewma, want time.Duration
	}{
		{0, ticketReaderSpin},
		{10 * time.Microsecond, ticketReaderSpin},
		{60 * time.Microsecond, 120 * time.Microsecond},
		{time.Millisecond, ticketReaderSpinBusy},
	} {
		tr.gapEWMA = c.ewma
		if got := tr.spinBudget(); got != c.want {
			t.Fatalf("gapEWMA %v: budget %v, want %v", c.ewma, got, c.want)
		}
	}
	lone = false
	if got := tr.spinBudget(); got != ticketReaderSpin {
		t.Fatalf("parallel budget %v, want %v", got, ticketReaderSpin)
	}

	tr = &ticketReader{}
	tr.completed() // first stamp: no gap yet
	if tr.gapEWMA != 0 {
		t.Fatal("the first completion has no gap to record")
	}
	// completed() reads the clock itself, so the gap it sees is the 80 us
	// planted here plus however long the host leaves this goroutine
	// descheduled between the two statements: hundreds of microseconds on a
	// loaded machine, which moved the average past its 20 us bound. A stall
	// can only add to the gap, so no attempt may land below the planted
	// figure's share (10 us), and one undisturbed attempt must land within
	// 20 us. A wrong weight misses the window on every attempt.
	var moved time.Duration
	for attempt := 0; attempt < 1000; attempt++ {
		tr = &ticketReader{}
		tr.completed()
		tr.lastTicket = time.Now().Add(-80 * time.Microsecond)
		tr.completed()
		moved = tr.gapEWMA
		if moved < 10*time.Microsecond {
			t.Fatalf("one 80 us gap moved the average to %v, want ~10 us", moved)
		}
		if moved <= 20*time.Microsecond {
			break
		}
	}
	if moved > 20*time.Microsecond {
		t.Fatalf("one 80 us gap moved the average to %v in every one of 1000 attempts, want ~10 us", moved)
	}
	before := tr.gapEWMA
	tr.lastTicket = time.Now().Add(-time.Second)
	tr.completed()
	if tr.gapEWMA != before {
		t.Fatal("an idle gap was counted as job length")
	}
}

// Close while the drain reader is parked behind the ring's waiting flag,
// idle and with a job still running: Close returns promptly, a waiter gets an
// answer instead of hanging, and the reader goroutine is gone.
func TestCloseWhileReaderIsParkedOnTheRing(t *testing.T) {
	spin := []byte{11, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(spin[1:], 2_000_000) // a few ms
	base := runtime.NumGoroutine()
	for round := 0; round < 40; round++ {
		h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
		if err != nil {
			t.Fatal(err)
		}
		if h.state.ring.Owner == nil {
			t.Fatal("no ring attached")
		}
		ctx := context.Background()
		if _, err := h.Call(ctx, []byte{0}); err != nil {
			t.Fatal(err)
		}
		waiting := (*uint32)(unsafe.Add(h.state.ring.Shared, ffi.RingOffWaiting))
		eventually(t, "reader to park", func() bool { return atomic.LoadUint32(waiting) == 1 })

		var waitErr chan error
		if round%2 == 1 {
			// A job in flight while the reader is parked: its completion
			// races Close.
			tk, err := h.Submit(ctx, spin)
			if err != nil {
				t.Fatal(err)
			}
			waitErr = make(chan error, 1)
			go func() {
				_, err := h.Wait(ctx, tk)
				waitErr <- err
			}()
		}
		done := make(chan error, 1)
		go func() { done <- h.Close() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: Close hung with the reader parked", round)
		}
		select {
		case <-h.state.drainDone:
		default:
			t.Fatalf("round %d: drain reader still running after Close", round)
		}
		if waitErr != nil {
			select {
			case <-waitErr: // a result or "closed"; either is an answer
			case <-time.After(10 * time.Second):
				t.Fatalf("round %d: Wait hung across Close", round)
			}
		}
	}
	eventually(t, "goroutines to wind down", func() bool { return runtime.NumGoroutine() <= base+2 })
}

// killDrain makes drainPipe give up on an open handle, as a read error or a
// corrupt record not caused by Close would.
func killDrain(t *testing.T, s *handleState) {
	t.Helper()
	_ = s.pipe.SetReadDeadline(time.Now())
	select {
	case <-s.drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("drainPipe did not exit on a read error")
	}
	if !s.drainExited.Load() {
		t.Fatal("drainPipe exited without setting drainExited")
	}
}

// Once drainPipe has stopped reading, no completion can ever be delivered, so a
// submitter already parked on the pool semaphore must be refused rather than
// left parked, and must never be admitted to run a job nobody can collect.
//
// The drainExited refusal covered only callers arriving after the exit. One
// parked on a full pool stayed parked until Close: forever, with a context
// that has no deadline. And one woken by a permit coming back — a waiter told
// "handle closed" returns its permit — passed the closed and poisoned checks
// and was handed a ticket for work whose completion nobody would read (I4).
func TestDrainExit_ParkedSubmitterIsRefusedNotParkedOrAdmitted(t *testing.T) {
	for _, withWaiter := range []bool{false, true} {
		name := "permit-held"
		if withWaiter {
			name = "permit-returned-by-waiter"
		}
		t.Run(name, func(t *testing.T) {
			h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			s := h.state
			ctx := context.Background()

			// The pool's only permit, held by a 300 ms job.
			busy, err := h.Submit(ctx, []byte{9, 30})
			if err != nil {
				t.Fatal(err)
			}
			waited := make(chan error, 1)
			if withWaiter {
				go func() { _, err := h.Wait(ctx, busy); waited <- err }()
				eventually(t, "the waiter to register", func() bool {
					s.mu.Lock()
					defer s.mu.Unlock()
					_, ok := s.pending[busy]
					return ok
				})
			}

			type result struct {
				ticket uint64
				err    error
			}
			parked := make(chan result, 1)
			go func() {
				tk, err := h.Submit(ctx, []byte{0, 1})
				parked <- result{tk, err}
			}()
			time.Sleep(20 * time.Millisecond) // let it park on the full pool
			select {
			case r := <-parked:
				t.Fatalf("second submit on a full pool of one did not park: %+v", r)
			default:
			}

			killDrain(t, s)
			if withWaiter {
				if err := <-waited; err == nil {
					t.Fatal("waiter got a result after the reader gave up")
				}
			}

			select {
			case r := <-parked:
				if r.err == nil {
					t.Fatalf("parked submitter was admitted with ticket %d after the reader "+
						"gave up: its completion can never be delivered", r.ticket)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("parked submitter still parked 2 s after the reader gave up")
			}

			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			if n := len(s.sem); n != 0 {
				t.Fatalf("%d permits held after Close (I4)", n)
			}
		})
	}
}

// When drainPipe stops on its own — a corrupt record, an errno nobody asked
// for — the reason has to survive. It used to be dropped: every waiter got
// "gusset: handle closed", every later call ErrClosed, and nothing was logged,
// so a handle that had stopped working could not say why. The cause is now
// logged, kept on the handle, and carried by the waiter's error and every
// later refusal, which still match ErrClosed.
func TestDrainExit_CauseIsRetainedAndStillErrClosed(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state
	ctx := context.Background()

	busy, err := h.Submit(ctx, []byte{9, 30}) // 300 ms job
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { _, err := h.Wait(ctx, busy); waited <- err }()
	eventually(t, "the waiter to register", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, ok := s.pending[busy]
		return ok
	})

	// killDrain ends the reader with a read-deadline error on an open handle.
	killDrain(t, s)

	isCause := func(what string, err error, prefix string) {
		t.Helper()
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("%s: %v does not match ErrClosed", what, err)
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("%s: %v lost the reader's exit cause", what, err)
		}
		if !strings.HasPrefix(err.Error(), prefix) {
			t.Fatalf("%s: %q dropped the historical text %q", what, err, prefix)
		}
	}

	cause := s.drainErr.Load()
	if cause == nil {
		t.Fatal("drainPipe exited on an open handle without recording why")
	}
	if !errors.Is(cause.cause, os.ErrDeadlineExceeded) {
		t.Fatalf("recorded cause %v is not the read error", cause.cause)
	}
	isCause("waiter", <-waited, "gusset: handle closed")
	_, err = h.Submit(ctx, []byte{0, 1})
	isCause("later Submit", err, "gusset: handle is closed")
	_, err = h.Call(ctx, []byte{0, 1})
	isCause("later Call", err, "gusset: handle is closed")
	if !strings.Contains(logs.String(), "completion reader stopped") {
		t.Fatalf("the reader's exit was not logged; log:\n%s", logs.String())
	}

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// A reader stopped by Close is not a failure: no cause, no warning.
	logs.Reset()
	h2, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	if err := h2.Close(); err != nil {
		t.Fatal(err)
	}
	if c := h2.state.drainErr.Load(); c != nil {
		t.Fatalf("Close recorded a reader failure: %v", c)
	}
	if strings.Contains(logs.String(), "completion reader stopped") {
		t.Fatalf("Close logged a reader failure:\n%s", logs.String())
	}
}

// BenchmarkCompletionTransport compares the shared-memory completion ring
// with the pipe on identical work, in one binary: the only difference between
// the arms is withPipeOnly. Run:
//
//	go test -run '^$' -bench CompletionTransport -count=6 .
func BenchmarkCompletionTransport(b *testing.B) {
	spin := func(iters uint32) []byte {
		in := []byte{11, 0, 0, 0, 0}
		binary.LittleEndian.PutUint32(in[1:], iters)
		return in
	}
	works := []struct {
		name string
		in   []byte
	}{
		{"noop", []byte{0}},
		// Mode 11's loop: about 10 us on the recording host.
		{"spin10us", spin(10_000)},
		{"spin100us", spin(100_000)},
	}
	for _, mode := range []struct {
		name string
		opts []Option
	}{
		{"ring", nil},
		{"pipe", []Option{withPipeOnly()}},
	} {
		h, err := Open(append([]Option{WithPoolSize(4), WithDiagnosticEngine()}, mode.opts...)...)
		if err != nil {
			b.Fatal(err)
		}
		ctx := context.Background()
		// 64 KiB in a Buffer, echoed: a stored result collected with take.
		b.Run("buf64k/serial/"+mode.name, func(b *testing.B) {
			buf, err := h.NewBuffer(64 << 10)
			if err != nil {
				b.Fatal(err)
			}
			defer buf.Free()
			data := buf.Bytes()
			for i := range data {
				data[i] = byte(i)
			}
			data[0] = 0
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				t, err := h.Submit(ctx, buf)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := h.Wait(ctx, t); err != nil {
					b.Fatal(err)
				}
			}
		})
		for _, w := range works {
			b.Run(fmt.Sprintf("%s/serial/%s", w.name, mode.name), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, err := h.Call(ctx, w.in); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(fmt.Sprintf("%s/parallel/%s", w.name, mode.name), func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						if _, err := h.Call(ctx, w.in); err != nil {
							b.Error(err)
							return
						}
					}
				})
			})
		}
		_ = h.Close()
	}
}
