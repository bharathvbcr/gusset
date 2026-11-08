package gusset

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The hammer suite drives every boundary path at once — Call, Submit/Wait,
// WaitBuffer, CallBuffer, NewBuffer/Free, cancellation, tiny deadlines,
// abandoned non-cooperative jobs, panics that poison, concurrent Close from
// many goroutines, and handles dropped for the AddCleanup backstop — and then
// checks the invariants each of those paths must restore:
//
//   - every pool permit is back (len(sem) == 0) and no ticket, waiter,
//     completed result, abandoned ticket or take buffer is left behind (I4);
//   - the buffer budget charge returns to zero;
//   - no goroutine outlives its handle;
//   - Rust live bytes return to their baseline;
//   - the Go thread count stays bounded (R11);
//   - every result that came back with a nil error is byte-for-byte right.
//
// By default it runs for two seconds, so `go test ./...` stays fast; -short
// skips it. GUSSET_STRESS=5m runs it for five minutes:
//
//	GUSSET_STRESS=2m go test -race -run Hammer -count=1 .
//	GUSSET_STRESS=2m GOGC=1 go test -run Hammer -count=1 .
//
// Every run prints its seed, and GUSSET_HAMMER_SEED replays one:
//
//	GUSSET_HAMMER_SEED=1790763872967289580 GUSSET_STRESS=1m go test -race -run Hammer -count=1 .
//
// A seed fixes each lane's sequence of operations, sizes and deadlines, not
// the interleaving of the lanes or of the Rust workers, so a replay reaches
// the same mix of paths without promising the same race.

func hammerDuration(t *testing.T) time.Duration {
	if testing.Short() {
		t.Skip("hammer suite skipped in -short mode")
	}
	d := 2 * time.Second
	if v := os.Getenv("GUSSET_STRESS"); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("GUSSET_STRESS=%q: %v", v, err)
		}
		d = parsed
	}
	return d
}

// hammerSeed is GUSSET_HAMMER_SEED when set, so a seed a failing run printed
// can be replayed, and otherwise a fresh one from the clock.
func hammerSeed() (uint64, error) {
	v := os.Getenv("GUSSET_HAMMER_SEED")
	if v == "" {
		return uint64(time.Now().UnixNano()), nil
	}
	seed, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("GUSSET_HAMMER_SEED=%q: %w", v, err)
	}
	return seed, nil
}

func TestHammerSeed_ReplaysTheRecordedSeed(t *testing.T) {
	// The 2026-09-30 crash's seed, which could not be replayed before.
	t.Setenv("GUSSET_HAMMER_SEED", "1790763872967289580")
	if seed, err := hammerSeed(); err != nil || seed != 1790763872967289580 {
		t.Fatalf("hammerSeed() = %d, %v; want the recorded seed", seed, err)
	}
	// The full uint64 range: seeds are printed as uint64, so any printed one
	// must parse back.
	t.Setenv("GUSSET_HAMMER_SEED", "18446744073709551615")
	if seed, err := hammerSeed(); err != nil || seed != 1<<64-1 {
		t.Fatalf("hammerSeed() = %d, %v; want the largest uint64", seed, err)
	}
	for _, bad := range []string{"-1", "0x10", "12 ", "seed", "18446744073709551616"} {
		t.Setenv("GUSSET_HAMMER_SEED", bad)
		if seed, err := hammerSeed(); err == nil {
			t.Errorf("GUSSET_HAMMER_SEED=%q: got seed %d, want an error rather than a different run", bad, seed)
		}
	}
	t.Setenv("GUSSET_HAMMER_SEED", "")
	a, errA := hammerSeed()
	time.Sleep(time.Microsecond)
	b, errB := hammerSeed()
	if errA != nil || errB != nil || a == b {
		t.Fatalf("unset: hammerSeed() = %d (%v), %d (%v); want two fresh seeds", a, errA, b, errB)
	}
}

// The abandon oracle wants the body back. Mode 9 returns the body only when
// something follows its two-byte prefix; with nothing after it the engine
// echoes the prefix itself. Both halves are pinned here: the engine's echo of
// an empty remainder, and a builder that never asks for one. The seed is one
// whose draw 5 was an empty body under the old r.IntN(6000) builder, so the
// check is cheap under -race and still fails on that builder.
func TestHammer_AbandonInputNeverHasAnEmptyBody(t *testing.T) {
	r := rand.New(rand.NewPCG(2470, 7))
	for i := range 200 {
		in, body := abandonInput(r)
		if len(body) == 0 {
			t.Fatalf("draw %d: empty body; mode 9 echoes % x and the oracle would want 0 bytes", i, in)
		}
		if in[0] != 9 || in[1] < 1 || in[1] > 4 || !bytes.Equal(in[2:], body) || body[0] != 0 {
			t.Fatalf("draw %d: in % x is not [9, 1..4] followed by an echo body % x", i, in[:min(len(in), 8)], body[:min(len(body), 8)])
		}
	}

	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, c := range []struct{ in, want []byte }{
		{[]byte{9, 1}, []byte{9, 1}}, // empty remainder: the whole input
		{[]byte{9, 1, 0}, []byte{0}},
		{[]byte{9, 2, 0, 0xAB}, []byte{0, 0xAB}},
	} {
		out, err := h.Call(ctx, c.in)
		if err != nil || !bytes.Equal(out, c.want) {
			t.Errorf("Call(% x) = % x, %v; want % x", c.in, out, err, c.want)
		}
	}
}

// hammerSlot is one lane of handles: the live one, replaced when it is
// poisoned or closed, and every state it ever held for the final checks.
type hammerSlot struct {
	cur    atomic.Pointer[Handle]
	mu     sync.Mutex
	states []*handleState
}

type hammer struct {
	t *testing.T
	// guards holds a *sync.RWMutex per handle state. A slice from Bytes is
	// valid only until Free or Close (see Buffer.Bytes), so a lane holds the
	// read side while it touches one and a closer takes the write side. Every
	// other call — Submit, Wait, WaitBuffer, CallBuffer, Free — still races
	// Close freely.
	guards     sync.Map
	slots      []*hammerSlot
	unexpected atomic.Int64
	reported   atomic.Int64
	counts     [opCount]atomic.Int64
	okBytes    atomic.Int64
	opens      atomic.Int64
	closes     atomic.Int64
	dropped    atomic.Int64
	poisonings atomic.Int64
}

