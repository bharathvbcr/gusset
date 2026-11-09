package gusset

import (
	"errors"
	"os"
	"runtime"
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

// parkedStallReader stands in for a reader parked in a pipe read; its name is
// what the dump has to show.
func parkedStallReader(r *os.File, done chan<- error) {
	var b [8]byte
	_, err := r.Read(b[:])
	done <- err
}

// The dump must still hold the parked reader. Setting the read deadline first
// lets it exit before the stacks are taken, which is how the arm64 stall of
// run 42536723333 reached the log with no reader goroutine in it.
func TestStacksThenUnblock_DumpsTheParkedReaderThenReleasesIt(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	done := make(chan error, 1)
	go parkedStallReader(r, done)
	// Let the goroutine reach the read and park in the netpoller.
	for i := 0; i < 1000; i++ {
		time.Sleep(time.Millisecond)
		buf := make([]byte, 1<<16)
		if strings.Contains(string(buf[:runtime.Stack(buf, true)]), "parkedStallReader") {
			break
		}
	}

	dump := string(stacksThenUnblock(r))
	if !strings.Contains(dump, "parkedStallReader") {
		t.Errorf("the dump lost the parked reader:\n%s", dump)
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
