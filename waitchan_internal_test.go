package gusset

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"math/rand"
	randv2 "math/rand/v2"
	"os"
	"runtime"
	"runtime/metrics"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset/internal/hostprobe"
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
	const rounds = 50
	for _, raced := range []bool{true, false} {
		racedHits := 0
		for round := 0; round < rounds; round++ {
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
			s.mu.Lock()
			_, isAbandoned := s.abandoned[ticket]
			free := len(s.waitChans)
			reused := free == 1 && s.waitChans[0] == ch
			s.mu.Unlock()
			switch {
			case raced && got.err == nil:
				// A starved runner woke the waiter only after the send, with
				// both cases ready, and select took the plain receive. Still
				// correct: the result is the delivered one.
				if string(got.res.data) != "late" {
					t.Fatalf("plain receive returned %q", got.res.data)
				}
			case !errors.Is(got.err, context.Canceled):
				t.Fatalf("raced=%v: err %v, want the deadline", raced, got.err)
			case raced:
				racedHits++
			}
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
		// The raced branch is the point of the raced half. Holding mu across
		// the cancel makes it the usual outcome; require most rounds to take it.
		if raced && racedHits < rounds/2 {
			t.Fatalf("the raced branch ran %d of %d rounds", racedHits, rounds)
		}
	}
}

// checkWaitChans asserts the result-channel free list invariant: every
// recycled channel is empty, no pending entry refers to it (so no send can
// ever reach it), no channel is listed twice, and the list is within cap(sem).
//
// mu is taken with a bound: every send happens under mu, so a send that
// reached a channel already holding a value blocks with mu held, and an
// unbounded Lock here would turn that failure into a hung test.
func checkWaitChans(s *handleState) error {
	deadline := time.Now().Add(10 * time.Second)
	for !s.mu.TryLock() {
		if time.Now().After(deadline) {
			return errors.New("mu held for 10 s: a send under mu blocked on a full channel")
		}
		time.Sleep(100 * time.Microsecond)
	}
	defer s.mu.Unlock()
	if len(s.waitChans) > cap(s.sem) {
		return fmt.Errorf("free list holds %d channels, past cap(sem)=%d", len(s.waitChans), cap(s.sem))
	}
	seen := make(map[chan callResult]bool, len(s.waitChans))
	for _, ch := range s.waitChans {
		if n := len(ch); n != 0 {
			return fmt.Errorf("a recycled channel holds %d value(s): a stale result for the next waiter", n)
		}
		if seen[ch] {
			return errors.New("a channel is on the free list twice: two waiters would share it")
		}
		seen[ch] = true
	}
	for tk, ch := range s.pending {
		if ch != nil && seen[ch] {
			return fmt.Errorf("ticket %d's pending channel is also on the free list: a send can reach a recycled channel", tk)
		}
	}
	return nil
}