const (
	opEcho = iota
	opSlow
	opCoop
	opSubmitWait
	opWaitBuffer
	opCallBuffer
	opAllocated
	opAbandon
	opPanic
	opDoubleWait
	opDiscard
	opCount
)

var opNames = [opCount]string{"echo", "slow", "coop", "submit-wait", "wait-buffer", "call-buffer", "allocated", "abandon", "panic", "double-wait", "discard"}

func (hm *hammer) failf(format string, args ...any) {
	hm.unexpected.Add(1)
	if hm.reported.Add(1) <= 25 {
		hm.t.Errorf(format, args...)
	}
}

func (hm *hammer) guard(h *Handle) *sync.RWMutex {
	g, _ := hm.guards.LoadOrStore(h.state, new(sync.RWMutex))
	return g.(*sync.RWMutex)
}

func (hm *hammer) open(r *rand.Rand) (*Handle, error) {
	opts := []Option{WithPoolSize(1 + r.IntN(6)), WithDiagnosticEngine()}
	if r.IntN(4) == 0 {
		opts = append(opts, withPipeOnly())
	}
	if r.IntN(3) == 0 {
		opts = append(opts, WithBufferBudget(int64(256<<10+r.IntN(1<<20))))
	}
	h, err := Open(opts...)
	if err == nil {
		hm.opens.Add(1)
	}
	return h, err
}

// replace swaps a fresh handle in for old, if old is still current, and then
// disposes of old: closed by several goroutines at once, or (rarely) simply
// dropped so the AddCleanup backstop has to close it.
func (hm *hammer) replace(s *hammerSlot, old *Handle, r *rand.Rand) {
	h, err := hm.open(r)
	if err != nil {
		hm.failf("Open: %v", err)
		return
	}
	if !s.cur.CompareAndSwap(old, h) {
		_ = h.Close()
		return
	}
	s.mu.Lock()
	s.states = append(s.states, h.state)
	s.mu.Unlock()

	if r.IntN(8) == 0 {
		hm.dropped.Add(1)
		return // the backstop closes it once nothing references it
	}
	closers := 1 + r.IntN(4)
	errs := make(chan error, closers)
	g := hm.guard(old)
	g.Lock()
	for i := 0; i < closers; i++ {
		go func() { errs <- old.Close() }()
	}
	first := <-errs
	for i := 1; i < closers; i++ {
		if e := <-errs; fmt.Sprint(e) != fmt.Sprint(first) {
			hm.failf("concurrent Close disagreed: %v vs %v", first, e)
		}
	}
	g.Unlock()
	if first != nil {
		hm.failf("Close: %v", first)
	}
	hm.closes.Add(1)
}

// expected reports whether err is an outcome the operation may legitimately
// see on handle h: a closed-handle error only once h is closed, poison only
// once h is poisoned, a deadline only where the caller set a short one.
func expected(h *Handle, err error, shortDeadline, mayPanic bool) bool {
	s := h.state
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return shortDeadline
	// Every closed-handle error is ErrClosed. Matching it by type rather than
	// by the two texts proves that under every race the hammer reaches.
	case errors.Is(err, ErrClosed) || strings.Contains(msg, "buffer is freed or closed"):
		return s.closed.Load()
	case errors.Is(err, ErrPanic):
		return mayPanic
	case errors.Is(err, ErrPoisoned):
		return s.poisoned.Load()
	case errors.Is(err, ErrBufferBudget):
		return s.bufBudget > 0
	}
	return false
}

func payload(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	if n > 0 {
		b[0] = 0 // diagnostic mode 0: echo
	}
	return b
}

// abandonInput builds opAbandon's job: diagnostic mode 9 (a delay of k ten-ms
// units that never checks its context), then an echo body. The body is the
// expected result, so it is at least one byte, as the other mode-9 builders'
// are: mode 9 with nothing after its prefix echoes the whole input, prefix
// included, and an empty body made the oracle want 0 bytes where the engine
// correctly returned `09 0k` (nightly 39575563846 and 39754401724).
func abandonInput(r *rand.Rand) (in, body []byte) {
	body = payload(r, 1+r.IntN(6000))
	if len(body) > inlineResultBytes-3 {
		body = body[:inlineResultBytes-3]
	}
	in = append([]byte{9, byte(1 + r.IntN(4))}, body...)
	return in, body
}

func (hm *hammer) ctx(r *rand.Rand) (context.Context, context.CancelFunc, bool) {
	switch r.IntN(10) {
	case 0, 1:
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(1+r.IntN(3000))*time.Microsecond)
		return ctx, cancel, true
	case 2:
		ctx, cancel := context.WithCancel(context.Background())
		d := time.Duration(r.IntN(2000)) * time.Microsecond
		timer := time.AfterFunc(d, cancel)
		return ctx, func() { timer.Stop(); cancel() }, true
	default:
		// Long enough that only a hang trips it on a loaded CI runner under
		// -race: every job here is at most 40 ms, so 60 s is not load.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		return ctx, cancel, false
	}
}

func (hm *hammer) check(h *Handle, op int, err error, short bool) {
	if !expected(h, err, short, op == opPanic) {
		hm.failf("%s: unexpected error on handle (closed=%v poisoned=%v): %v",
			opNames[op], h.state.closed.Load(), h.state.poisoned.Load(), err)
	}
}

func (hm *hammer) verify(op int, got, want []byte) {
	if !bytes.Equal(got, want) {
		i := firstDiff(got, want)
		hm.failf("%s: corrupted result: got %d bytes, want %d (first diff at %d: got % x, want % x)",
			opNames[op], len(got), len(want), i, got[i:min(i+8, len(got))], want[i:min(i+8, len(want))])
		return
	}
	hm.okBytes.Add(int64(len(got)))
}

// ticketState describes where a ticket sits in the handle's bookkeeping, so a
// stalled waiter's report says whether its completion was ever delivered.
func ticketState(s *handleState, tk uint64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, live := s.semTickets[tk]
	ch, waiting := s.pending[tk]
	_, done := s.completed[tk]
	_, gone := s.abandoned[tk]
	return fmt.Sprintf("ticket %d live=%v waiter=%v delivered=%v completed=%v abandoned=%v permits %d/%d drainExited=%v",
		tk, live, waiting, waiting && ch == nil, done, gone, len(s.sem), cap(s.sem), s.drainExited.Load())
}

