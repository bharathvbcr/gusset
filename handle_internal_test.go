package gusset

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"

	"github.com/bharathvbcr/gusset/internal/ffi"
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
