package gusset

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// These tests stop a waiter in the window between receiving its result from
// deliver and collecting it (popping its take buffer and returning its permit).
// A goroutine can be descheduled there for any length of time; the tests hold
// it there by playing that waiter themselves: they register the pending
// channel exactly as waitInternal does and then do not collect.

// registerWaiter puts ch in pending for ticket as waitInternal does.
func registerWaiter(t *testing.T, s *handleState, ticket uint64) chan callResult {
	t.Helper()
	ch := make(chan callResult, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, live := s.semTickets[ticket]; !live {
		t.Fatalf("ticket %d is not live", ticket)
	}
	if _, busy := s.pending[ticket]; busy {
		t.Fatalf("ticket %d already has a waiter", ticket)
	}
	s.pending[ticket] = ch
	return ch
}

// fakeTicket makes ticket live on s the way a successful submit does (a
// permit plus a semTickets entry) without running anything in Rust, so a test
// can deliver its completion by hand.
func fakeTicket(t *testing.T, s *handleState) uint64 {
	t.Helper()
	const ticket = 1 << 62 // far above anything NEXT_TICKET issues in a test run
	s.sem <- struct{}{}
	s.mu.Lock()
	s.semTickets[ticket] = struct{}{}
	s.mu.Unlock()
	return ticket
}

// A second Wait that arrives while the first waiter is collecting a delivered
// result must be refused (ErrTicketBusy), not parked.
//
// deliver removed the first waiter's pending entry when it sent, while the
// ticket stayed in semTickets until the first waiter returned its permit. In
// that window a second Wait saw a live ticket with no waiter, registered its
// own channel, and parked on a completion that had already been handed out: to
// its deadline, or forever with a context that has none. The hammer's
// double-wait lane hit it as a 20 s stall.
func TestWaiterHandoff_SecondWaitDuringCollectionIsRefusedNotParked(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state
	ticket := fakeTicket(t, s)

	ch := registerWaiter(t, s, ticket) // the first waiter
	s.deliver(ticket, callResult{data: []byte("done")}, 0)
	if got := <-ch; string(got.data) != "done" {
		t.Fatalf("first waiter received %q", got.data)
	}
	// The first waiter holds its result and has not collected yet.

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err = h.Wait(ctx, ticket)
	if !errors.Is(err, ErrTicketBusy) {
		t.Fatalf("second Wait during collection returned %v after %v; want ErrTicketBusy, not a park",
			err, time.Since(start).Round(time.Millisecond))
	}

	// Let the first waiter finish: its permit comes back exactly once.
	s.mu.Lock()
	delete(s.pending, ticket)
	if _, ok := s.semTickets[ticket]; ok {
		delete(s.semTickets, ticket)
		<-s.sem
	}
	s.mu.Unlock()
	if n := len(s.sem); n != 0 {
		t.Fatalf("%d permits held after the only ticket was collected", n)
	}
}

// slowLargeEcho submits a job that sleeps 100 ms and then echoes a 64 KiB
// Buffer, so its result is a Rust take buffer (> the 4 KiB inline copy
// limit) and the test has time to register as its waiter.
func slowLargeEcho(t *testing.T, h *Handle) (uint64, []byte) {
	t.Helper()
	in, err := h.NewBuffer(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	b := in.Bytes()
	for i := range b {
		b[i] = byte(i*7 + 3)
	}
	b[0], b[1], b[2] = 9, 10, 0 // mode 9: sleep 100 ms, then echo input[2:]
	want := append([]byte(nil), b[2:]...)
	ticket, err := h.Submit(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Free() })
	return ticket, want
}

// A result handed to a waiter keeps its take buffer id until that waiter
// collects it, whatever tears the handle down in between.
//
// close and drainPipe's exit both reset takeIDs wholesale. A waiter that had
// already received its result (a view of Rust memory) then popped id 0, and
// wait() returned that view as if it were Go memory: after Close, bytes read
// from freed Rust pages; after a drain exit without Close, drainPipe had
// freed the very buffer through bufFree. The hammer saw it as results of the
// right length with garbage from byte 0.
func TestWaiterHandoff_DeliveredResultKeepsItsTakeBufferAcrossTeardown(t *testing.T) {
	for _, tc := range []struct {
		name     string
		teardown func(*Handle)
	}{
		{"close", func(h *Handle) { _ = h.Close() }},
		{"drain-exit", func(h *Handle) {
			// A read error not caused by Close: the reader gives up on
			// the pipe while the handle stays open.
			_ = h.state.pipe.SetReadDeadline(time.Now())
			<-h.state.drainDone
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			s := h.state
			ticket, want := slowLargeEcho(t, h)
			ch := registerWaiter(t, s, ticket)
			res := <-ch
			if res.err != nil {
				t.Fatal(res.err)
			}
			s.mu.Lock()
			id := s.takeIDs[ticket]
			s.mu.Unlock()
			if id == 0 {
				t.Fatalf("a %d-byte result arrived without a take buffer", len(res.data))
			}

			tc.teardown(h)

			// The waiter collects now, as waitInternal does.
			s.mu.Lock()
			got := s.popTakeIDLocked(ticket)
			delete(s.pending, ticket)
			if _, ok := s.semTickets[ticket]; ok {
				delete(s.semTickets, ticket)
				<-s.sem
			}
			s.mu.Unlock()
			if got != id {
				t.Fatalf("waiter collected take id %d, want %d: wait() would return a view of "+
					"freed Rust memory as the result", got, id)
			}
			// Without Close the buffer must still be live and intact.
			if tc.name == "drain-exit" {
				out, err := func() ([]byte, error) {
					if !s.enterCgo() {
						return nil, errors.New("closed")
					}
					defer s.cgoMu.RUnlock()
					return append([]byte(nil), res.data...), nil
				}()
				if err != nil || !bytes.Equal(out, want) {
					t.Fatalf("result bytes changed after the drain exit (err %v)", err)
				}
				_ = s.bufFree(got)
			}
		})
	}
}