func firstDiff(a, b []byte) int {
	for i := 0; i < min(len(a), len(b)); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// step runs one random operation against the slot's current handle.
func (hm *hammer) step(s *hammerSlot, r *rand.Rand) {
	h := s.cur.Load()
	op := r.IntN(opCount)
	if op == opPanic && r.IntN(20) != 0 {
		op = opEcho // panics poison the handle: keep them rare
	}
	hm.counts[op].Add(1)
	ctx, cancel, short := hm.ctx(r)
	defer cancel()

	switch op {
	case opEcho:
		in := payload(r, r.IntN(inlineResultBytes+1))
		out, err := h.Call(ctx, in)
		if err != nil {
			hm.check(h, op, err, short)
			break
		}
		hm.verify(op, out, in)

	case opSlow: // non-cooperative delay, then echo
		body := payload(r, 1+r.IntN(200))
		in := append([]byte{9, byte(r.IntN(3))}, body...)
		out, err := h.Call(ctx, in)
		if err != nil {
			hm.check(h, op, err, short)
			break
		}
		hm.verify(op, out, body)

	case opCoop: // cooperative sleep loop
		out, err := h.Call(ctx, []byte{5, byte(r.IntN(4))})
		if err != nil {
			hm.check(h, op, err, short)
			break
		}
		hm.verify(op, out, []byte{5, 0})

	case opSubmitWait:
		in := payload(r, r.IntN(inlineResultBytes+1))
		tk, err := h.Submit(ctx, in)
		if err != nil {
			hm.check(h, op, err, short)
			break
		}
		if r.IntN(4) == 0 {
			runtime.Gosched() // let the result land in completed first
		}
		out, err := h.Wait(ctx, tk)
		if err != nil {
			hm.check(h, op, err, short)
			break
		}
		hm.verify(op, out, in)
		// The ticket is spent now, wherever it went.
		if _, err := h.Wait(ctx, tk); !errors.Is(err, ErrUnknownTicket) && !expected(h, err, short, false) {
			hm.failf("second Wait on a spent ticket: %v", err)
		}

	case opWaitBuffer:
		in := payload(r, r.IntN(inlineResultBytes+1))
		tk, err := h.Submit(ctx, in)
		if err != nil {
			hm.check(h, op, err, short)
			break
		}
		buf, err := h.WaitBuffer(ctx, tk)
		if err != nil {
			hm.check(h, op, err, short)
			break
		}
		// Bytes is nil once the handle closes; until then it is the result.
		g := hm.guard(h)
		g.RLock()
		if b := buf.Bytes(); b != nil || !h.state.closed.Load() {
			hm.verify(op, b, in)
		}
		g.RUnlock()
		// The one-in-ten buffer left to its cleanup is otherwise unreachable
		// once Bytes returns, and the cleanup frees the Rust memory verify is
		// still reading; the guard does not exclude cleanups.
		runtime.KeepAlive(buf)
		if r.IntN(10) != 0 {
			_ = buf.Free()
		}

	case opCallBuffer:
		n := inlineResultBytes + 1 + r.IntN(128<<10)
		in, err := h.NewBuffer(n)
		if err != nil {
			hm.check(h, op, err, short)
			break
		}
		want := payload(r, n)
		g := hm.guard(h)
		g.RLock()
		b := in.Bytes()
		copy(b, want)
		g.RUnlock()
		if b == nil {
			if !h.state.closed.Load() {
				hm.failf("NewBuffer returned a buffer with no bytes on an open handle")
			}
			_ = in.Free()
			break
		}
		out, err := h.CallBuffer(ctx, in)
		if err != nil {
			hm.check(h, op, err, short)
		} else {
			g.RLock()
			if ob := out.Bytes(); ob != nil || !h.state.closed.Load() {
				hm.verify(op, ob, want)
			}
			g.RUnlock()
			runtime.KeepAlive(out) // see opWaitBuffer
			if r.IntN(10) != 0 {
				_ = out.Free()
			}
		}
		// Rarely leave the input to the cleanup backstop and its budget refund.
		if r.IntN(12) != 0 {
			_ = in.Free()
		}

	case opAllocated: // diagnostic mode 16: generated output, pattern i ^ seed
		n := r.IntN(256 << 10)
		seed := byte(r.Uint32())
		in := []byte{16, 0, 0, 0, 0, seed}
		binary.LittleEndian.PutUint32(in[1:5], uint32(n))
		out, err := h.Call(ctx, in)
		if err != nil {
			hm.check(h, op, err, short)
			break
		}
		want := make([]byte, n)
		for i := range want {
			want[i] = byte(i) ^ seed
		}
		hm.verify(op, out, want)

	case opAbandon: // a non-cooperative job the caller walks away from
		in, body := abandonInput(r)
		short := true
		actx, acancel := context.WithTimeout(ctx, time.Duration(r.IntN(8000))*time.Microsecond)
		out, err := h.Call(actx, in)
		acancel()
		if err != nil {
			hm.check(h, op, err, short)
			break
		}
		hm.verify(op, out, body)

	case opPanic:
		in := []byte{1}
		if r.IntN(2) == 0 {
			in = []byte{15, byte(r.IntN(3))} // payload whose destructor panics
		}
		_, err := h.Call(ctx, in)
		switch {
		case err == nil:
			hm.failf("panic: a panicking job returned no error")
		case errors.Is(err, ErrPanic):
			hm.poisonings.Add(1)
			if !h.state.poisoned.Load() {
				hm.failf("panic: ErrPanic returned but the handle is not poisoned (I2)")
			}
		default:
			hm.check(h, op, err, short)
		}

	case opDoubleWait:
		in := append([]byte{9, 1}, payload(r, 1+r.IntN(64))...)
		tk, err := h.Submit(ctx, in)
		if err != nil {
			hm.check(h, op, fmt.Errorf("submit: %w", err), short)
			break
		}
		// A foreign or never-issued ticket is refused, not parked on.
		if _, err := h.Wait(ctx, tk+1<<40); !errors.Is(err, ErrUnknownTicket) && !expected(h, err, short, false) {
			hm.failf("wait on unknown ticket: %v", err)
		}
		res := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() {
				out, err := h.Wait(ctx, tk)
				if err == nil && !bytes.Equal(out, in[2:]) {
					err = fmt.Errorf("corrupted double-wait result")
				}
				res <- err
			}()
		}
		var okN int
		for i := 0; i < 2; i++ {
			err := <-res
			switch {
			case err == nil:
				okN++
			case errors.Is(err, ErrTicketBusy), errors.Is(err, ErrUnknownTicket):
			default:
				hm.check(h, op, fmt.Errorf("wait (%s): %w", ticketState(h.state, tk), err), short)
			}
		}
		if okN > 1 {
			hm.failf("double-wait: one result was delivered twice")
		}

	case opDiscard: // Discard racing a Wait for one result, often a take buffer
		n := r.IntN(64 << 10)
		seed := byte(r.Uint32())
		in := []byte{16, 0, 0, 0, 0, seed}
		binary.LittleEndian.PutUint32(in[1:5], uint32(n))
		if r.IntN(3) == 0 {
			in = append([]byte{9, 1}, payload(r, 1+r.IntN(64))...) // still running
		}
		tk, err := h.Submit(ctx, in)
		if err != nil {
			hm.check(h, op, fmt.Errorf("submit: %w", err), short)
			break
		}
		if r.IntN(2) == 0 {
			runtime.Gosched() // let the result land in completed first
		}
		waited := make(chan error, 1)
		racing := r.IntN(2) == 0
		if racing {
			go func() {
				_, err := h.Wait(ctx, tk)
				waited <- err
			}()
		}
		derr := h.Discard(tk)
		switch {
		case derr == nil:
		case errors.Is(derr, ErrTicketBusy), errors.Is(derr, ErrUnknownTicket):
			if !racing {
				hm.failf("discard: %v with no other claimant (%s)", derr, ticketState(h.state, tk))
			}
		default:
			hm.check(h, op, fmt.Errorf("discard (%s): %w", ticketState(h.state, tk), derr), short)
		}
		if racing {
			werr := <-waited
			if werr == nil && derr == nil {
				hm.failf("discard: a result Discard gave up was still delivered to Wait")
			}
			if werr != nil && !errors.Is(werr, ErrUnknownTicket) {
				hm.check(h, op, fmt.Errorf("wait: %w", werr), short)
			}
		}
		// Spent either way.
		if err := h.Discard(tk); err == nil || (!errors.Is(err, ErrUnknownTicket) && !expected(h, err, short, false)) {
			hm.failf("second Discard on a spent ticket: %v (first %v, racing %v, %s)", err, derr, racing, ticketState(h.state, tk))
		}
	}

	if h.state.poisoned.Load() || (h.state.closed.Load() && s.cur.Load() == h) {
		hm.replace(s, h, r)
	}
}

