package gusset

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
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

	got := ringStallState(tr, map[uint64][]byte{7: []byte("abc"), 9: nil})
	for _, want := range []string{
		"ring: capacity 2, waiting 1, overflow 2",
		"slot 0: sequence 1",
		"slot 1: sequence 1",
		"head 0 (slot 0, sequence 1, published true), overflowSeen 0",
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
