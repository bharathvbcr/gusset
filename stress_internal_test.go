package gusset

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"runtime"
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
		body := payload(r, r.IntN(6000))
		if len(body) > inlineResultBytes-3 {
			body = body[:inlineResultBytes-3]
		}
		in := append([]byte{9, byte(1 + r.IntN(4))}, body...)
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

	hm := &hammer{t: t}
	const slots, lanes = 4, 12
	seed := uint64(time.Now().UnixNano())
	t.Logf("seed %d, duration %v", seed, dur)
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
