//go:build unix

package isolate

import (
	"testing"
	"time"
)

// stallProc is a Proc with only what judgeStallLocked reads: a supervisor
// signal, a stall bound and a two-permit pool. No worker runs.
func stallProc(stall time.Duration) *Proc {
	return &Proc{
		sem:     make(chan struct{}, 2),
		tickets: make(map[uint64]*ticketState),
		retire:  make(chan struct{}, 1),
		stall:   stall,
	}
}

// strand records a ticket whose caller gave up and sent the cancel frame,
// then runs the judgement as Wait does after abandoning it.
func (p *Proc) strand(id uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tickets[id] = &ticketState{cancelled: true}
	p.judgeStallLocked()
}

// answer delivers a frame for a stranded ticket, as the reader does.
func (p *Proc) answer(id uint64) {
	p.mu.Lock()
	delete(p.tickets, id)
	p.mu.Unlock()
	p.frames.Add(1)
}

func retired(p *Proc, within time.Duration) bool {
	select {
	case <-p.retire:
		return true
	case <-time.After(within):
		return false
	}
}

// A frame inside the window restarts the judgement (Policy.StallTimeout). If
// the permits are all stranded again when the timer fires, nothing else can
// re-arm it: no Submit gets a permit, so no Wait can abandon. The timer must
// judge again itself, or the generation is never retired.
func TestStall_StrandedAgainInsideTheWindowIsStillRetired(t *testing.T) {
	const stall = 50 * time.Millisecond
	p := stallProc(stall)
	p.strand(1)
	p.strand(2) // every permit stranded: the timer is armed

	time.Sleep(stall / 5)
	p.answer(1) // a slow job answers inside the window...
	p.strand(3) // ...and the next one wedges, while the timer is still pending

	if !retired(p, 20*stall) {
		p.mu.Lock()
		armed, stalled := p.stallArmed, p.stalledLocked()
		p.mu.Unlock()
		t.Fatalf("not retired %v after the permits were stranded again (stall %v); stalled=%v armed=%v", 20*stall, stall, stalled, armed)
	}
	if got := p.retireReason(); got != "unresponsive" {
		t.Fatalf("retire cause = %q; want unresponsive", got)
	}
}

// A worker that keeps answering is not retired, however long it takes: each
// window that sees a frame and a free permit ends the judgement.
func TestStall_AnsweredInsideTheWindowIsNotRetired(t *testing.T) {
	const stall = 50 * time.Millisecond
	p := stallProc(stall)
	p.strand(1)
	p.strand(2)

	time.Sleep(stall / 5)
	p.answer(1)

	if retired(p, 6*stall) {
		t.Fatal("a generation that answered and has a free permit was retired")
	}
	p.mu.Lock()
	armed := p.stallArmed
	p.mu.Unlock()
	if armed {
		t.Fatal("the judgement is still armed with a permit free")
	}
}

// A worker that answers one stranded ticket per window while callers keep
// giving up is making progress: the judgement re-arms and does not retire.
func TestStall_ProgressEveryWindowIsNotRetired(t *testing.T) {
	const stall = 60 * time.Millisecond
	p := stallProc(stall)
	p.strand(1)
	p.strand(2)
	next := uint64(3)
	for i := 0; i < 5; i++ {
		time.Sleep(stall / 2)
		p.answer(next - 2)
		p.strand(next)
		next++
	}
	if retired(p, 0) {
		t.Fatal("a generation answering inside every window was retired")
	}
}
