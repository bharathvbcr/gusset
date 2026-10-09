package gusset

import (
	"context"
	"errors"
	"runtime"
	"time"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// Wait waits for completion of an asynchronously submitted job ticket.
func (h *Handle) Wait(ctx context.Context, ticket uint64) ([]byte, error) {
	if h == nil || h.state == nil {
		return nil, ErrNilHandle
	}
	if ctx == nil {
		return nil, ErrNilContext
	}
	res, err := h.state.wait(ctx, ticket)
	runtime.KeepAlive(h)
	return res, err
}

// WaitBuffer waits for completion of an asynchronously submitted job ticket,
// returning a zero-copy *Buffer backed by Rust-owned memory.
//
// If the job completed with inline bytes rather than an allocated buffer, a *Buffer
// is allocated to hold the data.
// The caller is responsible for calling buf.Free() when done, though runtime.AddCleanup
// acts as a safety net if the buffer is garbage collected.
func (h *Handle) WaitBuffer(ctx context.Context, ticket uint64) (*Buffer, error) {
	if h == nil || h.state == nil {
		return nil, ErrNilHandle
	}
	if ctx == nil {
		return nil, ErrNilContext
	}
	buf, err := h.state.waitBuffer(ctx, ticket)
	if buf != nil {
		buf.owner = h
	}
	runtime.KeepAlive(h)
	return buf, err
}

func (s *handleState) wait(ctx context.Context, ticket uint64) ([]byte, error) {
	res, takeID, err := s.waitInternal(ctx, ticket)
	if err != nil {
		return nil, shutdownCause(ctx, err)
	}
	if takeID != 0 {
		// Close takes cgoMu exclusively before HandleClose frees Rust memory.
		// Copying without that lock raced Close: the view was still readable and
		// the destination was filled from freed pages (GOGC=1
		// TestStress_ConcurrentCallAndCloseRace).
		if !s.enterCgo() {
			return nil, ErrClosed
		}
		out := make([]byte, len(res.data))
		copy(out, res.data)
		s.cgoMu.RUnlock()
		_ = s.bufFree(takeID)
		return out, nil
	}
	return res.data, nil
}

func (s *handleState) waitBuffer(ctx context.Context, ticket uint64) (*Buffer, error) {
	res, takeID, err := s.waitInternal(ctx, ticket)
	if err != nil {
		return nil, shutdownCause(ctx, err)
	}

	// Wrap under cgoMu so Close cannot HandleClose the take buffer between
	// waitInternal returning the view and newBufferFromRaw installing it.
	if !s.enterCgo() {
		s.discardTake(takeID)
		return nil, ErrClosed
	}
	if takeID != 0 {
		buf := newBufferFromRaw(s, takeID, res.data)
		s.cgoMu.RUnlock()
		return buf, nil
	}
	s.cgoMu.RUnlock()

	if len(res.data) > 0 {
		buf, err := s.allocBuffer(len(res.data), false)
		if err != nil {
			if s.closed.Load() {
				return nil, ErrClosed
			}
			// This result already exists and has left completed; any error
			// here would lose it for good. Poison refuses new work only (I2),
			// and NewBuffer's Rust allocation is poison-checked, so a sibling's
			// panic used to turn a finished result into ErrPoisoned. Carry it
			// on the Go heap instead, 64-byte aligned like a Rust buffer.
			return newHeapBuffer(s, res.data), nil
		}
		b := buf.Bytes()
		if b == nil {
			_ = buf.Free()
			return nil, ErrClosed
		}
		copy(b, res.data)
		return buf, nil
	}
	if s.closed.Load() {
		return nil, ErrClosed
	}
	// A job returning an empty output (0 bytes) produces a valid empty Buffer
	return newBufferFromRaw(s, 0, nil), nil
}

// claimLocked is the front half of Wait and Discard, the two ways a ticket's
// result is moved out exactly once.
//
// done reports a stored result, moved out of completed with its take buffer id
// and the ticket ended. err reports a ticket that cannot be claimed. With
// neither, the ticket is live, still running, and has no waiter: the caller
// either registers as its waiter or abandons it, still under mu.
func (s *handleState) claimLocked(ticket uint64) (res callResult, takeID uint64, done bool, err error) {
	if res, done := s.completed[ticket]; done {
		delete(s.completed, ticket)
		return res, s.collectLocked(ticket), true, nil
	}

	// An abandoned ticket is spent. Its result is already promised to the bin,
	// so a second waiter could only park for a completion it will never be
	// handed — the same forever-park the abandonment exists to remove.
	if _, gone := s.abandoned[ticket]; gone {
		return callResult{}, 0, false, ErrUnknownTicket
	}

	if s.closed.Load() || s.drainExited.Load() {
		return callResult{}, 0, false, s.closedErr()
	}

	// Refuse a ticket this handle is not holding.
	//
	// Wait used to register any ticket in the pending map and then, on ctx.Done,
	// block unconditionally for a completion that was never coming. A ticket that
	// was never submitted here — a typo, a stale id, one from another handle — parked
	// the caller forever and its context deadline did nothing at all. semTickets is
	// exactly the set of live tickets this handle issued and has not yet handed back.
	if _, live := s.semTickets[ticket]; !live {
		return callResult{}, 0, false, ErrUnknownTicket
	}

	// Refuse a second waiter rather than displacing the first.
	//
	// Assigning s.pending[ticket] unconditionally replaced the first waiter's
	// channel. The completion was then delivered to the second waiter and the first
	// blocked forever, because nothing held a reference to its channel any more. A
	// result can be moved out exactly once, so a ticket can have exactly one waiter.
	//
	// The permit still belongs to the owner. A deferred releaseSem on this
	// return used to consume it, so a third Submit could enter while the job
	// was still running (I4).
	if _, busy := s.pending[ticket]; busy {
		return callResult{}, 0, false, ErrTicketBusy
	}
	return callResult{}, 0, false, nil
}

// Discard gives up a submitted ticket's result without waiting for it.
//
// A result nobody Waits for is otherwise kept until Close (see Submit). For a
// result already stored, Discard frees it now, Rust take buffer included. For
// a job still running, the result is freed when it lands and the pool permit
// comes back when the work stops, exactly as for a ticket abandoned at its
// deadline (I4). Discard does not cancel the job: the work runs to completion,
// and only its result is dropped. To stop it as well, Wait with a context that
// is already cancelled.
//
// A discarded ticket is spent: Wait and Discard on it return ErrUnknownTicket.
// A ticket another goroutine is Waiting on returns ErrTicketBusy and stays
// that waiter's. Once Close has begun it returns ErrClosed.
func (h *Handle) Discard(ticket uint64) error {
	if h == nil || h.state == nil {
		return ErrNilHandle
	}
	err := h.state.discard(ticket)
	runtime.KeepAlive(h)
	return err
}

func (s *handleState) discard(ticket uint64) error {
	// Closed first, unlike Wait. drainPipe keeps storing completions while
	// Close joins the workers, so a claim here could answer ErrClosed for a
	// ticket and then nil for the same ticket once its completion landed.
	// Close frees every result itself; there is nothing for Discard to do.
	if s.closed.Load() {
		return s.closedErr()
	}
	s.mu.Lock()
	_, takeID, done, err := s.claimLocked(ticket)
	if !done && err == nil {
		// Running, with no waiter: deliver frees the result and returns the
		// permit when the completion lands.
		s.abandoned[ticket] = struct{}{}
	}
	s.mu.Unlock()
	s.discardTake(takeID)
	return err
}

func (s *handleState) waitInternal(ctx context.Context, ticket uint64) (callResult, uint64, error) {
	s.mu.Lock()
	if res, takeID, done, err := s.claimLocked(ticket); done || err != nil {
		s.mu.Unlock()
		if err != nil {
			return callResult{}, 0, err
		}
		if res.err != nil {
			s.discardTake(takeID)
			return callResult{}, 0, res.err
		}
		return res, takeID, nil
	}

	ticketCh := s.waitChanLocked()
	s.pending[ticket] = ticketCh
	s.mu.Unlock()

	res, received := s.pollResult(ctx, ticketCh)
	if !received {
		// Counted before the receive, read by deliver after its send: see
		// deliver's readied. Only with at most one call in flight, the one
		// case the reader reads it in (drainPipe's yieldEachPoll): under
		// parallel load every caller bumping one shared counter would bounce
		// its cache line between them. A waiter that parked under load and is
		// delivered to once the load has drained is not counted, and runs
		// when another P steals it, a few microseconds later.
		counted := len(s.sem) <= 1
		if counted {
			s.parkedWaiters.Add(1)
		}
		select {
		case res = <-ticketCh:
			received = true
		case <-ctx.Done():
		}
		if counted {
			s.parkedWaiters.Add(-1)
		}
	}
	if received {
		s.mu.Lock()
		s.recycleWaitChanLocked(ticketCh)
		takeID := s.collectLocked(ticket)
		s.mu.Unlock()
		if res.err != nil {
			s.discardTake(takeID)
			return callResult{}, 0, res.err
		}
		return res, takeID, nil
	}

	// ctx ended first. Ask Rust to stop. This is all cancellation can be: the flag is only
	// read by an engine that calls JobContext::check, and an engine that
	// never does — a tokenizer, a regex scan, a proof verifier — runs to
	// completion regardless.
	// Never blocks: a close in progress cancels every job itself, and
	// parking behind its worker join would hold this caller past the
	// deadline it is returning for.
	if s.enterCgo() {
		_ = ffi.Cancel(s.ptr, ticket)
		s.cgoMu.RUnlock()
	}

	// Detach rather than wait for the completion.
	//
	// This receive used to be unconditional, which made the caller's
	// deadline a statement about the engine rather than about Gusset: an
	// 80 ms deadline on a 1.5 s non-cooperative job returned after 1.5 s.
	// Bounding the caller is the whole contract, so the caller leaves now
	// and drainPipe disposes of the result when it lands.
	//
	// The permit stays behind deliberately. The worker is still executing,
	// and handing the permit back here would let a further submission run
	// alongside it — in-flight work above the pool size, which is exactly
	// the OS-thread bound I4 sells. deliver returns the permit at the
	// moment the work actually stops.
	s.mu.Lock()
	select {
	case res := <-ticketCh:
		// Raced: the completion landed between ctx firing and this lock, so
		// there is nothing to abandon and the permit is free now.
		s.recycleWaitChanLocked(ticketCh)
		takeID := s.collectLocked(ticket)
		s.mu.Unlock()
		s.discardTake(takeID)
		// A panic that arrived in the race window is reported as the panic,
		// not as the deadline: the caller asked what happened to its work
		// and a real answer exists. drainPipe has already latched the
		// poison.
		if res.err != nil && errors.Is(res.err, ErrPanic) {
			return callResult{}, 0, res.err
		}
		return callResult{}, 0, ctx.Err()
	default:
		// Out of pending under mu with nothing sent: no send can reach it.
		delete(s.pending, ticket)
		s.recycleWaitChanLocked(ticketCh)
	}
	s.abandoned[ticket] = struct{}{}
	s.mu.Unlock()
	return callResult{}, 0, ctx.Err()
}

// waiterSpin is how long a lone waiter polls for its result before parking.
const waiterSpin = ticketReaderSpin

// pollResult polls a lone in-flight call's result channel for up to
// waiterSpin, so a result that lands meanwhile is taken without the waiter
// ever parking.
//
// A parked waiter is made runnable by the reader's send, and with a P idle
// and no thread spinning that costs a system call to wake a thread
// (pthread_cond_signal on darwin), several microseconds on the reader's
// critical path; the reader then has to yield its P to run it. A send into
// the buffered channel of a waiter that is still polling does neither. Only
// a lone call polls, so a parallel load parks exactly as before and spends no
// extra core, and only with a P to spare beside this one and the reader's
// (quietAt): with one P the reader cannot run until this goroutine stops, so a
// poll would only delay the result it waits for, and with two the pair held
// both and nothing else ran.
//
// ok is false when the window ran out or ctx ended; the caller then waits
// as before, and its select sees ctx.
func (s *handleState) pollResult(ctx context.Context, ch chan callResult) (res callResult, ok bool) {
	if len(s.sem) != 1 || !s.quietProcs.Load() {
		return callResult{}, false
	}
	done := ctx.Done()
	start := time.Now()
	for i := 0; ; i++ {
		select {
		case res = <-ch:
			return res, true
		default:
		}
		if i&7 == 7 {
			select {
			case <-done:
				return callResult{}, false
			default:
			}
			if time.Since(start) >= waiterSpin {
				return callResult{}, false
			}
		}
	}
}
