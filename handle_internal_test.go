package gusset

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"runtime"
	"runtime/metrics"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset/internal/ffi"
	"github.com/bharathvbcr/gusset/internal/hostprobe"
)

func inlineRecord(ticket uint64, data []byte) []byte {
	rec := binary.NativeEndian.AppendUint64(nil, ticket|ffi.InlineRecordFlag)
	rec = binary.NativeEndian.AppendUint64(rec, uint64(len(data)))
	rec = append(rec, data...)
	for len(rec)%8 != 0 {
		rec = append(rec, 0)
	}
	return rec
}

// The reader must reassemble records a read cut anywhere, including inside
// the header words and inside the padded payload, and keep bare tickets and
// inline records apart.
func TestTicketReaderParsesMixedRecordsAcrossShortReads(t *testing.T) {
	type want struct {
		ticket uint64
		data   []byte
		inline bool
	}
	var stream []byte
	var wants []want
	for i := 0; i < 200; i++ {
		ticket := uint64(i + 1)
		if i%3 == 0 {
			stream = binary.NativeEndian.AppendUint64(stream, ticket)
			wants = append(wants, want{ticket: ticket})
			continue
		}
		data := bytes.Repeat([]byte{byte(i)}, i%(ffi.InlineResultMax+1))
		stream = append(stream, inlineRecord(ticket, data)...)
		wants = append(wants, want{ticket, data, true})
	}

	for _, chunk := range []int{1, 3, 7, 13, 64, 509, len(stream)} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		go func(stream []byte) {
			for len(stream) > 0 {
				n := min(chunk, len(stream))
				if _, err := w.Write(stream[:n]); err != nil {
					break
				}
				stream = stream[n:]
			}
			_ = w.Close()
		}(stream)
		tr := newTicketReader(r)
		for i, wt := range wants {
			ticket, data, inline, err := tr.next()
			if err != nil {
				t.Fatalf("chunk %d record %d: %v", chunk, i, err)
			}
			if ticket != wt.ticket || inline != wt.inline || !bytes.Equal(data, wt.data) {
				t.Fatalf("chunk %d record %d: got (%d, %q, %v), want (%d, %q, %v)",
					chunk, i, ticket, data, inline, wt.ticket, wt.data, wt.inline)
			}
		}
		if _, _, _, err := tr.next(); err == nil {
			t.Fatalf("chunk %d: expected EOF after the last record", chunk)
		}
		_ = r.Close()
	}
}

// A length above the inline maximum is not a record Rust can write; the
// reader refuses it instead of reading past it.
func TestTicketReaderRejectsOversizedInlineLength(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rec := binary.NativeEndian.AppendUint64(nil, 1|ffi.InlineRecordFlag)
	rec = binary.NativeEndian.AppendUint64(rec, uint64(ffi.InlineResultMax+1))
	if _, err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if _, _, _, err := newTicketReader(r).next(); err == nil {
		t.Fatal("oversized inline length was accepted")
	}
}

