package gusset

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The stall report must tell a lost wake-up apart from a lost completion. A
// record published at the reader's position while the waiting flag is still
// set is the lost wake-up shape, and the report has to say so in its own
// words, because a nightly failure is read from the log and nothing else.
func TestRingStallState_NamesAPublishedSlotAtTheReadersPosition(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	fr := newFakeRing(2, w)
	tr := newTicketReader(r)
	tr.attachRing(fr.ring())

	if !fr.tryPublish(encodeRecord(fuzzRecord{ticket: 7, inline: true, data: []byte("abc")})) {
		t.Fatal("an empty ring refused a record")
	}
	atomic.StoreUint32(fr.waiting(), 1)
	atomic.AddUint64(fr.overflow(), 2)
	// Bytes already read from the pipe but not yet parsed: the report prints
	// them raw so a partial record can be told from a whole one.
	copy(tr.buf[:], []byte{0xde, 0xad, 0xbe, 0xef, 1, 2, 3, 4})
	tr.start, tr.end = 2, 8

	got := ringStallState(tr, map[uint64][]byte{7: []byte("abc"), 9: nil})
	for _, want := range []string{
		"ring: capacity 2, waiting 1, overflow 2",
		"slot 0: sequence 1",
		"slot 1: sequence 1",
		"head 0 (slot 0, sequence 1, published true), overflowSeen 0, tokensOwed 0, buffered 6 bytes",
		"buffered bytes: beef01020304",
		"pipe: ",
		"outstanding tickets: [7 9]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stall report lacks %q:\n%s", want, got)
		}
	}
}

// With nothing published, the slot at the reader's position must read as
// unpublished, or every report would look like a lost wake-up.
func TestRingStallState_EmptyRingIsNotPublished(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	fr := newFakeRing(4, w)
	tr := newTicketReader(r)
	tr.attachRing(fr.ring())

	got := ringStallState(tr, map[uint64][]byte{})
	if !strings.Contains(got, "published false") || !strings.Contains(got, "outstanding tickets: []") {
		t.Errorf("empty ring reported as:\n%s", got)
	}
}

// start and end belong to a reader that may still be running, so a report can
// catch them torn (fillMode does end -= start, then start = 0). It must say so
// and carry on: a panic here would replace the whole report in the one case, a
// spinning reader, it exists for.
func TestRingStallState_TornBufferWindowIsNamedNotSliced(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	fr := newFakeRing(2, w)
	tr := newTicketReader(r)
	tr.attachRing(fr.ring())

	for _, c := range []struct{ start, end int }{
		{200, 0},   // start moved past end: end -= start landed first
		{0, 600},   // end past the 512-byte buffer
		{-1, 4},    // negative start
		{600, 700}, // both past the buffer
	} {
		tr.start, tr.end = c.start, c.end
		got := ringStallState(tr, map[uint64][]byte{})
		want := fmt.Sprintf("buffered bytes: <inconsistent start=%d end=%d len=%d: reader not parked>", c.start, c.end, len(tr.buf))
		if !strings.Contains(got, want) {
			t.Errorf("start=%d end=%d: report lacks %q:\n%s", c.start, c.end, want, got)
		}
		if !strings.Contains(got, "buffered inconsistent bytes") || !strings.Contains(got, "outstanding tickets: []") {
			t.Errorf("start=%d end=%d: the rest of the report is missing:\n%s", c.start, c.end, got)
		}
	}
}

// parkedStallReader stands in for a reader parked in a pipe read; its name is
// what the dump has to show.
func parkedStallReader(r *os.File, done chan<- error) {
	var b [8]byte
	_, err := r.Read(b[:])
	done <- err
}

// parkedReaderIn reports whether dump shows parkedStallReader blocked in the
// poller's IO wait: parked in the pipe read, not merely started.
func parkedReaderIn(dump string) bool {
	for _, g := range strings.Split(dump, "\n\n") {
		if strings.Contains(g, "parkedStallReader") && strings.Contains(g, "[IO wait") {
			return true
		}
	}
	return false
}

// startParkedReader starts parkedStallReader on a fresh pipe and returns only
// once it is parked in the read. It fails, rather than carries on, if the
// reader never parks.
func startParkedReader(t *testing.T) (r, w *os.File, done chan error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	done = make(chan error, 1)
	go parkedStallReader(r, done)
	deadline := time.Now().Add(10 * time.Second)
	for !parkedReaderIn(string(stacksThenUnblock(stackDumpMax, func([]byte) {}))) {
		if time.Now().After(deadline) {
			t.Fatal("the reader never parked in the pipe read")
		}
		time.Sleep(time.Millisecond)
	}
	return r, w, done
}

// The unblock step must run after the dump holds the parked reader. The step
// receives the captured dump, so this fails every time if the deadline is set
// first (the step then gets no dump), not only when the woken reader happens to
// be gone. That order is how the arm64 stall of run 42536723333 reached the log
// with no reader goroutine in it (inferred).
func TestStacksThenUnblock_CapturesTheParkedReaderBeforeUnblocking(t *testing.T) {
	r, _, done := startParkedReader(t)

	calls := 0
	dump := stacksThenUnblock(stackDumpMax, func(captured []byte) {
		calls++
		if !parkedReaderIn(string(captured)) {
			t.Errorf("unblock ran before the dump held the parked reader:\n%s", captured)
		}
		unblockReader(r)(captured)
	})
	if calls != 1 {
		t.Errorf("unblock ran %d times, want once", calls)
	}
	if !parkedReaderIn(string(dump)) {
		t.Errorf("the returned dump lost the parked reader:\n%s", dump)
	}
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("the released reader returned %v, want a deadline error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parked reader was not released after the dump")
	}
}

// A dump cut at its cap must say so; a silently short dump reads as complete.
func TestStacksThenUnblock_MarksATruncatedDump(t *testing.T) {
	cut := string(stacksThenUnblock(256, func([]byte) {}))
	if !strings.Contains(cut, "[stack dump truncated at 256 bytes]") {
		t.Errorf("a dump cut at 256 bytes carries no marker:\n%s", cut)
	}
	whole := string(stacksThenUnblock(stackDumpMax, func([]byte) {}))
	if strings.Contains(whole, "stack dump truncated") {
		t.Errorf("a dump that fits was marked truncated:\n%s", whole)
	}
}