// Result-channel reuse under Close, drain-reader exit and deadlines at once.
//
// The existing reuse stress runs one handle that never closes. Here every
// handle is torn down mid-flight — by Close, or by the completion reader
// giving up (drainPipe's exit path) and then Close — while callers with short
// deadlines abandon and race their completions. Every result must be the
// caller's own payload, every error a deadline or ErrClosed, and the free
// list must satisfy checkWaitChans throughout.
//
// GUSSET_WAITCHAN_STRESS sets the duration (default 3s).
func TestReusedWaitChansUnderCloseAndDrainExit(t *testing.T) {
	if testing.Short() {
		t.Skip("stress skipped in -short mode")
	}
	dur := 3 * time.Second
	if v := os.Getenv("GUSSET_WAITCHAN_STRESS"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("GUSSET_WAITCHAN_STRESS=%q: %v", v, err)
		}
		dur = d
	}
	var (
		results, deadlines, closedErrs, rounds, drainKills atomic.Int64
		bad                                                atomic.Int64
		nextID                                             atomic.Uint64
	)
	fail := func(format string, args ...any) {
		if bad.Add(1) <= 20 {
			t.Errorf(format, args...)
		}
	}
	end := time.Now().Add(dur)
	for round := 0; time.Now().Before(end); round++ {
		rounds.Add(1)
		rng := randv2.New(randv2.NewPCG(uint64(round), 7))
		opts := []Option{WithPoolSize(1 + rng.IntN(4)), WithDiagnosticEngine()}
		if rng.IntN(3) == 0 {
			opts = append(opts, withPipeOnly())
		}
		h, err := Open(opts...)
		if err != nil {
			t.Fatal(err)
		}
		s := h.state
		var guard sync.RWMutex // Bytes views are valid only until Close
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for g := 0; g < 12; g++ {
			wg.Add(1)
			go func(seed uint64) {
				defer wg.Done()
				r := randv2.New(randv2.NewPCG(seed, uint64(round)))
				for {
					select {
					case <-stop:
						return
					default:
					}
					id := nextID.Add(1)
					ctx, cancel := context.Background(), context.CancelFunc(func() {})
					switch r.IntN(3) {
					case 0:
						ctx, cancel = context.WithTimeout(ctx, time.Duration(r.IntN(300))*time.Microsecond)
					case 1:
						ctx, cancel = context.WithTimeout(ctx, time.Duration(r.IntN(30))*time.Millisecond)
					}
					var got, want []byte
					var err error
					switch r.IntN(4) {
					case 0, 1: // inline record (<= 48 bytes) or bare ticket + take copy
						n := 11 + r.IntN(200)
						if r.IntN(2) == 0 {
							n = 11 + r.IntN(30)
						}
						want = make([]byte, n)
						want[0], want[1], want[2] = 9, byte(r.IntN(3)), 9 // 0-20 ms, then echo
						binary.LittleEndian.PutUint64(want[3:], id)
						got, err = h.Call(ctx, want)
					case 2: // Submit + Wait, echo
						want = make([]byte, 9+r.IntN(100))
						binary.LittleEndian.PutUint64(want[1:], id)
						var tk uint64
						tk, err = h.Submit(ctx, want)
						if err == nil {
							got, err = h.Wait(ctx, tk)
						}
					case 3: // take-buffer path: result over 4 KiB kept in Rust
						var in *Buffer
						in, err = h.NewBuffer(5000 + r.IntN(3000))
						if err != nil {
							break
						}
						guard.RLock()
						if b := in.Bytes(); b != nil {
							b[0] = 0
							binary.LittleEndian.PutUint64(b[1:], id)
							want = append([]byte(nil), b...)
						}
						guard.RUnlock()
						if want == nil { // closed before we could fill it
							err = ErrClosed
							_ = in.Free()
							break
						}
						var out *Buffer
						out, err = h.CallBuffer(ctx, in)
						if err == nil {
							guard.RLock()
							if b := out.Bytes(); b != nil {
								got = append([]byte(nil), b...)
							} else {
								got = want // handle closed under us: nothing to compare
							}
							guard.RUnlock()
							_ = out.Free()
						}
						_ = in.Free()
					}
					cancel()
					switch {
					case err == nil:
						results.Add(1)
						if !bytes.Equal(got, want) {
							fail("call %d got someone else's result: len %d vs %d", id, len(got), len(want))
						}
					case errors.Is(err, context.DeadlineExceeded):
						deadlines.Add(1)
					case errors.Is(err, ErrClosed):
						closedErrs.Add(1)
						if !s.closed.Load() && !s.drainExited.Load() {
							fail("ErrClosed on an open handle: %v", err)
						}
						return
					case errors.Is(err, ErrBufferBudget), errors.Is(err, ErrUnknownTicket):
						fail("unexpected: %v", err)
					default:
						fail("unexpected error: %v", err)
					}
				}
			}(uint64(g))
		}
		// Let the lanes run, checking the free list as they go.
		for i, n := 0, 1+rng.IntN(20); i < n; i++ {
			time.Sleep(time.Millisecond)
			if err := checkWaitChans(s); err != nil {
				fail("round %d mid-flight: %v", round, err)
			}
		}
		if bad.Load() > 0 {
			t.FailNow() // the handle may be wedged; Close would hang behind it
		}
		if rng.IntN(3) == 0 {
			drainKills.Add(1)
			killDrain(t, s)
			if err := checkWaitChans(s); err != nil {
				fail("round %d after drain exit: %v", round, err)
			}
		}
		guard.Lock()
		if err := h.Close(); err != nil {
			fail("Close: %v", err)
		}
		guard.Unlock()
		close(stop)
		wg.Wait()
		if err := checkWaitChans(s); err != nil {
			fail("round %d after close: %v", round, err)
		}
		s.mu.Lock()
		leftover := 0
		for _, ch := range s.pending {
			if ch != nil {
				leftover++
			}
		}
		s.mu.Unlock()
		if leftover != 0 {
			fail("round %d: %d waiters still registered after every lane returned", round, leftover)
		}
	}
	t.Logf("rounds %d (drain exits %d): results %d, deadlines %d, closed %d",
		rounds.Load(), drainKills.Load(), results.Load(), deadlines.Load(), closedErrs.Load())
	if results.Load() == 0 || deadlines.Load() == 0 || closedErrs.Load() == 0 {
		t.Fatal("a path went unexercised: need results, deadlines and closed errors")
	}
}

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
			// Bounded: a completion that never arrives must fail here with a
			// reason, not hang until the package's 10-minute timeout.
			var res callResult
			select {
			case res = <-ch:
			case <-time.After(10 * time.Second):
				t.Fatalf("no completion for ticket %d within 10 s", ticket)
			}
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

