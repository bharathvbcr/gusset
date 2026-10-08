package gusset

import (
	"errors"
	"log/slog"
	"runtime"
	"time"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// drainPipe reads completion records from the pipe and routes each result.
func drainPipe(s *handleState) {
	defer close(s.drainDone)
	tr := newTicketReader(s.pipe)
	if s.ring.Owner != nil {
		tr.attachRing(s.ring)
		// Runs before drainDone closes: close waits on drainDone, so by the
		// time it returns nothing reads the ring any more.
		defer ffi.RingRelease(s.ring.Owner)
	}
	// Only a lone in-flight job earns the longer poll: under parallel load a
	// completion is always pending, and polling between them took a core the
	// workers needed (1 ms parallel jobs +18% on 4 vCPUs).
	tr.inFlight = func() bool { return len(s.sem) == 1 }
	// At most one call in flight on several Ps is the case the poll is quiet
	// for (see ticketReader.pollPause): a serial caller's next call comes
	// while the reader still polls after the last. Otherwise the reader
	// yields on every poll.
	tr.yieldEachPoll = func() bool { return !s.multiP.Load() || len(s.sem) > 1 }
	// handOff delivers a completion and, when that readied a goroutine the
	// reader's polls will not yield to, yields to it now.
	handOff := func(ticket uint64, res callResult, takeID uint64) {
		if s.deliver(ticket, res, takeID) && !tr.yieldsEachPoll() {
			runtime.Gosched()
		}
	}
	var procsAt time.Time

	for {
		ticket, inlineData, inline, err := tr.next()
		if err == nil && tr.lastTicket.Sub(procsAt) >= gomaxprocsRefresh {
			procsAt = tr.lastTicket
			s.multiP.Store(runtime.GOMAXPROCS(0) > 1)
		}
		if err != nil {
			// Close sets closed before it stops the reader, so an error with
			// the handle still open is the reader failing, not being stopped.
			waiterErr := errDrainClosed
			var exit *drainExitError
			if !s.closed.Load() {
				exit = &drainExitError{prefix: ErrClosed.Error(), cause: err}
				s.drainErr.Store(exit)
				waiterErr = &drainExitError{prefix: errDrainClosed.Error(), cause: err}
			}
			s.mu.Lock()
			for t, ch := range s.pending {
				if ch != nil { // nil: already delivered, its waiter is collecting
					s.pending[t] = nil
					ch <- callResult{err: waiterErr}
				}
			}
			toFree := s.takeUnclaimedLocked()
			s.drainExited.Store(true)
			// Permits held by abandoned tickets are returned by close, which
			// drains every entry in semTickets. Clearing the set here only stops
			// a late completion from being treated as abandoned after the drain
			// reader has already given up on the pipe.
			s.abandoned = make(map[uint64]struct{})
			s.mu.Unlock()

			if exit != nil {
				slog.Warn("gusset: completion reader stopped while the handle was open; "+
					"its waiters and every later call fail with ErrClosed", "err", exit.cause)
			}
			for _, id := range toFree {
				_ = s.bufFree(id)
			}
			return
		}

		// A small success arrived whole in its record: no gusset_take, no
		// registry buffer, no gusset_buf_free, and no cgoMu. The bytes alias
		// the reader's buffer, so they are copied out before the next read.
		if inline {
			var out []byte
			if len(inlineData) > 0 {
				out = make([]byte, len(inlineData))
				copy(out, inlineData)
			}
			handOff(ticket, callResult{data: out}, 0)
			continue
		}

		// Synchronize with handle close to eliminate UAF on s.ptr, without
		// ever blocking. This goroutine used to wait on cgoMu.RLock behind
		// Close's exclusive lock, so for the whole worker join nobody read the
		// pipe. On a small pipe (macOS falls back to 512 bytes, 64 tickets,
		// under memory pressure) workers then sat in the 10 s write backoff and
		// Close took that long. During a close every result is discarded
		// anyway, so keep reading and hand each waiter "closed".
		if !s.enterCgo() {
			handOff(ticket, callResult{err: errDrainClosed}, 0)
			continue
		}

		// Retrieve result from Rust
		bufID, outBytes, takeErr := ffi.Take(s.ptr, ticket)
		var res callResult
		var takeID uint64
		switch {
		case takeErr != nil:
			if errors.Is(takeErr, ErrPanic) {
				s.poisoned.Store(true)
			}
			res = callResult{err: takeErr}
		case (bufID&ffi.TakeOwnedFlag) != 0 || (bufID > 0 && len(outBytes) > inlineResultBytes):
			// Keep take()'s Rust buffer until a waiter consumes it. WaitBuffer
			// wraps with no Go copy; Wait copies out and frees. Wrapping here
			// forced every Wait of a large result to allocate a *Buffer it
			// immediately destroyed. The id lives in takeIDs, not callResult,
			// so a one-byte Call does not pay a larger completion object.
			res = callResult{data: outBytes}
			takeID = bufID &^ ffi.TakeOwnedFlag
		default:
			var out []byte
			if len(outBytes) > 0 {
				out = make([]byte, len(outBytes))
				copy(out, outBytes)
			}
			if bufID > 0 {
				_ = ffi.BufFree(s.ptr, bufID)
			}
			res = callResult{data: out}
		}
		s.cgoMu.RUnlock()

		handOff(ticket, res, takeID)
	}
}

// gomaxprocsRefresh is how often drainPipe re-reads GOMAXPROCS into multiP.
// The runtime can change it while the process runs (container CPU limits),
// and reading it takes the scheduler's global lock, so once per completion
// is too often.
const gomaxprocsRefresh = 10 * time.Millisecond

// deliver routes one completion: to its waiter, to the completed map for a
// waiter yet to arrive, or straight to the bin when the owner has abandoned the
// ticket.
//
// The pool permit tracks a busy worker, not an uncollected result. It comes
// back when the work stops and nobody is still waiting: an abandoned ticket,
// or a result parked in completed because Submit had no Wait yet. A live
// waiter returns the permit itself in collectLocked. Releasing it only in
// those two places is what lets pool_size fire-and-forget Submits finish
// without wedging the next Submit, and what keeps a still-running abandoned
// job from sharing its worker (I4).
//
// readied reports that the delivery may have made a goroutine runnable: a
// waiter parked on its channel while at most one call was in flight (see
// waitInternal), or a submitter parked on the permit returned here.
func (s *handleState) deliver(ticket uint64, res callResult, takeID uint64) (readied bool) {
	s.mu.Lock()
	if _, gone := s.abandoned[ticket]; gone {
		delete(s.abandoned, ticket)
		heldPermit := false
		if _, held := s.semTickets[ticket]; held {
			delete(s.semTickets, ticket)
			heldPermit = true
		}
		s.mu.Unlock()

		// Poison is not latched here. drainPipe latches it off the take error
		// before it calls deliver, so it is already set for an abandoned panic
		// as much as for a collected one (I2). A second store here read as a
		// safety net but was unkillable by any mutation, which is how a line
		// that guards nothing comes to look like a line that guards something.
		s.discardTake(takeID)
		if heldPermit {
			// Never blocks: the permit for this ticket is held by definition.
			<-s.sem
		}
		return heldPermit
	}

	if takeID != 0 {
		s.takeIDs[ticket] = takeID
	}
	if ch := s.pending[ticket]; ch != nil {
		// The entry stays, nil, until the waiter has collected: popped its take
		// id and returned its permit (collectLocked). Deleting it here left the
		// ticket live with no waiter in between, and a second Wait registered
		// and parked on a completion that had already been handed out.
		s.pending[ticket] = nil
		ch <- res
		s.mu.Unlock()
		// Read after the send: a waiter that registers as parked later finds
		// the result already buffered and never parks (waitInternal).
		return s.parkedWaiters.Load() > 0
	}
	s.completed[ticket] = res
	// The worker is done and no waiter is registered. The result stays until
	// Wait or Close. The permit does not: leaving it in semTickets wedged the
	// handle after pool_size Submits that nobody waited on.
	//
	// Submit may still be inserting semTickets (the job can finish before that
	// assignment). If the ticket is not here yet, Submit releases when it sees
	// the completed entry.
	heldPermit := false
	if _, held := s.semTickets[ticket]; held {
		delete(s.semTickets, ticket)
		heldPermit = true
	}
	s.mu.Unlock()
	if heldPermit {
		<-s.sem
	}
	return heldPermit
}