func TestHammer_BoundaryUnderChaos(t *testing.T) {
	dur := hammerDuration(t)
	// Thousands of deliberate backstop warnings would bury the report.
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	defer slog.SetDefault(prevLog)

	settle := func() {
		for i := 0; i < 3; i++ {
			runtime.GC()
			time.Sleep(10 * time.Millisecond)
		}
	}
	settle()
	baseGoroutines := runtime.NumGoroutine()
	baseLive := Stats().LiveBytes

	seed, err := hammerSeed()
	if err != nil {
		t.Fatal(err)
	}
	// Also straight to stderr: t.Logf is buffered without -v, and a crash
	// inside a cgo call (the 2026-09-30 SIGSEGV in gusset_take) kills the
	// process before the test's log is flushed.
	fmt.Fprintf(os.Stderr, "hammer: GUSSET_HAMMER_SEED=%d\n", seed)
	t.Logf("seed %d, duration %v", seed, dur)
	// Every failure path, the hammer's own and the final invariant checks,
	// ends with the line that replays it.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("replay: GUSSET_HAMMER_SEED=%d GUSSET_STRESS=%v", seed, dur)
		}
	})
	hm := &hammer{t: t}
	const slots, lanes = 4, 12
	for i := 0; i < slots; i++ {
		s := &hammerSlot{}
		h, err := hm.open(rand.New(rand.NewPCG(seed, uint64(i))))
		if err != nil {
			t.Fatal(err)
		}
		s.cur.Store(h)
		s.states = append(s.states, h.state)
		hm.slots = append(hm.slots, s)
	}

	var maxThreads atomic.Int64
	var peakLive uint64 // written by the sampler, read after wg.Wait
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for si, s := range hm.slots {
		for l := 0; l < lanes; l++ {
			wg.Add(1)
			go func(r *rand.Rand) {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					hm.step(s, r)
				}
			}(rand.New(rand.NewPCG(seed, uint64(1000+si*lanes+l))))
		}
		// Chaos: close the live handle out from under the lanes.
		wg.Add(1)
		go func(r *rand.Rand) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(time.Duration(5+r.IntN(60)) * time.Millisecond):
				}
				hm.replace(s, s.cur.Load(), r)
			}
		}(rand.New(rand.NewPCG(seed, uint64(9000+si))))
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if n := Threads(); n > maxThreads.Load() {
					maxThreads.Store(n)
				}
				if n := Stats().LiveBytes; n > peakLive {
					peakLive = n
				}
			}
		}
	}()

	time.Sleep(dur)
	close(stop)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		buf := make([]byte, 1<<20)
		t.Fatalf("lanes did not stop within 60 s (a caller is parked forever):\n%s", buf[:runtime.Stack(buf, true)])
	}

	// Close every live handle; dropped ones are closed by the backstop.
	var all []*handleState
	for _, s := range hm.slots {
		if err := s.cur.Load().Close(); err != nil {
			t.Errorf("final Close: %v", err)
		}
		s.cur.Store(nil)
		all = append(all, s.states...)
	}
	// A GC only helps a dropped handle still waiting for its backstop. This
	// used to run one before checking each state, and `all` holds every
	// handle the chaos goroutines opened — tens of thousands over 5 minutes —
	// so the run spent 15+ minutes on back-to-back GOGC=1 collections after
	// the chaos had ended, with no gusset goroutine left, which read as a hang.
	closedYet := func(st *handleState) bool {
		select {
		case <-st.closeDone:
			return true
		default:
			return false
		}
	}
	tailStart := time.Now()
	deadline := tailStart.Add(30 * time.Second)
	gcs := 0
	for _, st := range all {
		for !closedYet(st) {
			if time.Now().After(deadline) {
				t.Fatalf("a dropped handle was never closed by its AddCleanup backstop")
			}
			runtime.GC()
			gcs++
			select {
			case <-st.closeDone:
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	t.Logf("final close: %d handle states closed after %d GCs in %v", len(all), gcs, time.Since(tailStart).Round(time.Millisecond))
	settle()

	for i, st := range all {
		st.mu.Lock()
		if n := len(st.sem); n != 0 {
			t.Errorf("handle %d: %d pool permits still held after Close (I4)", i, n)
		}
		if n := len(st.semTickets) + len(st.pending) + len(st.completed) + len(st.abandoned) + len(st.takeIDs); n != 0 {
			t.Errorf("handle %d: bookkeeping left behind: semTickets %d pending %d completed %d abandoned %d takeIDs %d",
				i, len(st.semTickets), len(st.pending), len(st.completed), len(st.abandoned), len(st.takeIDs))
		}
		st.mu.Unlock()
		if n := st.bufBytes.Load(); n != 0 {
			t.Errorf("handle %d: buffer budget charge %d after every buffer was freed or collected", i, n)
		}
	}

	var goroutines int
	for end := time.Now().Add(10 * time.Second); ; {
		goroutines = runtime.NumGoroutine()
		if goroutines <= baseGoroutines || time.Now().After(end) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if goroutines > baseGoroutines {
		buf := make([]byte, 1<<20)
		t.Errorf("goroutines: %d after, %d before:\n%s", goroutines, baseGoroutines, buf[:runtime.Stack(buf, true)])
	}

	// Rust keeps a bounded log ring and panic-location map; anything beyond
	// that is a leaked buffer, result or handle.
	const slack = 512 << 10
	live := Stats().LiveBytes
	if live > baseLive+slack {
		t.Errorf("Rust live bytes %d after, %d before: %d leaked", live, baseLive, live-baseLive)
	}

	// Each goroutine inside a cgo call pins at most one M; the calls are short
	// and nothing parks on an OS thread, so threads stay near GOMAXPROCS plus
	// the callers that can be in cgo at once.
	bound := int64(runtime.GOMAXPROCS(0) + slots*(lanes+4) + 16)
	if mt := maxThreads.Load(); mt > bound {
		t.Errorf("peak Go threads %d exceeds bound %d (R11)", mt, bound)
	}

	var summary []string
	for i := range hm.counts {
		summary = append(summary, fmt.Sprintf("%s=%d", opNames[i], hm.counts[i].Load()))
	}
	t.Logf("ops: %s", strings.Join(summary, " "))
	t.Logf("handles opened %d, closed concurrently %d, dropped to backstop %d, poisoned by panics %d; verified %d MiB; peak threads %d; live bytes %d -> peak %d -> %d",
		hm.opens.Load(), hm.closes.Load(), hm.dropped.Load(), hm.poisonings.Load(), hm.okBytes.Load()>>20, maxThreads.Load(), baseLive, peakLive, live)
	if n := hm.unexpected.Load(); n != 0 {
		t.Fatalf("%d unexpected outcomes", n)
	}
}

// TestSoak_SingleHandleBookkeepingStaysFlat runs a long mix of Call,
// Submit/Wait, WaitBuffer and abandoned deadlines on ONE handle that is never
// closed, and at every checkpoint — with nothing in flight — requires every
// bookkeeping map to be empty and the heap, Rust live bytes and goroutine
// count to be where the first checkpoint left them.
//
// The hammer checks bookkeeping only after Close, so anything Close sweeps up
// (a completed entry, a nil collecting marker, a take id, an abandoned ticket
// whose completion was never matched) is invisible to it. A handle that lives
// for weeks never gets that sweep: a per-ticket leak there is unbounded growth.
//
// GUSSET_SOAK sets the total operations (default 40000; the audit ran 2000000).
func TestSoak_SingleHandleBookkeepingStaysFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("soak skipped in -short mode")
	}
	total := 40000
	if v := os.Getenv("GUSSET_SOAK"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("GUSSET_SOAK=%q: want a positive operation count", v)
		}
		total = n
	}
	const workers, checkpoints = 8, 8
	perPhase := total / checkpoints / workers

	h, err := Open(WithPoolSize(4), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state

	var ops [5]atomic.Int64 // call, submitWaitLarge, completedPath, waitBuffer, abandon
	var bad, lateResults atomic.Int64
	fail := func(format string, args ...any) {
		if bad.Add(1) <= 10 {
			t.Errorf(format, args...)
		}
	}

	var inputsMu sync.Mutex
	var inputs []*Buffer
	worker := func(r *rand.Rand) {
		for i := 0; i < perPhase; i++ {
			switch k := r.IntN(100); {
			case k < 55: // Call: inline record, or a take copied onto the Go heap
				n := r.IntN(64)
				if r.IntN(8) == 0 {
					n = 64 + r.IntN(inlineResultBytes-64)
				}
				in := make([]byte, n+1)
				for j := 1; j < len(in); j++ {
					in[j] = byte(r.Uint32())
				}
				out, err := h.Call(context.Background(), in)
				if err != nil || !bytes.Equal(out, in) {
					fail("Call(%d bytes): %d bytes, %v", len(in), len(out), err)
				}
				ops[0].Add(1)
			case k < 75: // Submit + Wait of a large result: a take buffer id in takeIDs
				n, seed := 4097+r.IntN(12000), byte(r.Uint32())
				tk, err := h.Submit(context.Background(), mode16(n, seed))
				if err != nil {
					fail("Submit: %v", err)
					continue
				}
				out, err := h.Wait(context.Background(), tk)
				if err != nil || !generated(out, n, seed) {
					fail("Wait(large %d): %d bytes, %v", n, len(out), err)
				}
				ops[1].Add(1)
			case k < 88: // the completed-map path: the result lands before Wait
				in := []byte{0, byte(r.Uint32()), 3}
				if r.IntN(2) == 0 {
					in = mode16(4097+r.IntN(4000), 7)
				}
				tk, err := h.Submit(context.Background(), in)
				if err != nil {
					fail("Submit: %v", err)
					continue
				}
				time.Sleep(50 * time.Microsecond)
				if _, err := h.Wait(context.Background(), tk); err != nil {
					fail("Wait(completed): %v", err)
				}
				if _, err := h.Wait(context.Background(), tk); !errors.Is(err, ErrUnknownTicket) {
					fail("second Wait: %v", err)
				}
				ops[2].Add(1)
			case k < 99: // WaitBuffer of a large result, freed by the caller
				n := 4097 + r.IntN(8000)
				tk, err := h.Submit(context.Background(), mode16(n, 1))
				if err != nil {
					fail("Submit: %v", err)
					continue
				}
				buf, err := h.WaitBuffer(context.Background(), tk)
				if err != nil || !generated(buf.Bytes(), n, 1) {
					fail("WaitBuffer: %v", err)
				}
				if buf != nil {
					_ = buf.Free()
				}
				ops[3].Add(1)
			default: // abandon: a 1 ms deadline on a 20 ms non-cooperative job
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				var err error
				var out, want []byte
				start := time.Now()
				if r.IntN(2) == 0 {
					want = []byte{0, 1, 2, 3}
					out, err = h.Call(ctx, []byte{9, 2, 0, 1, 2, 3})
				} else {
					// A large abandoned result: a take buffer deliver must free.
					// The input stays allocated until the phase ends — the job
					// still reads it after the caller has gone.
					in, aerr := h.NewBuffer(8000)
					if aerr != nil {
						fail("NewBuffer: %v", aerr)
						cancel()
						continue
					}
					b := in.Bytes()
					for j := range b {
						b[j] = byte(j * 5)
					}
					b[0], b[1], b[2] = 9, 2, 0
					want = append([]byte(nil), b[2:]...)
					inputsMu.Lock()
					inputs = append(inputs, in)
					inputsMu.Unlock()
					var tk uint64
					if tk, err = h.Submit(ctx, in); err == nil {
						out, err = h.Wait(ctx, tk)
					}
				}
				cancel()
				// A result can win only when the deadline's timer fired late
				// (the job takes 20 ms); then it must be the right result.
				if err == nil {
					lateResults.Add(1)
					if el := time.Since(start); el < 20*time.Millisecond || !bytes.Equal(out, want) {
						fail("abandon: nil error after %v with %d bytes (want %d, equal %v)", el, len(out), len(want), bytes.Equal(out, want))
					}
				}
				ops[4].Add(1)
			}
		}
	}

	type sample struct {
		heap, live      uint64
		goroutines, ops int
	}
	var samples []sample
	quiesce := func() {
		deadline := time.Now().Add(10 * time.Second)
		for {
			s.mu.Lock()
			n := len(s.semTickets) + len(s.pending) + len(s.completed) + len(s.abandoned) + len(s.takeIDs)
			m := fmt.Sprintf("sem %d semTickets %d pending %d completed %d abandoned %d takeIDs %d",
				len(s.sem), len(s.semTickets), len(s.pending), len(s.completed), len(s.abandoned), len(s.takeIDs))
			s.mu.Unlock()
			if n == 0 && len(s.sem) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("bookkeeping left behind on an open, idle handle: %s", m)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	measure := func(done int) {
		quiesce()
		for _, in := range inputs {
			_ = in.Free()
		}
		inputs = nil
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		samples = append(samples, sample{ms.HeapInuse, Stats().LiveBytes, runtime.NumGoroutine(), done})
	}

	seed := uint64(time.Now().UnixNano())
	t.Logf("seed %d, %d ops over %d workers", seed, perPhase*workers*checkpoints, workers)
	measure(0)
	for c := 0; c < checkpoints; c++ {
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(r *rand.Rand) { defer wg.Done(); worker(r) }(rand.New(rand.NewPCG(seed, uint64(c*workers+w))))
		}
		wg.Wait()
		measure((c + 1) * perPhase * workers)
	}

	for _, sm := range samples {
		t.Logf("after %8d ops: HeapInuse %6d KiB, Rust live %6d KiB, goroutines %d", sm.ops, sm.heap>>10, sm.live>>10, sm.goroutines)
	}
	t.Logf("call %d submit+wait(large) %d completed-path %d waitbuffer %d abandon-attempts %d (%d got the result: the 1 ms timer fired after the 20 ms job)",
		ops[0].Load(), ops[1].Load(), ops[2].Load(), ops[3].Load(), ops[4].Load(), lateResults.Load())

	// Compare against the first loaded checkpoint: it has the warmed maps,
	// channels and reader buffers every later one reuses.
	base := samples[1]
	const heapSlack, liveSlack = 2 << 20, 512 << 10
	for _, sm := range samples[2:] {
		if sm.heap > base.heap+heapSlack {
			t.Errorf("Go heap grew: %d KiB after %d ops, %d KiB after %d", sm.heap>>10, sm.ops, base.heap>>10, base.ops)
		}
		if sm.live > base.live+liveSlack {
			t.Errorf("Rust live bytes grew: %d after %d ops, %d after %d", sm.live, sm.ops, base.live, base.ops)
		}
		if sm.goroutines > base.goroutines {
			t.Errorf("goroutines grew: %d after %d ops, %d after %d", sm.goroutines, sm.ops, base.goroutines, base.ops)
		}
	}
}

// mode16 asks the diagnostic engine for n generated bytes, byte i = i ^ seed.
func mode16(n int, seed byte) []byte {
	in := make([]byte, 6)
	in[0] = 16
	binary.LittleEndian.PutUint32(in[1:5], uint32(n))
	in[5] = seed
	return in
}

func generated(b []byte, n int, seed byte) bool {
	if len(b) != n {
		return false
	}
	for i, v := range b {
		if v != byte(i)^seed {
			return false
		}
	}
	return true
}

// churnSample is the process-level footprint a handle can leak into.
type churnSample struct {
	goroutines int
	goThreads  int64  // /sched/threads/total:threads (Go's Ms)
	osTasks    int    // countOSThreads: Go's threads and Rust's workers; -1 unmeasured
	fds        int    // /proc/self/fd or /dev/fd; -1 unmeasured
	heapLive   uint64 // /memory/classes/heap/objects:bytes after GC
	rustLive   uint64 // Stats().LiveBytes
}

func countDir(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return -1
	}
	return len(ents)
}

// countOSThreads counts the process's OS threads: /proc/self/task on Linux,
// one `ps -M` line per thread on darwin, which has no /proc. Reading only
// /proc made every thread comparison -1 against -1 on macOS, a pass that
// measured nothing.
func countOSThreads(t *testing.T) int {
	t.Helper()
	if n := countDir(t, "/proc/self/task"); n >= 0 {
		return n
	}
	if runtime.GOOS != "darwin" {
		return -1
	}
	out, err := exec.Command("ps", "-M", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return -1
	}
	return bytes.Count(out, []byte("\n")) - 1 // minus the header
}

func countFDs(t *testing.T) int {
	t.Helper()
	if n := countDir(t, "/proc/self/fd"); n >= 0 {
		return n
	}
	return countDir(t, "/dev/fd")
}

func takeChurnSample(t *testing.T, gc bool) churnSample {
	t.Helper()
	if gc {
		runtime.GC()
		runtime.GC()
	}
	m := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(m)
	return churnSample{
		goroutines: runtime.NumGoroutine(),
		goThreads:  Threads(),
		osTasks:    countOSThreads(t),
		fds:        countFDs(t),
		heapLive:   m[0].Value.Uint64(),
		rustLive:   Stats().LiveBytes,
	}
}

// rustThreads is the OS threads that are not Go Ms: Rust's workers.
func (a churnSample) rustThreads() int64 { return int64(a.osTasks) - a.goThreads }

func (a churnSample) max(b churnSample) churnSample {
	a.goroutines = max(a.goroutines, b.goroutines)
	a.goThreads = max(a.goThreads, b.goThreads)
	a.osTasks = max(a.osTasks, b.osTasks)
	a.fds = max(a.fds, b.fds)
	a.heapLive = max(a.heapLive, b.heapLive)
	a.rustLive = max(a.rustLive, b.rustLive)
	return a
}

// churnOne opens a handle, makes a few calls of each egress shape, and either
// closes it or drops it for the AddCleanup backstop. It returns the state of a
// dropped handle (nil when closed), which does not keep the Handle reachable:
// the cleanup's argument is the state.
func churnOne(t *testing.T, i int, drop bool) *handleState {
	h, err := Open(WithPoolSize(1+i%2), WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open #%d: %v", i, err)
	}
	ctx := context.Background()
	small := []byte{0, byte(i), byte(i >> 8)} // inline completion record
	if out, err := h.Call(ctx, small); err != nil || !bytes.Equal(out, small) {
		t.Fatalf("Call #%d: %v", i, err)
	}
	mid := bytes.Repeat([]byte{byte(i)}, 300) // bare ticket, take copy
	mid[0] = 0
	if out, err := h.Call(ctx, mid); err != nil || !bytes.Equal(out, mid) {
		t.Fatalf("Call(300) #%d: %v", i, err)
	}
	in, err := h.NewBuffer(8 << 10) // take-buffer egress, zero copy
	if err != nil {
		t.Fatalf("NewBuffer #%d: %v", i, err)
	}
	in.Bytes()[0] = 0
	out, err := h.CallBuffer(ctx, in)
	if err != nil {
		t.Fatalf("CallBuffer #%d: %v", i, err)
	}
	if i%3 == 0 {
		// Leave one result buffer to its own backstop as well.
		_ = in.Free()
		out.owner = nil
	} else {
		_ = out.Free()
		_ = in.Free()
	}
	if drop {
		s := h.state
		return s
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close #%d: %v", i, err)
	}
	return nil
}

// Open and close handles in a loop — some dropped to the AddCleanup backstop
// instead of closed — and require goroutines, OS threads, descriptors, the Go
// heap and Rust live bytes to come back to a warm baseline.
//
// Every handle is a pipe, a drain goroutine and a pool of OS threads; a
// per-handle leak of any of them is invisible to the single-handle soak.
// GUSSET_CHURN sets the handle count (default 2000; the audit ran 20000).
func TestChurn_HandlesLeaveNothingBehind(t *testing.T) {
	if testing.Short() {
		t.Skip("churn skipped in -short mode")
	}
	total := 2000
	if v := os.Getenv("GUSSET_CHURN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("GUSSET_CHURN=%q: want a positive handle count", v)
		}
		total = n
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)

	// Warm up: Go keeps idle Ms, so a cold baseline would read their creation
	// as a leak.
	for i := 0; i < 200; i++ {
		churnOne(t, i, false)
	}
	base := takeChurnSample(t, true)
	peak := base

	// Dropped handles are collected in batches: one GC finds the whole batch
	// unreachable at once. GUSSET_CHURN_BATCH sets its size (default 64).
	churnBatch := 64
	if v := os.Getenv("GUSSET_CHURN_BATCH"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("GUSSET_CHURN_BATCH=%q: want a positive count", v)
		}
		churnBatch = n
	}
	var dropped []*handleState
	drops := 0
	settle := func() {
		runtime.GC()
		runtime.GC()
		for _, s := range dropped {
			select {
			case <-s.closeDone:
			case <-time.After(30 * time.Second):
				t.Fatal("a dropped handle was not closed by its AddCleanup backstop within 30 s")
			}
		}
		dropped = dropped[:0]
	}
	start := time.Now()
	for i := 0; i < total; i++ {
		drop := i%8 == 7
		if s := churnOne(t, 200+i, drop); s != nil {
			dropped = append(dropped, s)
			drops++
		}
		if len(dropped) >= churnBatch {
			settle()
		}
		if i%250 == 0 {
			peak = peak.max(takeChurnSample(t, false))
		}
	}
	settle()
	elapsed := time.Since(start)

	t.Logf("%d handles (%d dropped to the backstop) in %v", total, drops, elapsed.Round(time.Millisecond))
	// Go's own Ms are not a per-handle leak but a high-water mark: every
	// backstop close is its own goroutine in a cgo call joining workers, one
	// M each, and Go never retires an idle M. A batch of dropped handles
	// collected by one GC therefore raises the count by up to the batch size
	// (measured over 20000 handles: batch 1 -> +1, 8 -> +7, 64 -> +36,
	// 256 -> +48), and it must not grow with the number of handles.
	requireChurnBaseline(t, base, peak, int64(churnBatch)+8)
}