// schedTransitions is how many goroutine runnable-to-running transitions the
// scheduler has sampled since the process started.
func schedTransitions(t *testing.T) uint64 {
	t.Helper()
	s := []metrics.Sample{{Name: "/sched/latencies:seconds"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindFloat64Histogram {
		t.Skipf("/sched/latencies:seconds unsupported here (kind %v)", s[0].Value.Kind())
	}
	var n uint64
	for _, c := range s[0].Value.Float64Histogram().Counts {
		n += c
	}
	return n
}

// A serial Call, with more than one P, must reach its caller without going
// through the scheduler.
//
// The waiter used to park on its result channel for every call, so the
// reader's send readied it, and the reader yielded on every poll so a readied
// goroutine could run. Each of those is a runnable-to-running transition, and
// each one taken while a P sat idle woke a thread with a system call first:
// 200k serial Calls on an M5 Pro spent 1.5 us each in runtime.wakep at
// ~0.7 us jobs and 5.9 us at ~7 us jobs, a cost that grew with the job.
// Now a lone waiter polls for its result and the reader yields only after a
// delivery that readied someone, so a stream of serial Calls schedules
// nothing at all. The old path showed four to eight sampled transitions per
// call here (16k-30k for 4000 calls on an M5 Pro); this one shows tens.
func TestSerialCall_HandsOffWithoutScheduling(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	h, err := Open(WithPoolSize(4), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()
	in := []byte{0}
	for i := 0; i < 200; i++ { // past the first-call and refresh paths
		if _, err := h.Call(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	const calls = 4000
	before := schedTransitions(t)
	for i := 0; i < calls; i++ {
		if _, err := h.Call(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	got := schedTransitions(t) - before
	// One sampled transition per call. The code this guards made four to eight
	// per call (16k-30k for 4000), so one per call sits well below it; the old
	// quiet path made tens, but a loaded host or the race detector adds GC
	// workers and stalled calls (510-1385 were seen with a limit of calls/8, on
	// a build that was correct), so a tighter bound measured the host.
	if limit := uint64(calls); got > limit {
		t.Fatalf("%d serial Calls made %d sampled scheduler transitions, want <= %d: "+
			"the waiter parked or the reader yielded on the handoff", calls, got, limit)
	}
	t.Logf("%d serial Calls: %d sampled scheduler transitions", calls, got)
}

// A serial caller must leave a P for every other goroutine.
//
// The quiet handoff runs two loops that do not pass through the scheduler:
// the waiter polling its result channel and the reader polling the ring
// without yielding. With two Ps that is every P, and a goroutine readied
// meanwhile waited for one of them to be preempted. Measured with a goroutine
// sleeping 200 us between serial Calls at GOMAXPROCS=2 on an M5 Pro, it woke
// 0.3-3.5 ms late at the median (p99 5-21 ms), against 3-8 us with the waiter
// parking and the reader yielding on every poll, and 32 us in a process
// making no Calls at all. Under a Linux 2-CPU quota (podman --cpus=2) the
// median was ~1 ms and p99 15-19 ms, against 4 us.
//
// The check sees the starvation only where it shows. With the process pinned
// to two Linux CPUs (taskset 0,1) the code before the fix passed it 55 times
// out of 55, on a darwin host at load 17.5 it passed 13 of 16, and on GitHub's
// macos-latest runners it woke 25-45 us late, inside the bound. On GitHub's
// Linux runners it failed at 6.3-14 ms in the runs first recorded (the
// budget below rests on 6.3 ms as that floor; a later 40-run probe saw
// 1.86-15.9 ms, see the budget). No tail
// statistic set beside the median separated the two builds on Linux either
// (bench/results/handoff-gomaxprocs/wake-tail/). TestQuietProcs_FollowsGOMAXPROCS
// and TestPollResult_Gates check the gate itself rather than a wake time, and
// failed on that code in every run, idle or at load 13-16.
func TestSerialCall_LeavesAPForOtherGoroutines(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	const nap = 200 * time.Microsecond
	// wakes times a goroutine's sleeps and returns how late each one ended.
	wakes := func() []time.Duration {
		var late []time.Duration
		for deadline := time.Now().Add(2 * time.Second); len(late) < 300 && time.Now().Before(deadline); {
			t0 := time.Now()
			time.Sleep(nap)
			late = append(late, time.Since(t0)-nap)
		}
		slices.Sort(late)
		return late
	}

	// The same sleeps with no Call running, but with the scheduler cycling as
	// it does beside correct Calls (the waiter parks and the reader yields):
	// what this host does to a 200 us nap on its own. A fixed limit measured
	// the host (a median of 127-170 us was seen on a build with nothing wrong,
	// under load), so the claim is relative to this.
	//
	// The baseline must not be an idle process. With every P idle, Go's Linux
	// netpoller rounds a sub-millisecond timer wait up to epoll_wait(1)
	// (runtime/netpoll_epoll.go), so an idle 200 us nap wakes ~860 us late on
	// every Linux host. That baseline skipped this test on every Linux runner,
	// and GUSSET_REQUIRE_PROBES made the skip a failure (Matrix 39270656716).
	// A pure spinner sits on the same floor; a goroutine cycling through the
	// scheduler checks timers on each pass. Measured on GitHub runners
	// (probe/w1-linux-evidence, run 40058658715, 10 runs each): this baseline
	// 2-5 us on ubuntu-latest and ubuntu-24.04-arm and 0.5-17 us on
	// macos-latest; the fixed build beside Calls 1.5-5 us on Linux; the code
	// before the fix 6.3-14 ms on Linux, every run.
	var cycling atomic.Bool
	cycled := make(chan struct{})
	go func() {
		defer close(cycled)
		for !cycling.Load() {
			runtime.Gosched()
		}
	}()
	baseline := wakes()
	cycling.Store(true)
	<-cycled
	base := baseline[len(baseline)/2]
	if base > 100*time.Microsecond {
		hostprobe.Skip(t, "this host wakes a 200 us nap %v late at the median with the scheduler "+
			"cycling and no Calls running: too noisy to show the cost of a handoff "+
			"(TestPollResult_Gates and TestQuietProcs_FollowsGOMAXPROCS check the gate itself)", base)
	}

	var stop atomic.Bool
	done := make(chan error, 1)
	go func() {
		ctx := context.Background()
		in := []byte{0}
		for !stop.Load() {
			if _, err := h.Call(ctx, in); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	time.Sleep(20 * time.Millisecond) // past the first-call and refresh paths
	late := wakes()
	stop.Store(true)
	if err := <-done; err != nil {
		t.Fatalf("serial Call: %v", err)
	}

	median := late[len(late)/2]
	// 1 ms over the baseline, set from GitHub Linux runner medians (ft-c689767b).
	// The earlier 100 us over the baseline failed correct code: ten failing
	// rows over three days had medians of 121.4 (two rows of one run), 180.6,
	// 119.3, 210.8, 144.6, 109.8, 117.4, 103.5 (PR #61, Rust 1.97 floor, run
	// 42754607690) and 167.6 us (two of the runs were on a push that changed
	// only comments), against a baseline of 0.3-0.6 us in the same runs. They
	// came under the full suite's load. The budget must clear the slowest
	// correct median with margin and sit far under the broken code's: at least
	// 2x 210.8 us = 422 us, and at most 1/5 of 6.3 ms (the earlier recorded
	// floor of the code before the fix on these runners, above) = 1.26 ms.
	// 1 ms is 4.7x the slowest correct median and 6.3x under that floor.
	//
	// Probe, test alone on ubuntu-latest and ubuntu-24.04-arm, GOMAXPROCS=2
	// (run 42758127775): the fixed build 20 of 20 passes each (medians 1.5-6.7
	// us); the code before the fix, quietAt(procs) = procs > 1, failed 40 of 40
	// at 1.86-15.9 ms. The thinnest margin is ARM's 1.86 ms, about 1.9x the
	// 1 ms budget. In an earlier probe run (42757990314, aborted by a shell
	// error after one planted run) one planted sample in 21 passed at 81 us,
	// which the previous budget would also have passed. Linux runners are the
	// only hosts where this discriminates; see the passes noted above on
	// darwin and under taskset.
	if limit := base + time.Millisecond; median > limit {
		t.Fatalf("a goroutine sleeping %v beside serial Calls at GOMAXPROCS=2 woke %v late "+
			"at the median over %d wakes (max %v), want <= %v (%v with no Calls, plus 1 ms): "+
			"the waiter and the reader held both Ps", nap, median, len(late), late[len(late)-1], limit, base)
	}
	t.Logf("%d wakes beside serial Calls: median %v late (%v with none), max %v", len(late), median, base, late[len(late)-1])
}

// pollResult polls only for a lone call, and only with a P to spare.
//
// With one P the reader cannot run while the waiter polls, so a polling
// waiter only delays the result it is waiting for: a version that polled
// there took 56 us per serial Call at GOMAXPROCS=1, against 3.5 us parked.
// With two, the polling waiter and the quiet reader held both Ps
// (TestSerialCall_LeavesAPForOtherGoroutines). With several calls in flight a
// poll would hold a core per caller.
func TestPollResult_Gates(t *testing.T) {
	newState := func(procs int, inFlight int) *handleState {
		s := &handleState{sem: make(chan struct{}, 4)}
		s.quietProcs.Store(quietAt(procs))
		for i := 0; i < inFlight; i++ {
			s.sem <- struct{}{}
		}
		return s
	}
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		procs    int
		inFlight int
	}{
		{"one P", 1, 1},
		{"two Ps", 2, 1},
		{"two in flight", quietMinProcs, 2},
	} {
		ch := make(chan callResult, 1)
		s := newState(tc.procs, tc.inFlight)
		d := minElapsed(5, func() {
			if _, ok := s.pollResult(ctx, ch); ok {
				t.Fatalf("%s: pollResult took a result that was never sent", tc.name)
			}
		})
		if d >= waiterSpin {
			t.Fatalf("%s: pollResult polled for %v at best of 5; it must not poll at all", tc.name, d)
		}
	}

	// Lone, a P to spare: a result sent meanwhile is taken without parking.
	ch := make(chan callResult, 1)
	go func() { ch <- callResult{data: []byte("x")} }()
	s := newState(quietMinProcs, 1)
	var res callResult
	var ok bool
	for deadline := time.Now().Add(5 * time.Second); !ok && time.Now().Before(deadline); {
		res, ok = s.pollResult(ctx, ch)
	}
	if !ok || string(res.data) != "x" {
		t.Fatalf("pollResult = %q, %v; want the sent result", res.data, ok)
	}

	// An ended context stops the poll at once.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	ended := newState(quietMinProcs, 1)
	d := minElapsed(5, func() {
		if _, ok := ended.pollResult(cctx, make(chan callResult, 1)); ok {
			t.Fatal("pollResult reported a result on a cancelled context")
		}
	})
	if d >= waiterSpin {
		t.Fatalf("pollResult ran %v past a cancelled context at best of 5", d)
	}
}

// minElapsed is the fastest of n runs of f. A poll that does not stop when it
// should takes the whole spin window on every run, so its fastest run still
// shows it; a correct one is only slowed by the host, and one run in n that
// the scheduler leaves alone is enough to show that. A single timed run
// failed under cgocheck2 on a loaded host (258 us past a cancelled context).
func minElapsed(n int, f func()) time.Duration {
	best := time.Duration(1<<63 - 1)
	for i := 0; i < n; i++ {
		start := time.Now()
		f()
		if d := time.Since(start); d < best {
			best = d
		}
	}
	return best
}

// The quiet handoff follows GOMAXPROCS while a handle is open, both ways.
//
// The runtime changes GOMAXPROCS under a running process when a container's
// CPU limit changes, so the gate is re-read on the reader's side every
// gomaxprocsRefresh rather than fixed at Open.
func TestQuietProcs_FollowsGOMAXPROCS(t *testing.T) {
	for procs, want := range map[int]bool{1: false, 2: false, quietMinProcs: true, 8: true} {
		if got := quietAt(procs); got != want {
			t.Errorf("quietAt(%d) = %v, want %v", procs, got, want)
		}
	}

	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state
	if s.quietProcs.Load() {
		t.Fatal("handle opened at GOMAXPROCS=2 runs the quiet handoff")
	}

	ctx := context.Background()
	in := []byte{0}
	callUntil := func(want bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); s.quietProcs.Load() != want; {
			if time.Now().After(deadline) {
				t.Fatalf("quiet handoff still %v at GOMAXPROCS=%d after 5s of Calls",
					!want, runtime.GOMAXPROCS(0))
			}
			if _, err := h.Call(ctx, in); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Millisecond)
		}
	}
	runtime.GOMAXPROCS(quietMinProcs)
	callUntil(true)
	runtime.GOMAXPROCS(2)
	callUntil(false)
}
