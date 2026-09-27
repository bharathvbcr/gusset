package gusset

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math/bits"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A Call makes one allocation, the result slice it returns. The result
// channel is reused: it used to be two allocations (header and buffer) per
// call, the only two a Call made.
func TestCallAllocatesOnlyItsResult(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()
	in := []byte{0}
	for i := 0; i < 100; i++ { // warm the free list and the maps
		if _, err := h.Call(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	allocs := testing.AllocsPerRun(2000, func() {
		if _, err := h.Call(ctx, in); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 1.05 {
		t.Fatalf("Call made %.2f allocations, want 1 (the result slice)", allocs)
	}
}

// The free list hands back empty channels, refuses one that still holds a
// value, and never grows past the number of waiters that can exist.
func TestWaitChanFreeListRules(t *testing.T) {
	s := &handleState{sem: make(chan struct{}, 2)}
	a := s.waitChanLocked()
	b := s.waitChanLocked()
	c := s.waitChanLocked()
	if cap(a) != 1 || len(a) != 0 {
		t.Fatal("a result channel must be empty with a buffer of one")
	}
	s.recycleWaitChanLocked(a)
	s.recycleWaitChanLocked(b)
	s.recycleWaitChanLocked(c)
	if len(s.waitChans) != 2 {
		t.Fatalf("free list holds %d, want at most cap(sem)=2", len(s.waitChans))
	}
	if got := s.waitChanLocked(); got != b {
		t.Fatal("the most recently returned channel should be reused first")
	}
	full := make(chan callResult, 1)
	full <- callResult{data: []byte("stale")}
	s.waitChans = s.waitChans[:0]
	s.recycleWaitChanLocked(full)
	if len(s.waitChans) != 0 {
		t.Fatal("a channel holding a value was recycled; its stale result would reach the next waiter")
	}
}

// Reused channels under deadlines that fire mid-flight: every caller that gets
// a result gets its own, through the plain receive, the raced receive after
// the deadline, and the abandon path alike.
func TestReusedWaitChansNeverCrossDeliver(t *testing.T) {
	h, err := Open(WithPoolSize(4), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var wg sync.WaitGroup
	var ok, expired atomic.Int64
	errs := make(chan error, 32)
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			for i := 0; i < 400; i++ {
				// Mode 0 echoes its input, so the payload identifies the call.
				in := make([]byte, 1+rng.Intn(40))
				rng.Read(in)
				in[0] = 0
				if len(in) >= 9 {
					binary.LittleEndian.PutUint64(in[1:], uint64(g)<<32|uint64(i))
				}
				ctx := context.Background()
				var cancel context.CancelFunc = func() {}
				var want []byte
				if rng.Intn(2) == 0 {
					// A short job whose deadline lands near its end: the
					// completion races the deadline, which drives the raced
					// receive and the abandon path. Mode 11's result is a
					// function of its iteration count, so it identifies the call.
					iters := uint32(2_000 + rng.Intn(18_000))
					in = []byte{11, 0, 0, 0, 0}
					binary.LittleEndian.PutUint32(in[1:], iters)
					want = spinResult(iters)
					ctx, cancel = context.WithTimeout(ctx, time.Duration(2+rng.Intn(25))*time.Microsecond)
				} else {
					want = in
				}
				switch rng.Intn(3) {
				case 0: // expires around the round trip
					ctx, cancel = context.WithTimeout(ctx, time.Duration(rng.Intn(8))*time.Microsecond)
				case 1: // already expired
					if want[0] != 0 || len(want) == 0 {
						break
					}
					ctx, cancel = context.WithTimeout(ctx, 0)
				}
				out, err := h.Call(ctx, in)
				cancel()
				switch {
				case err == nil:
					ok.Add(1)
					if !bytes.Equal(out, want) {
						errs <- &crossDelivery{want: want, got: out}
						return
					}
				case errors.Is(err, context.DeadlineExceeded):
					expired.Add(1)
				default:
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// Both kinds of outcome must occur, or the reuse paths went unexercised.
	if ok.Load() == 0 || expired.Load() == 0 {
		t.Fatalf("results %d, deadlines %d: need both", ok.Load(), expired.Load())
	}
	t.Logf("results %d, deadlines %d", ok.Load(), expired.Load())
	h.state.mu.Lock()
	n := len(h.state.waitChans)
	h.state.mu.Unlock()
	if n > cap(h.state.sem) {
		t.Fatalf("free list grew to %d, past cap(sem)=%d", n, cap(h.state.sem))
	}
}

// spinResult is diagnostic mode 11's output for iters iterations.
func spinResult(iters uint32) []byte {
	var acc uint64
	for i := uint64(0); i < uint64(iters); i++ {
		acc += i*i ^ bits.RotateLeft64(acc, 7)
	}
	return binary.LittleEndian.AppendUint64(nil, acc)
}

type crossDelivery struct{ want, got []byte }

func (c *crossDelivery) Error() string {
	return "a waiter received another call's result: want " + string(c.want) + ", got " + string(c.got)
}

// fakeWaitState is the part of handleState waitInternal touches, with no
// Rust handle behind it (ptr nil: enterCgo refuses, so no cgo call is made).
func fakeWaitState(ticket uint64) *handleState {
	s := &handleState{
		sem:        make(chan struct{}, 1),
		pending:    make(map[uint64]chan callResult),
		completed:  make(map[uint64]callResult),
		semTickets: map[uint64]struct{}{ticket: {}},
		abandoned:  make(map[uint64]struct{}),
		takeIDs:    make(map[uint64]uint64),
	}
	s.sem <- struct{}{} // the ticket's permit
	return s
}

// Drives both deadline branches of waitInternal deterministically and checks
// that each hands its channel back empty.
func TestWaitDeadlineBranchesRecycleTheirChannel(t *testing.T) {
	const ticket = 7
	for _, raced := range []bool{true, false} {
		for round := 0; round < 50; round++ {
			s := fakeWaitState(ticket)
			ctx, cancel := context.WithCancel(context.Background())
			type out struct {
				res callResult
				err error
			}
			done := make(chan out, 1)
			go func() {
				res, _, err := s.waitInternal(ctx, ticket)
				done <- out{res, err}
			}()
			var ch chan callResult
			eventually(t, "waiter to register", func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				ch = s.pending[ticket]
				return ch != nil
			})
			// Hold mu across the cancel, so the waiter wakes in the deadline
			// branch and blocks on mu before anything is delivered.
			s.mu.Lock()
			cancel()
			time.Sleep(2 * time.Millisecond)
			if raced {
				// What deliver does: out of pending, then one send.
				delete(s.pending, ticket)
				ch <- callResult{data: []byte("late")}
			}
			s.mu.Unlock()

			got := <-done
			if !errors.Is(got.err, context.Canceled) {
				t.Fatalf("raced=%v: err %v, want the deadline", raced, got.err)
			}
			s.mu.Lock()
			_, isAbandoned := s.abandoned[ticket]
			free := len(s.waitChans)
			reused := free == 1 && s.waitChans[0] == ch
			s.mu.Unlock()
			if raced == isAbandoned {
				t.Fatalf("raced=%v: abandoned=%v", raced, isAbandoned)
			}
			if !reused || len(ch) != 0 {
				t.Fatalf("raced=%v: channel not recycled empty (free %d, len %d)", raced, free, len(ch))
			}
			if raced && len(s.sem) != 0 {
				t.Fatal("the raced branch must return the permit")
			}
			if !raced && len(s.sem) != 1 {
				t.Fatal("the abandon branch must leave the permit with the running job")
			}
		}
	}
}