// requireChurnBaseline waits up to 30 s for the process to come back to base,
// then fails on every resource still above it. goThreadSlack bounds Go's own
// Ms, which are a high-water mark rather than a per-handle leak. A resource
// the platform cannot count skips the test after the other checks have run,
// rather than passing without having measured it.
func requireChurnBaseline(t *testing.T, base, peak churnSample, goThreadSlack int64) {
	t.Helper()
	// Converge: cleanups for dropped result buffers and the last drain
	// goroutines finish asynchronously.
	var after churnSample
	deadline := time.Now().Add(30 * time.Second)
	for {
		after = takeChurnSample(t, true)
		ok := after.goroutines <= base.goroutines && after.fds <= base.fds &&
			after.rustLive <= base.rustLive && after.rustThreads() <= base.rustThreads()
		if ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("baseline:   %+v", base)
	t.Logf("peak:       %+v", peak)
	t.Logf("after:      %+v", after)

	if after.goroutines > base.goroutines {
		t.Errorf("goroutines %d -> %d", base.goroutines, after.goroutines)
	}
	if after.fds > base.fds {
		t.Errorf("open descriptors %d -> %d", base.fds, after.fds)
	}
	if after.rustLive > base.rustLive {
		t.Errorf("Rust live bytes %d -> %d", base.rustLive, after.rustLive)
	}
	// Threads that are not Go's are Rust workers, and Close joins every one.
	if after.rustThreads() > base.rustThreads() {
		t.Errorf("non-Go OS threads (Rust workers) %d -> %d", base.rustThreads(), after.rustThreads())
	}
	if after.goThreads > base.goThreads+goThreadSlack {
		t.Errorf("Go threads %d -> %d, past the bound +%d", base.goThreads, after.goThreads, goThreadSlack)
	}
	if after.heapLive > base.heapLive+4<<20 {
		t.Errorf("Go heap %d -> %d bytes", base.heapLive, after.heapLive)
	}
	if base.osTasks < 0 || after.osTasks < 0 || base.fds < 0 || after.fds < 0 {
		t.Skipf("OS threads or descriptors cannot be counted on %s; those checks did not run", runtime.GOOS)
	}
}

// poisonOne runs the documented recovery path once (DECISIONS.md, 2026-09-30:
// poison is per handle; recovery is Close and Open): a fresh handle answers, a
// slow job is in flight when a sibling's panic poisons the handle, new work is
// refused, and Close drains the in-flight job — collected by Wait on even
// cycles, left for Close on odd ones. The panicking worker, the in-flight
// result (a Rust take buffer when large) and the handle's pipe and drain
// goroutine are what a recovery that leaks would leave behind.
func poisonOne(t *testing.T, i int) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open #%d: %v", i, err)
	}
	ctx := context.Background()
	if out, err := h.Call(ctx, []byte{0, byte(i)}); err != nil || !bytes.Equal(out, []byte{0, byte(i)}) {
		t.Fatalf("Call on a fresh handle #%d: %v", i, err)
	}
	// Mode 9: sleep 20 ms, then echo slow[2:]. Over 4 KiB on alternate pairs
	// of cycles, so both the inline and the take-buffer result are in flight.
	n := 300
	if i%4 >= 2 {
		n = 5000
	}
	slow := bytes.Repeat([]byte{byte(i)}, n)
	slow[0], slow[1], slow[2] = 9, 2, 0
	var in any = slow
	if n > 4096 {
		// A []byte input is capped at 4 KiB; the large one goes as a Buffer.
		buf, err := h.NewBuffer(n)
		if err != nil {
			t.Fatalf("NewBuffer #%d: %v", i, err)
		}
		copy(buf.Bytes(), slow)
		defer func() { _ = buf.Free() }()
		in = buf
	}
	inflight, err := h.Submit(ctx, in)
	if err != nil {
		t.Fatalf("Submit #%d: %v", i, err)
	}
	if _, err := h.Call(ctx, []byte{1}); !errors.Is(err, ErrPanic) {
		t.Fatalf("mode 1 #%d: want ErrPanic, got %v", i, err)
	}
	if _, err := h.Call(ctx, []byte{0}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("Call after the panic #%d: want ErrPoisoned, got %v", i, err)
	}
	if _, err := h.NewBuffer(64); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("NewBuffer after the panic #%d: want ErrPoisoned, got %v", i, err)
	}
	if i%2 == 0 {
		// Poison refuses new work, not finished work (DECISIONS.md, 2026-09-22).
		out, err := h.Wait(ctx, inflight)
		if err != nil || !bytes.Equal(out, slow[2:]) {
			t.Fatalf("Wait on the in-flight job #%d: %d bytes, %v", i, len(out), err)
		}
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close of the poisoned handle #%d: %v", i, err)
	}
}

