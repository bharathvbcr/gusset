package gusset

// waitChanLocked returns an empty result channel, reused when one is free.
func (s *handleState) waitChanLocked() chan callResult {
	if n := len(s.waitChans); n > 0 {
		ch := s.waitChans[n-1]
		s.waitChans[n-1] = nil
		s.waitChans = s.waitChans[:n-1]
		return ch
	}
	return make(chan callResult, 1)
}

// recycleWaitChanLocked returns a channel for reuse. The caller has taken it
// out of pending under mu, so no send can reach it any more; a channel that
// still holds a value, which that rule excludes, is dropped rather than
// reused, so a stale result can never reach the next waiter.
func (s *handleState) recycleWaitChanLocked(ch chan callResult) {
	if len(ch) == 0 && len(s.waitChans) < cap(s.sem) {
		s.waitChans = append(s.waitChans, ch)
	}
}

func (s *handleState) popTakeIDLocked(ticket uint64) uint64 {
	id := s.takeIDs[ticket]
	delete(s.takeIDs, ticket)
	return id
}

// takeUnclaimedLocked drops every result no waiter has claimed (the completed
// map) and returns the take buffer ids among them for the caller to free.
//
// A result already handed to a waiter keeps its id. That waiter holds a view of
// the buffer and pops the id itself; resetting takeIDs wholesale, as close and
// drainPipe's exit both did, made it pop 0 and return the view as if it were Go
// memory — read after Close freed it, or after this very path had freed it
// through bufFree. The id is how wait knows to copy under cgoMu, and how it
// learns the handle closed in between.
func (s *handleState) takeUnclaimedLocked() []uint64 {
	var ids []uint64
	for ticket := range s.completed {
		if id := s.popTakeIDLocked(ticket); id != 0 {
			ids = append(ids, id)
		}
	}
	s.completed = make(map[uint64]callResult)
	return ids
}

// collectLocked ends a waiter's claim on a ticket whose outcome it holds: it
// removes the waiter's pending entry, pops the take buffer id and returns the
// permit, all in one critical section.
//
// These were separate steps under separate lock holds. Between them the
// ticket was still live in semTickets with no pending entry and no completed
// result, and a second Wait read that as "nobody is waiting": it registered and
// parked, to its deadline or forever, on a completion already handed out.
func (s *handleState) collectLocked(ticket uint64) uint64 {
	delete(s.pending, ticket)
	takeID := s.popTakeIDLocked(ticket)
	s.releaseSemLocked(ticket)
	return takeID
}

// discardTake releases a take() buffer nobody is going to consume.
func (s *handleState) discardTake(takeID uint64) {
	if takeID != 0 {
		_ = s.bufFree(takeID)
	}
}

func (s *handleState) releaseSemLocked(ticket uint64) {
	if _, ok := s.semTickets[ticket]; ok {
		delete(s.semTickets, ticket)
		<-s.sem // never blocks: this ticket's permit is held by definition
	}
}
