package gusset

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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
		rng := rand.New(rand.NewPCG(uint64(round), 7))
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
				r := rand.New(rand.NewPCG(seed, uint64(round)))
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
