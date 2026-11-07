package gusset

import (
	"encoding/binary"
	"os"
	"testing"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

func pipeReaderWith(t *testing.T, stream []byte) *ticketReader {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if _, err := w.Write(stream); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	return newTicketReader(r)
}

func ringReaderWith(t *testing.T, stream []byte, word, n uint64) (*ticketReader, []uint64) {
	t.Helper()
	tr := pipeReaderWith(t, stream)
	ring, _, slots := fakeRing(2)
	rec := uint64(ffi.RingSlotOffRecord) / 8
	slots[rec] = word
	slots[rec+1] = n
	if word != 0 || n != 0 {
		slots[0] = 1 // published for position 0
	}
	tr.attachRing(ring)
	return tr, slots
}

func bareTicket(w uint64) []byte {
	return binary.NativeEndian.AppendUint64(nil, w)
}

// Tickets start at 1, so a ticket id of 0 outside the wake-token position is a
// corrupt stream: next returns an error, as it does for a corrupt length. (An
// id at or above 1<<63 cannot reach the reader: bit 63 is the inline flag and
// is stripped.)
func TestTicketReaderRejectsImpossibleZeroTicket(t *testing.T) {
	t.Run("pipe bare record", func(t *testing.T) {
		if _, _, _, err := pipeReaderWith(t, bareTicket(0)).next(); err == nil {
			t.Fatal("bare ticket 0 was accepted without a ring")
		}
	})
	t.Run("pipe inline record", func(t *testing.T) {
		if _, _, _, err := pipeReaderWith(t, inlineRecord(0, []byte("x"))).next(); err == nil {
			t.Fatal("inline ticket 0 was accepted on the pipe")
		}
	})
	t.Run("ring pipe inline record", func(t *testing.T) {
		tr, _ := ringReaderWith(t, inlineRecord(0, []byte("x")), 0, 0)
		tr.tokensOwed = 1
		if _, _, _, err := tr.next(); err == nil {
			t.Fatal("inline ticket 0 was accepted on the ring reader's overflow pipe")
		}
	})
	t.Run("ring slot bare", func(t *testing.T) {
		tr, slots := ringReaderWith(t, nil, 0, 0)
		slots[0] = 1
		if _, _, _, err := tr.next(); err == nil {
			t.Fatal("ring slot with ticket 0 was accepted")
		}
	})
	t.Run("ring slot inline", func(t *testing.T) {
		tr, _ := ringReaderWith(t, nil, ffi.InlineRecordFlag, 0)
		if _, _, _, err := tr.next(); err == nil {
			t.Fatal("inline ring slot with ticket 0 was accepted")
		}
	})
}

// A real inline record and a real bare ticket still pass.
func TestTicketReaderAcceptsNonzeroTickets(t *testing.T) {
	tr := pipeReaderWith(t, append(inlineRecord(7, []byte("ok")), bareTicket(9)...))
	tk, data, inline, err := tr.next()
	if err != nil || tk != 7 || !inline || string(data) != "ok" {
		t.Fatalf("inline: got (%d, %q, %v, %v)", tk, data, inline, err)
	}
	if tk, _, inline, err = tr.next(); err != nil || tk != 9 || inline {
		t.Fatalf("bare: got (%d, %v, %v)", tk, inline, err)
	}

	rtr, _ := ringReaderWith(t, nil, 5|ffi.InlineRecordFlag, 2)
	if tk, _, inline, err = rtr.next(); err != nil || tk != 5 || !inline {
		t.Fatalf("ring slot: got (%d, %v, %v)", tk, inline, err)
	}
}

// Ticket 0 in the wake-token position stays legal: a ring reader consumes it
// silently and returns the record behind it.
func TestRingReaderConsumesZeroWakeToken(t *testing.T) {
	stream := append(bareTicket(0), inlineRecord(4, []byte("y"))...)
	tr, _ := ringReaderWith(t, stream, 0, 0)
	tr.tokensOwed = 1
	tk, data, inline, err := tr.next()
	if err != nil || tk != 4 || !inline || string(data) != "y" {
		t.Fatalf("got (%d, %q, %v, %v), want (4, \"y\", true, nil)", tk, data, inline, err)
	}
	if tr.tokensOwed != 0 {
		t.Fatalf("tokensOwed = %d, want the token paid off", tr.tokensOwed)
	}
}