func goThreadCount(t *testing.T) int64 {
	t.Helper()
	sample := []metrics.Sample{{Name: "/sched/threads/total:threads"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindUint64 {
		t.Skip("/sched/threads/total:threads is not available in this Go")
	}
	return int64(sample[0].Value.Uint64())
}

// A burst of handles dropped to the GC backstop must not leave a thread per
// handle behind. Each close below blocks in cgo for ~300 ms joining a worker
// that runs a non-cooperative job, and Go never returns the Ms such calls
// used; closing them all at once grew the process by one thread per handle
// (46 of 48 measured). Meaningful once per process: Ms retained by an earlier
// run are already in a later run's baseline, which is the leak itself.
func TestBackstop_ABurstOfDroppedHandlesRetainsBoundedThreads(t *testing.T) {
	if testing.Short() {
		t.Skip("backstop burst skipped in -short mode")
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)

	const burst = 48
	// Warm up the Ms an ordinary open/call/close uses, so the baseline holds
	// them already.
	for i := 0; i < 8; i++ {
		h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Call(context.Background(), []byte{0, 1}); err != nil {
			t.Fatal(err)
		}
		_ = h.Close()
	}
	runtime.GC()
	before := goThreadCount(t)

	states := make([]*handleState, 0, burst)
	func() {
		for i := 0; i < burst; i++ {
			h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
			if err != nil {
				t.Fatal(err)
			}
			// Mode 9: sleep 300 ms without checking for cancellation, so the
			// close has a worker to wait for.
			if _, err := h.Submit(context.Background(), []byte{9, 30}); err != nil {
				t.Fatal(err)
			}
			states = append(states, h.state)
		}
	}()
	runtime.GC()
	runtime.GC()
	for _, s := range states {
		select {
		case <-s.closeDone:
		case <-time.After(60 * time.Second):
			t.Fatal("a dropped handle was not closed by its backstop within 60 s")
		}
	}
	grown := goThreadCount(t) - before
	t.Logf("%d dropped handles grew Go's threads by %d", burst, grown)
	// cap(backstopClosers) closes in cgo at once, plus slack for the
	// runtime's own Ms (GC workers, sysmon, the cleanup goroutine).
	if limit := int64(cap(backstopClosers)) + 12; grown > limit {
		t.Fatalf("%d dropped handles grew Go's threads by %d, over %d", burst, grown, limit)
	}
}

// discardTestWait polls cond under s.mu until it holds or 10s pass.
func discardTestWait(t *testing.T, s *handleState, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		ok := cond()
		s.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Discard of a stored result empties the bookkeeping that holds it: the
// completed entry and, for a result over 4 KiB, the take buffer id whose Rust
// memory the GC cannot see.
func TestDiscard_StoredResultLeavesNothingBehind(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	buf, err := h.NewBuffer(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	buf.Bytes()[0] = 0 // echo
	ticket, err := h.Submit(ctx, buf)
	if err != nil {
		t.Fatal(err)
	}
	_ = buf.Free()

	discardTestWait(t, s, "the result to be stored", func() bool {
		_, done := s.completed[ticket]
		return done
	})
	s.mu.Lock()
	if s.takeIDs[ticket] == 0 {
		s.mu.Unlock()
		t.Fatal("a 64 KiB result should be held as a take buffer; the test measures nothing")
	}
	s.mu.Unlock()

	if err := h.Discard(ticket); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.completed[ticket]; ok {
		t.Error("Discard left the result in completed")
	}
	if _, ok := s.takeIDs[ticket]; ok {
		t.Error("Discard left the take buffer id behind")
	}
	if _, ok := s.semTickets[ticket]; ok {
		t.Error("Discard left the ticket holding a permit")
	}
	if len(s.sem) != 0 {
		t.Errorf("%d permits still held after the only ticket was discarded", len(s.sem))
	}
}

// Discard on a closing handle answers ErrClosed, even for a result stored in
// completed. drainPipe keeps storing completions while Close joins the
// workers, so claiming one there made a ticket answer ErrClosed and then nil
// once its completion landed (found by the hammer's discard op).
func TestDiscard_ClosingHandleIsErrClosedEvenWithAStoredResult(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	s := h.state
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ticket, err := h.Submit(ctx, []byte{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	discardTestWait(t, s, "the result to be stored", func() bool {
		_, done := s.completed[ticket]
		return done
	})

	// The window inside Close: closed is set, the stored result not yet freed.
	s.closed.Store(true)
	err = h.Discard(ticket)
	s.closed.Store(false)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Discard while closing: got %v, want ErrClosed", err)
	}
	s.mu.Lock()
	_, kept := s.completed[ticket]
	s.mu.Unlock()
	if !kept {
		t.Fatal("Discard while closing claimed the result instead of leaving it to Close")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

// Discard of a running ticket leaves its permit with the work (I4): the
// worker is still busy, and returning the permit would let another job run
// beside it. deliver frees the result and returns the permit when the work
// stops, as it does for a ticket abandoned at its deadline.
func TestDiscard_RunningTicketKeepsItsPermitUntilTheWorkStops(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Mode 9: 1.5 s without checking for cancellation. It was 300 ms against a
	// 50 ms probe, so a host that stalled this goroutine for 250 ms let the job
	// finish and the probe succeed, which reads as an I4 breach and is not one.
	const jobRuns = 1500 * time.Millisecond
	submitted := time.Now()
	ticket, err := h.Submit(ctx, []byte{9, byte(jobRuns / (10 * time.Millisecond))})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Discard(ticket); err != nil {
		t.Fatalf("Discard of a running ticket: %v", err)
	}
	s.mu.Lock()
	_, held := s.semTickets[ticket]
	_, abandoned := s.abandoned[ticket]
	s.mu.Unlock()
	if !held || !abandoned {
		t.Fatalf("running ticket after Discard: permit held %v, abandoned %v; want both", held, abandoned)
	}

	short, shortCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	probeTicket, err := h.Submit(short, []byte{0})
	shortCancel()
	if err == nil {
		// Admitted. Before the job could have ended that is a breach whatever
		// the load; after, the host stalled the probe past the job and it
		// proves nothing.
		if since := time.Since(submitted); since < jobRuns {
			t.Fatalf("a second Submit was admitted %v after a %v job was submitted, while its worker was still busy (I4)", since, jobRuns)
		}
		_ = h.Discard(probeTicket)
		hostprobe.Skip(t, "the probe ran past the job's end: it proves nothing on this host")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second Submit on a 1-worker pool whose worker is still busy: got %v, want DeadlineExceeded (I4)", err)
	}

	discardTestWait(t, s, "the discarded job to finish and return its permit", func() bool {
		_, held := s.semTickets[ticket]
		_, abandoned := s.abandoned[ticket]
		_, stored := s.completed[ticket]
		return !held && !abandoned && !stored
	})
	if _, err := h.Call(ctx, []byte{0}); err != nil {
		t.Fatalf("Call once the discarded job finished: %v", err)
	}
}

// A ticket with a waiter belongs to that waiter: Discard refuses it rather
// than pulling the result out from under a parked Wait.
func TestDiscard_RefusesATicketWithAWaiter(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ticket, err := h.Submit(ctx, []byte{9, 20}) // 200 ms, non-cooperative
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() {
		_, err := h.Wait(ctx, ticket)
		waited <- err
	}()
	discardTestWait(t, s, "the waiter to register", func() bool {
		_, ok := s.pending[ticket]
		return ok
	})

	if err := h.Discard(ticket); !errors.Is(err, ErrTicketBusy) {
		t.Fatalf("Discard of a ticket with a waiter: got %v, want ErrTicketBusy", err)
	}
	if err := <-waited; err != nil {
		t.Fatalf("the waiter lost its result to a refused Discard: %v", err)
	}
}

// A closed handle's errors were distinct errors.New values with two different
// texts ("gusset: handle is closed" at the entry points, "gusset: handle
// closed" for a waiter released by the completion reader), so the only way to
// recognise one was to match strings. Every closed-handle error must now
// satisfy errors.Is(err, ErrClosed), and keep the text it always had, so the
// substring matchers adopters already wrote keep working.

// wantClosed fails unless err is ErrClosed and still carries legacyText.
func wantClosed(t *testing.T, what string, err error, legacyText string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: nil error, want ErrClosed", what)
		return
	}
	if !errors.Is(err, ErrClosed) {
		t.Errorf("%s: %q (%T) does not match errors.Is(err, ErrClosed)", what, err, err)
	}
	if !strings.Contains(err.Error(), legacyText) {
		t.Errorf("%s: %q lost the historical text %q", what, err, legacyText)
	}
}

// Every entry point that returns an error, called after Close.
func TestErrClosed_EveryEntryPointAfterClose(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// A ticket still outstanding at Close, never waited on before it.
	outstanding, err := h.Submit(ctx, []byte{0, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	buf, err := h.NewBuffer(8 << 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	const is = "gusset: handle is closed"
	_, err = h.Submit(ctx, []byte{0})
	wantClosed(t, "Submit([]byte)", err, is)
	_, err = h.Submit(ctx, buf)
	wantClosed(t, "Submit(*Buffer)", err, is)
	_, err = h.Submit(ctx, nil)
	wantClosed(t, "Submit(nil)", err, is)
	_, err = h.Call(ctx, []byte{0})
	wantClosed(t, "Call", err, is)
	_, err = h.CallBuffer(ctx, buf)
	wantClosed(t, "CallBuffer", err, is)
	_, err = h.Wait(ctx, outstanding)
	wantClosed(t, "Wait(outstanding ticket)", err, is)
	_, err = h.WaitBuffer(ctx, outstanding)
	wantClosed(t, "WaitBuffer(outstanding ticket)", err, is)
	_, err = h.Wait(ctx, 1<<62)
	wantClosed(t, "Wait(unknown ticket)", err, is)
	_, err = h.NewBuffer(64)
	wantClosed(t, "NewBuffer", err, is)
	wantClosed(t, "Discard(outstanding ticket)", h.Discard(outstanding), is)
	wantClosed(t, "Discard(unknown ticket)", h.Discard(1<<62), is)

	// Buffer operations after Close are contract, not errors.
	if b := buf.Bytes(); b != nil {
		t.Errorf("Bytes after Close returned %d bytes, want nil", len(b))
	}
	if err := buf.Free(); err != nil {
		t.Errorf("Free after Close: %v, want nil", err)
	}
	// A second Close reports the first Close's outcome, not ErrClosed.
	if err := h.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// A waiter parked when the completion reader gives up is released with
// "gusset: handle closed" (drainPipe's exit), and a submitter parked on the
// full pool at that moment is refused. Both are ErrClosed.
func TestErrClosed_WaiterAndSubmitterReleasedByDrainExit(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state
	ctx := context.Background()

	busy, err := h.Submit(ctx, []byte{9, 30}) // holds the only permit for 300 ms
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { _, err := h.Wait(ctx, busy); waited <- err }()
	eventually(t, "the waiter to register", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.pending[busy] != nil
	})
	parked := make(chan error, 1)
	go func() { _, err := h.Submit(ctx, []byte{0}); parked <- err }()
	time.Sleep(20 * time.Millisecond) // let it park on the full pool

	killDrain(t, s)

	select {
	case err := <-waited:
		wantClosed(t, "Wait released by drainPipe exit", err, "gusset: handle closed")
	case <-time.After(5 * time.Second):
		t.Fatal("waiter not released by drainPipe exit")
	}
	select {
	case err := <-parked:
		wantClosed(t, "Submit parked across drainPipe exit", err, "gusset: handle is closed")
	case <-time.After(5 * time.Second):
		t.Fatal("parked submitter not released by drainPipe exit")
	}
	// And every later call, with the handle not yet closed.
	_, err = h.Call(ctx, []byte{0})
	wantClosed(t, "Call after drainPipe exit", err, "gusset: handle is closed")
	_, err = h.Wait(ctx, busy)
	wantClosed(t, "Wait after drainPipe exit", err, "gusset: handle is closed")
}

// bigInput is an echo (mode 0) input too long for an inline completion
// record, so its completion is a bare ticket collected with gusset_take.
func bigInput(n int) []byte {
	in := bytes.Repeat([]byte{7}, n)
	in[0] = 0
	return in
}

// A bare-ticket completion read while the handle is closing is answered
// "handle closed" by drainPipe itself (it cannot enter cgo to take it).
//
// White-box: closed is set by hand, the state close holds while it joins the
// workers, so the completion is read in exactly that window.
func TestErrClosed_CompletionReadDuringClose(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	s := h.state
	ctx := context.Background()

	in := append([]byte{9, 5, 9}, bytes.Repeat([]byte{1}, 200)...) // 50 ms, then echo 203 bytes
	tk, err := h.Submit(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { _, err := h.Wait(ctx, tk); waited <- err }()
	eventually(t, "the waiter to register", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.pending[tk] != nil
	})
	s.closed.Store(true)
	select {
	case err = <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter not released")
	}
	s.closed.Store(false) // let the real Close run
	wantClosed(t, "Wait whose completion was read during close", err, "gusset: handle closed")
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

// A large result already completed, collected after close has begun: Wait
// cannot copy it out and WaitBuffer cannot wrap it.
func TestErrClosed_CompletedTakeResultAfterCloseBegins(t *testing.T) {
	for _, viaBuffer := range []bool{false, true} {
		name := "Wait"
		if viaBuffer {
			name = "WaitBuffer"
		}
		t.Run(name, func(t *testing.T) {
			h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
			if err != nil {
				t.Fatal(err)
			}
			s := h.state
			ctx := context.Background()
			in, err := h.NewBuffer(16 << 10)
			if err != nil {
				t.Fatal(err)
			}
			copy(in.Bytes(), bigInput(16<<10))
			tk, err := h.Submit(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			eventually(t, "the result to complete", func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				_, ok := s.completed[tk]
				return ok && s.takeIDs[tk] != 0
			})
			s.closed.Store(true)
			if viaBuffer {
				_, err = h.WaitBuffer(ctx, tk)
			} else {
				_, err = h.Wait(ctx, tk)
			}
			s.closed.Store(false)
			wantClosed(t, name+" of a take result after close began", err, "gusset: handle is closed")
			_ = in.Free()
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Submit's buffer refusal is ErrClosed only when the handle being called is
// the one closing (a race with Close past the handle check, which the hammer
// drives). A freed buffer, or one belonging to another handle that is closed,
// leaves the called handle open: that must not read as ErrClosed.
func TestErrClosed_BufferRefusalsOfAnOpenHandleAreNot(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()

	freed, err := h.NewBuffer(64)
	if err != nil {
		t.Fatal(err)
	}
	_ = freed.Free()
	_, err = h.Submit(ctx, freed)
	if err == nil || errors.Is(err, ErrClosed) {
		t.Errorf("Submit of a freed buffer on an open handle: %v, want a non-ErrClosed refusal", err)
	}

	other, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := other.NewBuffer(64)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = h.Submit(ctx, foreign)
	if err == nil || errors.Is(err, ErrClosed) {
		t.Errorf("Submit of another closed handle's buffer on an open handle: %v, want a non-ErrClosed refusal", err)
	}
	if errBufferClosed.Error() != "gusset: buffer is freed or closed" || !errors.Is(errBufferClosed, ErrClosed) {
		t.Errorf("errBufferClosed = %q, want the historical text and ErrClosed", errBufferClosed)
	}
}

// R10 is enforced in two layers: Rust refuses a poisoned handle at the export,
// and Go latches handleState.poisoned and refuses before it enters Rust. The
// Rust layer answers ErrPoisoned (matched by code) for everything, so a test
// that only checks errors.Is(err, ErrPoisoned) passes with the Go layer gone.
// These read the Go latch directly, and tell the layers apart by their text:
// Go's is errHandlePoisoned ("...a job panicked; Close it and open a new
// handle"), Rust's is the bare "handle is poisoned".

const goPoisonText = "a job panicked; Close it and open a new handle"

// The Go latch is set by the reader goroutine off the take error of a panicked
// job, before the caller sees ErrPanic. Nothing else sets it on this path (the
// submit path latches only when Rust itself refused a submit).
func TestPoison_GoLatchIsSetByThePanickedJobsCompletion(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if h.state.poisoned.Load() {
		t.Fatal("a fresh handle is poisoned")
	}
	if _, err := h.Call(ctx, []byte{1}); !errors.Is(err, ErrPanic) {
		t.Fatalf("mode 1: want ErrPanic, got %v", err)
	}
	if !h.state.poisoned.Load() {
		t.Fatal("ErrPanic was delivered but the Go poison latch is clear (R10/I2)")
	}

	// The next submit is answered by Go itself, never reaching Rust.
	_, err = h.Submit(ctx, []byte{0})
	if !errors.Is(err, ErrPoisoned) {
		t.Fatalf("want ErrPoisoned, got %v", err)
	}
	if !strings.Contains(err.Error(), goPoisonText) {
		t.Fatalf("the refusal came from Rust, not the Go layer: %v", err)
	}
}

// With the latch set and Rust's own state untouched (the handle is healthy
// there), Go must still refuse on its own: this is the Go layer with the Rust
// check out of the picture.
func TestPoison_GoLayerRefusesWithoutEnteringRust(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h.state.poisoned.Store(true)
	for name, call := range map[string]func() error{
		"Submit": func() error { _, err := h.Submit(ctx, []byte{0}); return err },
		"Call":   func() error { _, err := h.Call(ctx, []byte{0}); return err },
	} {
		err := call()
		if !errors.Is(err, ErrPoisoned) || !strings.Contains(err.Error(), goPoisonText) {
			t.Fatalf("%s on a Go-latched handle: want the Go ErrPoisoned text, got %v", name, err)
		}
	}
	if n := len(h.state.sem); n != 0 {
		t.Fatalf("a refused submit leaked %d pool permit(s)", n)
	}

	// Rust never saw the refused jobs, so a healthy handle with the latch
	// cleared works again.
	h.state.poisoned.Store(false)
	out, err := h.Call(ctx, []byte{0, 7})
	if err != nil || len(out) != 2 || out[1] != 7 {
		t.Fatalf("echo after clearing the latch: %v %v", out, err)
	}
}

// TestPitfall_SubmitWithoutWaitDoesNotWedgeThePool is the regression for a
// Submit whose result the reader has already stored and that nobody has
// Waited on. The permit used to stay in semTickets until Wait or Close, so
// pool_size of those Submits filled the semaphore and the next Submit blocked
// until its context died, with every worker idle.
func TestPitfall_SubmitWithoutWaitDoesNotWedgeThePool(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	first, err := h.Submit(ctx, []byte{0})
	if err != nil {
		t.Fatalf("first Submit: %v", err)
	}

	started := time.Now()
	second, err := h.Submit(ctx, []byte{0, 1})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Submit after an un-waited Submit failed in %s: %v (permit still held after the reader stored the result?)", elapsed, err)
	}
	if elapsed > time.Second {
		t.Fatalf("second Submit took %s; the idle worker's permit was not returned", elapsed)
	}

	if _, err := h.Wait(ctx, first); err != nil {
		t.Fatalf("Wait of the fire-and-forget ticket: %v", err)
	}
	if _, err := h.Wait(ctx, second); err != nil {
		t.Fatalf("Wait of the second ticket: %v", err)
	}
}

// goexitSpan is a trace carrier that ends its goroutine with runtime.Goexit,
// which is what t.Fatal and t.FailNow do inside an adopter's test carrier.
type goexitSpan struct{}

func (goexitSpan) TraceID() [16]byte {
	runtime.Goexit()
	return [16]byte{}
}
func (goexitSpan) SpanID() [8]byte { return [8]byte{} }

// A carrier that calls runtime.Goexit must not take a pool permit with it.
//
// readTraceCarrier recovers panics, but recover returns nil during Goexit: the
// goroutine keeps unwinding through submitInput, which read the carrier after
// taking a permit, so the `<-s.sem` on its error path never ran. Every such
// call removed one permit from the pool for the life of the handle; pool-size
// of them and every later Submit parked forever (I4).
func TestTraceCarrier_GoexitDoesNotLeakAPermit(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := context.WithValue(context.Background(), SpanContextKey, goexitSpan{})
		_, _ = h.Submit(ctx, []byte{0, 1})
		t.Error("Submit returned; the carrier's Goexit should have ended the goroutine")
	}()
	<-done

	if n := len(h.state.sem); n != 0 {
		t.Errorf("%d pool permit(s) held after the submitting goroutine exited (I4)", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := h.Call(ctx, []byte{0, 7})
	if err != nil || len(out) != 2 || out[1] != 7 {
		t.Fatalf("Call after a Goexiting carrier on a pool of 1: %v %v (the permit leaked)", out, err)
	}
}

type testSpanCarrier struct{ trace [16]byte }

func (s testSpanCarrier) TraceID() [16]byte { return s.trace }
func (s testSpanCarrier) SpanID() [8]byte   { return [8]byte{s.trace[0]} }

var benchSink []byte

func benchmarkCallNoop(b *testing.B, ctx context.Context) {
	h, err := Open(WithDiagnosticEngine())
	if err != nil {
		b.Fatal(err)
	}
	defer h.Close()
	in := []byte{0, 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := h.Call(ctx, in)
		if err != nil {
			b.Fatal(err)
		}
		benchSink = out
	}
}

func BenchmarkCallNoopBare(b *testing.B) { benchmarkCallNoop(b, context.Background()) }

func BenchmarkCallNoopTraced(b *testing.B) {
	benchmarkCallNoop(b, context.WithValue(context.Background(), SpanContextKey, testSpanCarrier{trace: [16]byte{1}}))
}

func openFDCount(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skipf("cannot list /dev/fd: %v", err)
	}
	return len(ents)
}

// A SetNonblock failure on the duplicated write end must fail Open, wrap the
// cause, and close both descriptors of the pipe pair: Rust would otherwise own
// a blocking write end, and a full pipe would block a worker inside write.
func TestOpenSetNonblockFailureFailsAndClosesBothDescriptors(t *testing.T) {
	injected := errors.New("injected SetNonblock failure")
	prev := setNonblock
	t.Cleanup(func() { setNonblock = prev })
	setNonblock = func(int, bool) error { return injected }

	const rounds = 50
	before := openFDCount(t)
	for i := 0; i < rounds; i++ {
		h, err := Open(WithPoolSize(1))
		if err == nil {
			_ = h.Close()
			t.Fatal("Open succeeded although SetNonblock failed")
		}
		if !errors.Is(err, injected) {
			t.Fatalf("Open error %v does not wrap the injected failure", err)
		}
	}
	// A leaked descriptor per Open is 50 or 100 here; 10 absorbs background noise.
	if after := openFDCount(t); after-before > 10 {
		t.Fatalf("descriptors grew from %d to %d over %d failed Opens", before, after, rounds)
	}
}
