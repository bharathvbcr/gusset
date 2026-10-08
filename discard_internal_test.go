package gusset

import (
	"context"
	"errors"
	"testing"
	"time"
)

// discardTestWait polls cond under s.mu until it holds or 10s pass.
func discardTestWait(t *testing.T, s *handleState, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		ok := cond()
		s.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Discard of a stored result empties the bookkeeping that holds it: the
// completed entry and, for a result over 4 KiB, the take buffer id whose Rust
// memory the GC cannot see.
func TestDiscard_StoredResultLeavesNothingBehind(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	buf, err := h.NewBuffer(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	buf.Bytes()[0] = 0 // echo
	ticket, err := h.Submit(ctx, buf)
	if err != nil {
		t.Fatal(err)
	}
	_ = buf.Free()

	discardTestWait(t, s, "the result to be stored", func() bool {
		_, done := s.completed[ticket]
		return done
	})
	s.mu.Lock()
	if s.takeIDs[ticket] == 0 {
		s.mu.Unlock()
		t.Fatal("a 64 KiB result should be held as a take buffer; the test measures nothing")
	}
	s.mu.Unlock()

	if err := h.Discard(ticket); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.completed[ticket]; ok {
		t.Error("Discard left the result in completed")
	}
	if _, ok := s.takeIDs[ticket]; ok {
		t.Error("Discard left the take buffer id behind")
	}
	if _, ok := s.semTickets[ticket]; ok {
		t.Error("Discard left the ticket holding a permit")
	}
	if len(s.sem) != 0 {
		t.Errorf("%d permits still held after the only ticket was discarded", len(s.sem))
	}
}

// Discard on a closing handle answers ErrClosed, even for a result stored in
// completed. drainPipe keeps storing completions while Close joins the
// workers, so claiming one there made a ticket answer ErrClosed and then nil
// once its completion landed (found by the hammer's discard op).
func TestDiscard_ClosingHandleIsErrClosedEvenWithAStoredResult(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	s := h.state
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ticket, err := h.Submit(ctx, []byte{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	discardTestWait(t, s, "the result to be stored", func() bool {
		_, done := s.completed[ticket]
		return done
	})

	// The window inside Close: closed is set, the stored result not yet freed.
	s.closed.Store(true)
	err = h.Discard(ticket)
	s.closed.Store(false)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Discard while closing: got %v, want ErrClosed", err)
	}
	s.mu.Lock()
	_, kept := s.completed[ticket]
	s.mu.Unlock()
	if !kept {
		t.Fatal("Discard while closing claimed the result instead of leaving it to Close")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

// Discard of a running ticket leaves its permit with the work (I4): the
// worker is still busy, and returning the permit would let another job run
// beside it. deliver frees the result and returns the permit when the work
// stops, as it does for a ticket abandoned at its deadline.
func TestDiscard_RunningTicketKeepsItsPermitUntilTheWorkStops(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Mode 9: 300 ms without checking for cancellation.
	ticket, err := h.Submit(ctx, []byte{9, 30})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Discard(ticket); err != nil {
		t.Fatalf("Discard of a running ticket: %v", err)
	}
	s.mu.Lock()
	_, held := s.semTickets[ticket]
	_, abandoned := s.abandoned[ticket]
	s.mu.Unlock()
	if !held || !abandoned {
		t.Fatalf("running ticket after Discard: permit held %v, abandoned %v; want both", held, abandoned)
	}

	short, shortCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = h.Submit(short, []byte{0})
	shortCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second Submit on a 1-worker pool whose worker is still busy: got %v, want DeadlineExceeded (I4)", err)
	}

	discardTestWait(t, s, "the discarded job to finish and return its permit", func() bool {
		_, held := s.semTickets[ticket]
		_, abandoned := s.abandoned[ticket]
		_, stored := s.completed[ticket]
		return !held && !abandoned && !stored
	})
	if _, err := h.Call(ctx, []byte{0}); err != nil {
		t.Fatalf("Call once the discarded job finished: %v", err)
	}
}

// A ticket with a waiter belongs to that waiter: Discard refuses it rather
// than pulling the result out from under a parked Wait.
func TestDiscard_RefusesATicketWithAWaiter(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ticket, err := h.Submit(ctx, []byte{9, 20}) // 200 ms, non-cooperative
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() {
		_, err := h.Wait(ctx, ticket)
		waited <- err
	}()
	discardTestWait(t, s, "the waiter to register", func() bool {
		_, ok := s.pending[ticket]
		return ok
	})

	if err := h.Discard(ticket); !errors.Is(err, ErrTicketBusy) {
		t.Fatalf("Discard of a ticket with a waiter: got %v, want ErrTicketBusy", err)
	}
	if err := <-waited; err != nil {
		t.Fatalf("the waiter lost its result to a refused Discard: %v", err)
	}
}