// A poisoned handle closed and replaced by a fresh one, over and over, must
// bring goroutines, OS threads, descriptors, the Go heap and Rust live bytes
// back to a warm baseline: recovery from ErrPoisoned leaks nothing. The engine
// here is the diagnostic one, which needs no registration; an adopter whose
// panic tore its engine's captured state also runs clear_engine_handlers and
// registers again in Rust before the Open, which no Go entry point does.
// GUSSET_POISON_CHURN sets the cycle count (default 200).
func TestChurn_PoisonedHandlesRecreateLeavingNothingBehind(t *testing.T) {
	if testing.Short() {
		t.Skip("poison churn skipped in -short mode")
	}
	total := 200
	if v := os.Getenv("GUSSET_POISON_CHURN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("GUSSET_POISON_CHURN=%q: want a positive cycle count", v)
		}
		total = n
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)

	// Warm up: Go keeps idle Ms, so a cold baseline would read their creation
	// as a leak.
	for i := 0; i < 20; i++ {
		poisonOne(t, i)
	}
	base := takeChurnSample(t, true)
	peak := base
	start := time.Now()
	for i := 0; i < total; i++ {
		poisonOne(t, 20+i)
		if i%25 == 0 {
			peak = peak.max(takeChurnSample(t, false))
		}
	}
	t.Logf("%d poisoned handles closed and reopened in %v", total, time.Since(start).Round(time.Millisecond))

	// Recovery closes on the caller's goroutine, so there is no backstop
	// burst: Go's Ms may only grow by the runtime's own slack.
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open after recovery: %v", err)
	}
	if out, err := h.Call(context.Background(), []byte{0, 7}); err != nil || !bytes.Equal(out, []byte{0, 7}) {
		t.Fatalf("Call on the recovered handle: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close of the recovered handle: %v", err)
	}
	requireChurnBaseline(t, base, peak, 8)
}
