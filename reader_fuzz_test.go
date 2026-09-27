package gusset

import (
	"encoding/binary"
	"os"
	"testing"
	"time"
	"unsafe"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// Arbitrary bytes on the completion pipe: every call to next either returns a
// record whose inline data fits the protocol or an error, and the stream ends
// (EOF) without a panic or a hang. Each record consumes at least 8 bytes, so
// the loop is bounded by the input.
func FuzzTicketReaderPipeBytes(f *testing.F) {
	rec := binary.NativeEndian.AppendUint64(nil, 7|ffi.InlineRecordFlag)
	rec = binary.NativeEndian.AppendUint64(rec, 3)
	rec = append(rec, 'a', 'b', 'c', 0, 0, 0, 0, 0)
	f.Add(rec)
	f.Add(binary.NativeEndian.AppendUint64(nil, 42))
	f.Add(binary.NativeEndian.AppendUint64(binary.NativeEndian.AppendUint64(nil, ffi.InlineRecordFlag), 1<<40))
	f.Add([]byte{1, 2, 3})
	f.Fuzz(func(t *testing.T, stream []byte) {
		if len(stream) > 1<<16 {
			return
		}
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		go func() {
			_, _ = w.Write(stream)
			_ = w.Close()
		}()
		tr := newTicketReader(r)
		_ = r.SetReadDeadline(time.Now().Add(10 * time.Second))
		for i := 0; i <= len(stream)/8; i++ {
			_, data, inline, err := tr.next()
			if err != nil {
				return
			}
			if !inline && data != nil {
				t.Fatal("a bare ticket carried data")
			}
			if len(data) > ffi.InlineResultMax {
				t.Fatalf("inline data of %d bytes passed the parser", len(data))
			}
		}
		if _, _, _, err := tr.next(); err == nil {
			t.Fatal("reader produced more records than the stream can hold")
		}
	})
}

// fakeRing lays out a ring header and slots in Go memory, as gusset.h
// describes them, so the reader's slot decoding can be driven directly.
func fakeRing(capacity uint64) (ffi.Ring, []uint64, []uint64) {
	shared := make([]uint64, 32)
	slots := make([]uint64, capacity*uint64(ffi.RingSlotBytes)/8)
	shared[ffi.RingOffCapacity/8] = capacity
	shared[ffi.RingOffSlotBytes/8] = uint64(ffi.RingSlotBytes)
	for i := uint64(0); i < capacity; i++ {
		slots[i*uint64(ffi.RingSlotBytes)/8] = i
	}
	return ffi.Ring{
		Owner:    unsafe.Pointer(&shared[0]), // never released in tests
		Shared:   unsafe.Pointer(&shared[0]),
		Slots:    unsafe.Pointer(&slots[0]),
		Capacity: capacity,
	}, shared, slots
}

// Arbitrary record words in a published ring slot: the reader returns the
// ticket and at most InlineResultMax bytes, or a clean error for an
// impossible length, and hands a decoded slot back to the producers.
func FuzzRingSlotDecode(f *testing.F) {
	f.Add(uint64(9), uint64(0), []byte{})
	f.Add(uint64(9)|ffi.InlineRecordFlag, uint64(5), []byte("hello"))
	f.Add(uint64(9)|ffi.InlineRecordFlag, uint64(49), []byte{})
	f.Add(^uint64(0), ^uint64(0), []byte{0xff})
	f.Fuzz(func(t *testing.T, w0, n uint64, data []byte) {
		ring, _, slots := fakeRing(2)
		rec := uint64(ffi.RingSlotOffRecord) / 8
		slots[rec] = w0
		slots[rec+1] = n
		payload := make([]byte, ffi.InlineResultMax)
		copy(payload, data)
		for i := 0; i < ffi.InlineResultMax/8; i++ {
			slots[rec+2+uint64(i)] = binary.NativeEndian.Uint64(payload[i*8:])
		}
		slots[0] = 1 // published for position 0

		tr := &ticketReader{}
		tr.attachRing(ring)
		ticket, got, inline, err := tr.next()
		isInline := w0&ffi.InlineRecordFlag != 0
		if isInline && n > uint64(ffi.InlineResultMax) {
			if err == nil {
				t.Fatalf("length %d accepted", n)
			}
			return
		}
		if err != nil {
			t.Fatalf("valid slot refused: %v", err)
		}
		if inline != isInline || ticket != w0&^ffi.InlineRecordFlag {
			t.Fatalf("got ticket %d inline %v from word %#x", ticket, inline, w0)
		}
		if inline && string(got) != string(payload[:n]) {
			t.Fatalf("data %q, want %q", got, payload[:n])
		}
		if slots[0] != 2 || tr.head != 1 {
			t.Fatalf("slot not handed back: seq %d head %d", slots[0], tr.head)
		}
	})
}
