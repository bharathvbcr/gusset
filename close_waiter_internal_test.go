package gusset

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

// waiterParked reports whether a Wait has registered as the waiter for ticket.
func waiterParked(s *handleState, ticket uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.pending[ticket]
	return ok
}

// TestClose_ReleasesALiveContextWaiterWithErrClosed closes a handle under a Wait
// whose context is live, on a cooperative job that would otherwise run for
// seconds, and requires every released waiter to match ErrClosed (A3-2).
//
// This is a characterization test of the existing behaviour, not a fix: the
// audit's claim that the engine's "cancelled: Explicit" reaches the waiter as a
// bare context.Canceled does not reproduce. close() sets closed before it
// cancels, enterCgo refuses on closed, and drainPipe hands errDrainClosed, which
// wraps ErrClosed, to every completion it then reads. The test pins that promise.
//
// The ordering is a condition, not a sleep: Close starts only once the waiter is
// registered in pending, so the waiter is always in Wait when Close begins. Where
// the result then lands (the reader delivering the cancel, or the reader stopping
// first) varies, and every branch must answer ErrClosed.
func TestClose_ReleasesALiveContextWaiterWithErrClosed(t *testing.T) {
	const reps = 200
	var viaCancel int
	for i := 0; i < reps; i++ {
		// Four shapes, 50 reps each: the completion ring or the pipe alone, and
		// Wait or WaitBuffer, which have separate result paths.
		opts := []Option{WithPoolSize(1), WithDiagnosticEngine()}
		if i%2 == 1 {
			opts = append(opts, withPipeOnly())
		}
		viaBuffer := i%4 >= 2
		h, err := Open(opts...)
		if err != nil {
			t.Fatalf("rep %d: Open: %v", i, err)
		}
		live := context.Background()
		// Mode 5 with a count of 255 is a cooperative sleep loop of up to 2.55 s
		// that checks cancellation between steps.
		ticket, err := h.Submit(live, []byte{5, 255})
		if err != nil {
			_ = h.Close()
			t.Fatalf("rep %d: Submit: %v", i, err)
		}
		got := make(chan error, 1)
		go func() {
			if viaBuffer {
				_, err := h.WaitBuffer(live, ticket)
				got <- err
				return
			}
			_, err := h.Wait(live, ticket)
			got <- err
		}()
		for !waiterParked(h.state, ticket) {
			runtime.Gosched()
		}
		if err := h.Close(); err != nil {
			t.Fatalf("rep %d: Close: %v", i, err)
		}
		err = <-got
		if err == nil {
			t.Fatalf("rep %d: Wait returned a result from a job Close cancelled", i)
		}
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("rep %d: Wait released by Close with a live context = %v, want errors.Is(err, ErrClosed)", i, err)
		}
		if errors.Is(err, context.Canceled) {
			viaCancel++
		}
	}
	t.Logf("%d of %d waiters were released with an error that also matches context.Canceled", viaCancel, reps)
}
