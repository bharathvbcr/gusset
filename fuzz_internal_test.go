package gusset

// Byte-level and stateful fuzzing of the Go side of the boundary:
//
//   - FuzzTicketReaderPipeSplitReads: arbitrary bytes, split at arbitrary read
//     boundaries with arbitrary pauses, against a reference parser that sees
//     the whole stream at once.
//   - FuzzTicketReaderPipeRecords: only well-formed records, so deep streams
//     are exercised instead of dying at the first bogus length word.
//   - FuzzTicketReaderFakeRing: the ring path (next/parseBuffered/fillMode/
//     waitRing) against a Go producer that follows the Rust protocol
//     (pool::ring::Ring::try_publish, take_waiter, note_overflow) with several
//     producers, a small ring, overflow to the pipe and wake tokens.
//   - FuzzTicketReaderRustRing: the real Rust ring and inline_record encoder
//     against this parser.
//   - FuzzTicketReaderPipeBytes: arbitrary bytes on the completion pipe.
//   - FuzzRingSlotDecode: arbitrary record words in a published ring slot.
//   - FuzzHandleOpSequence: end-to-end differential fuzzing of a real handle
//     against a model written from the documented contract.
//   - FuzzOpcodeFromContext, FuzzReadTraceCarrier, FuzzExtractCallHeaderDeadline:
//     CallHeader construction, opcode validation, panic-safe trace carrier,
//     and deadline stamping.
//
// Default `go test` runs only the seed corpus; run each target with
// `go test -run '^$' -fuzz '^FuzzTicketReaderPipeSplitReads$' -fuzztime 5m .`

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// fuzzRecord is one completion as the reader should report it.
type fuzzRecord struct {
	ticket uint64
	data   []byte
	inline bool
}

func (r fuzzRecord) String() string {
	return fmt.Sprintf("{t=%#x inline=%v data=%x}", r.ticket, r.inline, r.data)
}

type oracleEnd int

const (
	endEOF     oracleEnd = iota // clean or partial trailing record: io.EOF
	endCorrupt                  // an inline length word above InlineResultMax
)

// oracleParse is the reference parser: the whole stream at once, no reads,
// no buffer, straight from the gusset.h record layout.
func oracleParse(b []byte) ([]fuzzRecord, oracleEnd) {
	var out []fuzzRecord
	for {
		if len(b) < 8 {
			return out, endEOF
		}
		w := binary.NativeEndian.Uint64(b)
		if w&ffi.InlineRecordFlag == 0 {
			out = append(out, fuzzRecord{ticket: w})
			b = b[8:]
			continue
		}
		if len(b) < 16 {
			return out, endEOF
		}
		n := binary.NativeEndian.Uint64(b[8:])
		if n > uint64(ffi.InlineResultMax) {
			return out, endCorrupt
		}
		size := 16 + (int(n)+7)/8*8
		if len(b) < size {
			return out, endEOF
		}
		out = append(out, fuzzRecord{
			ticket: w &^ ffi.InlineRecordFlag,
			data:   append([]byte{}, b[16:16+n]...),
			inline: true,
		})
		b = b[size:]
	}
}

// encodeRecord writes one record exactly as pool::inline_record (or a bare
// ticket) lays it out.
func encodeRecord(r fuzzRecord) []byte {
	if !r.inline {
		return binary.NativeEndian.AppendUint64(nil, r.ticket)
	}
	out := binary.NativeEndian.AppendUint64(nil, r.ticket|ffi.InlineRecordFlag)
	out = binary.NativeEndian.AppendUint64(out, uint64(len(r.data)))
	out = append(out, r.data...)
	for len(out)%8 != 0 {
		out = append(out, 0)
	}
	return out
}

// splitPlan turns fuzz bytes into write chunk sizes and pauses. Each byte:
// low 6 bits are the chunk length (0 means 1..64 from the next byte's value is
// not needed: 0 reads as 1), high 2 bits choose a pause of none, a yield,
// ~60 us (inside the 50-200 us spin budget) or ~300 us (past it, so the
// reader parks in the netpoller).
type chunkStep struct {
	n     int
	pause int
}

func splitPlan(splits []byte) []chunkStep {
	if len(splits) == 0 {
		return []chunkStep{{n: 1 << 20}}
	}
	steps := make([]chunkStep, len(splits))
	for i, s := range splits {
		n := int(s & 0x3F)
		if n == 0 {
			n = 1
		}
		steps[i] = chunkStep{n: n, pause: int(s >> 6)}
	}
	return steps
}

const maxSleepingPauses = 48

func doPause(p int) {
	switch p {
	case 1:
		runtime.Gosched()
	case 2:
		time.Sleep(60 * time.Microsecond)
	case 3:
		time.Sleep(300 * time.Microsecond)
	}
}

// writeSplit writes data in the planned chunks (cycling the plan) and closes w.
func writeSplit(w *os.File, data []byte, steps []chunkStep) {
	defer w.Close()
	for i := 0; len(data) > 0; i++ {
		st := steps[i%len(steps)]
		n := min(st.n, len(data))
		if _, err := w.Write(data[:n]); err != nil {
			return // reader stopped early (corrupt stream) and closed its end
		}
		data = data[n:]
		// Sleeping pauses are capped per input: a 64 KiB stream written a
		// byte at a time with a 300 us pause each is 20 s of wall clock per
		// exec, which starves the fuzzer without covering anything new.
		if st.pause < 2 || i < maxSleepingPauses {
			doPause(st.pause)
		}
	}
}

// newFuzzTicketReader builds a reader on r. rawConn=false leaves tr.rc nil,
// so fill takes the io.ReadAtLeast path instead of RawConn.Read.
func newFuzzTicketReader(r *os.File, rawConn bool) *ticketReader {
	if rawConn {
		return newTicketReader(r)
	}
	tr := &ticketReader{f: r}
	tr.readFn = tr.readOnce
	return tr
}

const readerWatchdog = 10 * time.Second

// drainReader calls next until it errors, copying each result out (the data
// aliases the reader's buffer). It fails the test if next stops making
// progress for readerWatchdog.
func drainReader(t *testing.T, tr *ticketReader, r *os.File, between func(int)) ([]fuzzRecord, error) {
	t.Helper()
	type step struct {
		rec fuzzRecord
		err error
	}
	steps := make(chan step, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; ; i++ {
			tk, d, in, err := tr.next()
			if err != nil {
				steps <- step{err: err}
				return
			}
			if len(d) > ffi.InlineResultMax {
				steps <- step{err: fmt.Errorf("record of %d bytes > InlineResultMax", len(d))}
				return
			}
			if !in && d != nil {
				steps <- step{err: fmt.Errorf("bare ticket %#x carried %d bytes", tk, len(d))}
				return
			}
			if tr.start < 0 || tr.start > tr.end || tr.end > len(tr.buf) {
				steps <- step{err: fmt.Errorf("reader cursor out of range: start %d end %d", tr.start, tr.end)}
				return
			}
			rec := fuzzRecord{ticket: tk, inline: in}
			if in {
				rec.data = append([]byte{}, d...)
			}
			select {
			case steps <- step{rec: rec}:
			case <-stop:
				return
			}
			if between != nil {
				between(i)
			}
		}
	}()
	var got []fuzzRecord
	for {
		select {
		case s := <-steps:
			if s.err != nil {
				return got, s.err
			}
			got = append(got, s.rec)
		case <-time.After(readerWatchdog):
			_ = r.SetReadDeadline(time.Now())
			buf := make([]byte, 1<<20)
			t.Fatalf("ticketReader made no progress for %v after %d records (livelock or lost wake-up)\n%s",
				readerWatchdog, len(got), buf[:runtime.Stack(buf, true)])
			return nil, nil
		}
	}
}

func sameRecords(a, b []fuzzRecord) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ticket != b[i].ticket || a[i].inline != b[i].inline || !bytes.Equal(a[i].data, b[i].data) {
			return false
		}
	}
	return true
}

// checkPipeStream runs data through a pipe-only reader and compares it with
// the oracle.
func checkPipeStream(t *testing.T, data, splits []byte, rawConn bool) {
	wantRecs, wantEnd := oracleParse(data)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	tr := newFuzzTicketReader(r, rawConn)
	go writeSplit(w, data, splitPlan(splits))

	got, rerr := drainReader(t, tr, r, nil)
	_ = r.Close() // unblocks a writer still holding bytes after a corrupt record

	if !sameRecords(got, wantRecs) {
		t.Fatalf("records differ (rawConn=%v)\n got %v\nwant %v", rawConn, got, wantRecs)
	}
	switch wantEnd {
	case endEOF:
		if !errors.Is(rerr, io.EOF) {
			t.Fatalf("stream end: got %v, want io.EOF", rerr)
		}
	case endCorrupt:
		if rerr == nil || !strings.Contains(rerr.Error(), "corrupt completion record") {
			t.Fatalf("corrupt length: got %v, want a corrupt completion record error", rerr)
		}
	}
}

// FuzzTicketReaderPipeSplitReads: any byte stream, any read split. The reader must
// agree with the whole-stream oracle, never return more than InlineResultMax
// bytes, report a bad length as corruption, and always terminate.
func FuzzTicketReaderPipeSplitReads(f *testing.F) {
	bare := binary.NativeEndian.AppendUint64(nil, 7)
	inl := encodeRecord(fuzzRecord{ticket: 9, data: []byte("hello"), inline: true})
	full := encodeRecord(fuzzRecord{ticket: 1 << 40, data: bytes.Repeat([]byte{0xAB}, ffi.InlineResultMax), inline: true})
	bad := binary.NativeEndian.AppendUint64(binary.NativeEndian.AppendUint64(nil, 3|ffi.InlineRecordFlag), uint64(ffi.InlineResultMax+1))
	huge := binary.NativeEndian.AppendUint64(binary.NativeEndian.AppendUint64(nil, 3|ffi.InlineRecordFlag), ^uint64(0))
	f.Add([]byte{}, []byte{}, true)
	f.Add(bare, []byte{1}, true)
	f.Add(append(append([]byte{}, bare...), inl...), []byte{3, 5, 0x81}, false)
	f.Add(bytes.Repeat(full, 12), []byte{0x3F, 0x07, 0xC1}, true) // > 512 B buffer, reads end mid-record
	f.Add(append(append([]byte{}, inl...), bad...), []byte{16}, true)
	f.Add(huge, []byte{9}, false)
	f.Add(append(append([]byte{}, inl...), 1, 2, 3), []byte{2}, true) // partial trailing record

	f.Fuzz(func(t *testing.T, data, splits []byte, rawConn bool) {
		if len(data) > 1<<16 {
			data = data[:1<<16]
		}
		checkPipeStream(t, data, splits, rawConn)
	})
}

// recordsFromFuzz decodes fuzz bytes into well-formed records: per record a
// kind byte (bit 0: inline), a 7-byte ticket, and for inline a length byte
// (mod InlineResultMax+1) followed by that many payload bytes.
func recordsFromFuzz(b []byte) []fuzzRecord {
	var out []fuzzRecord
	for len(b) >= 8 {
		kind := b[0]
		var tb [8]byte
		copy(tb[:7], b[1:8])
		b = b[8:]
		rec := fuzzRecord{ticket: binary.LittleEndian.Uint64(tb[:]) &^ ffi.InlineRecordFlag}
		if kind&1 == 1 {
			rec.inline = true
			n := 0
			if len(b) > 0 {
				n = int(b[0]) % (ffi.InlineResultMax + 1)
				b = b[1:]
			}
			n = min(n, len(b))
			rec.data = append([]byte{}, b[:n]...)
			b = b[n:]
		}
		out = append(out, rec)
	}
	return out
}

// FuzzTicketReaderPipeRecords: a valid stream round-trips exactly, however
// the reads are split.
func FuzzTicketReaderPipeRecords(f *testing.F) {
	f.Add([]byte{1, 1, 0, 0, 0, 0, 0, 0, 5, 'a', 'b', 'c', 'd', 'e'}, []byte{1}, true)
	f.Add(bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7, 8, 48}, 80), []byte{0x3F, 0x3E, 0x01}, true)
	f.Add(bytes.Repeat([]byte{0, 9, 9, 9, 9, 9, 9, 9}, 100), []byte{0xC7}, false)

	f.Fuzz(func(t *testing.T, spec, splits []byte, rawConn bool) {
		recs := recordsFromFuzz(spec)
		var stream []byte
		for _, r := range recs {
			stream = append(stream, encodeRecord(r)...)
		}
		// Self-check the oracle against the generator before trusting it.
		if o, end := oracleParse(stream); end != endEOF || !sameRecords(o, recs) {
			t.Fatalf("oracle disagrees with generator: %v vs %v", o, recs)
		}
		checkPipeStream(t, stream, splits, rawConn)
	})
}

// fzFakeRing is a completion ring in Go memory with the gusset.h layout, and a
// producer side that follows pool::ring::Ring and Handle::complete.
type fzFakeRing struct {
	shared   []uint64 // 256 bytes: capacity@0, slot_bytes@8, waiting@64, overflow@128, tail@192
	slots    []uint64 // capacity * 16 words
	capacity uint64
	w        *os.File
	wmu      sync.Mutex
}

const fakeRingTailWord = 192 / 8

func newFakeRing(capacity uint64, w *os.File) *fzFakeRing {
	fr := &fzFakeRing{
		shared:   make([]uint64, 32),
		slots:    make([]uint64, capacity*uint64(ffi.RingSlotBytes/8)),
		capacity: capacity,
		w:        w,
	}
	fr.shared[0] = capacity
	fr.shared[1] = uint64(ffi.RingSlotBytes)
	for i := uint64(0); i < capacity; i++ {
		fr.slots[i*uint64(ffi.RingSlotBytes/8)] = i
	}
	return fr
}

func (fr *fzFakeRing) ring() ffi.Ring {
	return ffi.Ring{
		Owner:    unsafe.Pointer(&fr.shared[0]), // only ever compared with nil here
		Shared:   unsafe.Pointer(&fr.shared[0]),
		Slots:    unsafe.Pointer(&fr.slots[0]),
		Capacity: fr.capacity,
	}
}

func (fr *fzFakeRing) waiting() *uint32 {
	return (*uint32)(unsafe.Add(unsafe.Pointer(&fr.shared[0]), ffi.RingOffWaiting))
}

func (fr *fzFakeRing) overflow() *uint64 {
	return (*uint64)(unsafe.Add(unsafe.Pointer(&fr.shared[0]), ffi.RingOffOverflow))
}

// tryPublish mirrors Ring::try_publish (Vyukov MPSC).
func (fr *fzFakeRing) tryPublish(record []byte) bool {
	tail := &fr.shared[fakeRingTailWord]
	mask := fr.capacity - 1
	stride := uint64(ffi.RingSlotBytes / 8)
	pos := atomic.LoadUint64(tail)
	for {
		base := (pos & mask) * stride
		seq := atomic.LoadUint64(&fr.slots[base])
		switch d := int64(seq - pos); {
		case d == 0:
			if atomic.CompareAndSwapUint64(tail, pos, pos+1) {
				for i := 0; i < len(record)/8; i++ {
					atomic.StoreUint64(&fr.slots[base+1+uint64(i)], binary.NativeEndian.Uint64(record[i*8:]))
				}
				atomic.StoreUint64(&fr.slots[base], pos+1)
				return true
			}
			pos = atomic.LoadUint64(tail)
		case d < 0:
			return false
		default:
			pos = atomic.LoadUint64(tail)
		}
	}
}

func (fr *fzFakeRing) pipeWrite(b []byte) {
	fr.wmu.Lock() // one write per record, as write_completion's lock does
	_, _ = fr.w.Write(b)
	fr.wmu.Unlock()
}

// complete mirrors Handle::complete for a host with a ring attached.
func (fr *fzFakeRing) complete(record []byte) {
	if fr.tryPublish(record) {
		// take_waiter: a SeqCst fence, then swap. Go atomics are sequentially
		// consistent, which is at least as strong.
		if atomic.LoadUint32(fr.waiting()) != 0 && atomic.SwapUint32(fr.waiting(), 0) != 0 {
			fr.pipeWrite(make([]byte, 8)) // wake token: ticket 0
		}
		return
	}
	fr.pipeWrite(record)
	atomic.AddUint64(fr.overflow(), 1)
}

// FuzzTicketReaderFakeRing: completions published by 1-3 concurrent
// producers into a small ring, spilling to the pipe when it is full, with
// wake tokens whenever the reader announced a park. Every record must come
// out exactly once, tokens must never surface as completions, and the reader
// must never park with a record left in the ring.
func FuzzTicketReaderFakeRing(f *testing.F) {
	f.Add(uint8(0), uint8(1), []byte{1, 1, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c'}, []byte{0})
	f.Add(uint8(0), uint8(3), bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7, 8, 48}, 40), []byte{0x80, 0x00, 0xC0})
	f.Add(uint8(2), uint8(2), bytes.Repeat([]byte{0, 9, 9, 9, 9, 9, 9, 9}, 64), []byte{0x40, 0xC0})
	f.Add(uint8(1), uint8(1), bytes.Repeat([]byte{1, 3, 1, 3, 1, 3, 1, 3, 0}, 30), []byte{0xFF, 0x03})

	f.Fuzz(func(t *testing.T, capLog, producers uint8, spec, pauses []byte) {
		capacity := uint64(2) << (capLog % 3) // 2, 4, 8
		nprod := int(producers%3) + 1
		spec = spec[:min(len(spec), 4096)]
		recs := recordsFromFuzz(spec)
		// Unique non-zero tickets: ticket 0 is the wake token on this path.
		for i := range recs {
			recs[i].ticket = uint64(i + 1)
		}
		if len(pauses) == 0 {
			pauses = []byte{0}
		}

		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		fr := newFakeRing(capacity, w)
		tr := newTicketReader(r)
		tr.attachRing(fr.ring())

		var wg sync.WaitGroup
		for p := 0; p < nprod; p++ {
			wg.Add(1)
			go func(p int) {
				defer wg.Done()
				for i := p; i < len(recs); i += nprod {
					fr.complete(encodeRecord(recs[i]))
					if i < maxSleepingPauses {
						doPause(int(pauses[i%len(pauses)] >> 6))
					}
				}
			}(p)
		}
		go func() {
			wg.Wait()
			_ = w.Close()
		}()

		got, rerr := drainReader(t, tr, r, func(i int) {
			if i < maxSleepingPauses {
				doPause(int(pauses[i%len(pauses)]) & 3)
			}
		})
		if !errors.Is(rerr, io.EOF) {
			t.Fatalf("reader ended with %v after %d/%d records, want io.EOF", rerr, len(got), len(recs))
		}
		sort.Slice(got, func(i, j int) bool { return got[i].ticket < got[j].ticket })
		if !sameRecords(got, recs) {
			t.Fatalf("records differ (cap %d, %d producers)\n got %v\nwant %v", capacity, nprod, got, recs)
		}
		if ov := atomic.LoadUint64(fr.overflow()); tr.overflowSeen != ov {
			t.Fatalf("reader counted %d pipe records, producers %d", tr.overflowSeen, ov)
		}
	})
}

// FuzzTicketReaderRustRing: the real Rust encoder (pool::inline_record) and
// ring against this parser. Pool 1, ring capacity 2: completions beyond two
// unread spill to the pipe, so both paths carry Rust-built records. Payloads
// above InlineResultMax come back as bare tickets and are collected with Take.
func FuzzTicketReaderRustRing(f *testing.F) {
	f.Add([]byte{0, 1, 47, 48, 49, 200})
	f.Add([]byte{48, 48, 48, 48, 48})
	f.Add([]byte{8, 0, 16})

	f.Fuzz(func(t *testing.T, sizes []byte) {
		if len(sizes) == 0 || len(sizes) > 16 {
			return
		}
		h, ring, r := rawRingHandle(t)
		tr := newTicketReader(r)
		tr.attachRing(ring)

		want := map[uint64][]byte{}
		consumeOne := func() {
			type res struct {
				tk  uint64
				d   []byte
				in  bool
				err error
			}
			ch := make(chan res, 1)
			go func() {
				tk, d, in, err := tr.next()
				ch <- res{tk: tk, d: append([]byte{}, d...), in: in, err: err}
			}()
			var got res
			select {
			case got = <-ch:
			case <-time.After(readerWatchdog):
				_ = r.SetReadDeadline(time.Now())
				t.Fatalf("reader stalled with %d completions outstanding", len(want))
			}
			if got.err != nil {
				t.Fatalf("next: %v", got.err)
			}
			exp, ok := want[got.tk]
			if !ok {
				t.Fatalf("ticket %#x not outstanding (or delivered twice)", got.tk)
			}
			delete(want, got.tk)
			data := got.d
			if !got.in {
				if len(exp) <= ffi.InlineResultMax {
					t.Fatalf("ticket %#x: %d-byte result came back bare, want inline", got.tk, len(exp))
				}
				id, out, err := ffi.Take(h, got.tk)
				if err != nil {
					t.Fatalf("take %#x: %v", got.tk, err)
				}
				data = append([]byte{}, out...)
				if id != 0 {
					_ = ffi.BufFree(h, id&^ffi.TakeOwnedFlag)
				}
			} else if len(exp) > ffi.InlineResultMax {
				t.Fatalf("ticket %#x: %d-byte result came back inline", got.tk, len(exp))
			}
			if !bytes.Equal(data, exp) {
				t.Fatalf("ticket %#x: got %x want %x", got.tk, data, exp)
			}
		}

		for i, s := range sizes {
			n := int(s) // 0..255 bytes: both sides of InlineResultMax
			in := make([]byte, n)
			for j := range in {
				in[j] = byte(i*31 + j)
			}
			if n > 0 {
				in[0] = 0 // diagnostic mode 0: echo
			}
			for {
				tk, err := ffi.Submit(h, inlineEcho, in, 0)
				if err == nil {
					want[tk] = in
					break
				}
				// The submission queue is bounded; drain one completion
				// (unread ones pile into the ring, then the pipe) and retry.
				if !strings.Contains(err.Error(), "queue is full") || len(want) == 0 {
					t.Fatalf("submit %d: %v", i, err)
				}
				if s&1 == 1 { // sometimes let completions pile up first
					time.Sleep(200 * time.Microsecond)
				}
				consumeOne()
			}
		}
		for len(want) > 0 {
			consumeOne()
		}
	})
}

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
		records := 0
		for {
			_, data, inline, err := tr.next()
			if err != nil {
				break
			}
			records++
			if records > len(stream)/8 {
				t.Fatalf("%d records from %d bytes: a record is at least 8", records, len(stream))
			}
			if !inline && data != nil {
				t.Fatal("a bare ticket carried data")
			}
			if len(data) > ffi.InlineResultMax {
				t.Fatalf("inline data of %d bytes passed the parser", len(data))
			}
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

type modelTicket struct {
	want    []byte // expected result bytes
	wantErr string // expected error substring instead of bytes
}

// echoLens and allocLens are the payload sizes worth hitting: both sides of
// the inline record limit, of the 4 KiB copy limit, and of mode 16's 64 KiB
// chunk.
var (
	echoLens  = []int{0, 1, 2, 47, 48, 49, 100, 4095, 4096, 4097}
	allocLens = []int{-1, 0, 1, 48, 49, 4096, 4097, 65535, 65536, 65537, 1 << 20}
)

func mode16Input(n int, seed byte) []byte {
	if n < 0 {
		return []byte{16, 1} // too short for the length word: an engine error
	}
	in := []byte{16, 0, 0, 0, 0, seed}
	binary.LittleEndian.PutUint32(in[1:5], uint32(n))
	return in
}

func mode16Output(n int, seed byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i) ^ seed
	}
	return out
}

const opBound = 10 * time.Second

func FuzzHandleOpSequence(f *testing.F) {
	f.Add(uint8(0), []byte{0, 3, 2, 0})
	f.Add(uint8(1), []byte{1, 4, 0, 5, 2, 0, 3, 1})
	f.Add(uint8(5), []byte{0, 9, 1, 10, 4, 0, 5, 0, 6, 0, 0, 1, 7, 0, 2, 0, 5, 3})
	f.Add(uint8(3), []byte{1, 6, 1, 7, 1, 8, 4, 0, 4, 1, 4, 2, 0, 0})
	f.Add(uint8(6), []byte{0, 1, 0, 2, 0, 3, 0, 4, 4, 0, 4, 0, 4, 0, 4, 0, 0, 5, 2, 0})
	f.Add(uint8(2), []byte{7, 0, 0, 1, 2, 0, 7, 0})

	f.Fuzz(func(t *testing.T, cfg uint8, ops []byte) {
		if len(ops) > 96 {
			ops = ops[:96]
		}
		pool := int(cfg%4) + 1
		opts := []Option{WithPoolSize(pool), WithDiagnosticEngine()}
		if cfg&4 != 0 {
			opts = append(opts, withPipeOnly())
		}
		baseline := Stats().LiveBytes

		h, err := Open(opts...)
		if err != nil {
			t.Fatal(err)
		}
		s := h.state
		closed := false
		var closeErr error
		defer func() {
			if !closed {
				_ = h.Close()
			}
		}()

		live := map[uint64]modelTicket{}
		var order []uint64 // live tickets in submission order, for picking
		var spent []uint64 // collected or abandoned: must answer ErrUnknownTicket
		abandoned := 0

		pick := func(arg byte) (uint64, bool) {
			if len(order) == 0 {
				return 0, false
			}
			return order[int(arg)%len(order)], true
		}
		retire := func(tk uint64) {
			delete(live, tk)
			order = slices.DeleteFunc(order, func(x uint64) bool { return x == tk })
			spent = append(spent, tk)
		}
		bounded := func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), opBound)
		}
		done := func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}
		checkResult := func(what string, tk uint64, mt modelTicket, got []byte, err error) {
			t.Helper()
			if mt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), mt.wantErr) {
					t.Fatalf("%s ticket %d: err %v, want %q", what, tk, err, mt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s ticket %d: %v", what, tk, err)
			}
			if !bytes.Equal(got, mt.want) {
				t.Fatalf("%s ticket %d: %d bytes, want %d (first diff at %d)", what, tk, len(got), len(mt.want), firstDiff(got, mt.want))
			}
		}
		closedErr := func(what string, err error) {
			t.Helper()
			if err == nil || !strings.Contains(err.Error(), "handle is closed") {
				t.Fatalf("%s after Close: %v, want handle is closed", what, err)
			}
		}
		submit := func(in []byte, mt modelTicket, refuse string) {
			t.Helper()
			if closed {
				_, err := h.Submit(context.Background(), in)
				closedErr("Submit", err)
				return
			}
			if refuse != "" {
				_, err := h.Submit(context.Background(), in)
				if err == nil || !strings.Contains(err.Error(), refuse) {
					t.Fatalf("Submit %d bytes: %v, want %q", len(in), err, refuse)
				}
				return
			}
			if len(live) >= pool {
				// The model still tracks uncollected tickets as live. A finished
				// ticket that nobody has Waited on no longer holds a permit, so
				// this skips a Submit that would now succeed. That is the
				// conservative side: it never claims a permit is free when one
				// is actually still held by a running or abandoned job.
				return
			}
			ctx, cancel := bounded()
			defer cancel()
			tk, err := h.Submit(ctx, in)
			if err != nil {
				// The model says a permit is free or will be once an
				// abandoned job lands: a timeout here is a leaked permit.
				t.Fatalf("Submit refused with %d live, %d abandoned, pool %d: %v (I4 permit leak?)",
					len(live), abandoned, pool, err)
			}
			if _, dup := live[tk]; dup || slices.Contains(spent, tk) {
				t.Fatalf("ticket %d issued twice", tk)
			}
			live[tk] = mt
			order = append(order, tk)
		}

		for i := 0; i+1 < len(ops); i += 2 {
			op, arg := ops[i]%8, ops[i+1]
			switch op {
			case 0: // Submit echo
				n := echoLens[int(arg)%len(echoLens)]
				in := make([]byte, n)
				for j := range in {
					in[j] = byte(j*7) + arg
				}
				if n > 0 {
					in[0] = 0
				}
				refuse := ""
				if n > 4096 {
					refuse = "copy limit"
				}
				submit(in, modelTicket{want: in}, refuse)
			case 1: // Submit mode 16
				n := allocLens[int(arg)%len(allocLens)]
				mt := modelTicket{want: mode16Output(max(n, 0), arg)}
				if n < 0 {
					mt = modelTicket{wantErr: "mode 16 needs a u32 LE length"}
				}
				submit(mode16Input(n, arg), mt, "")
			case 2, 3: // Wait / WaitBuffer on a live ticket
				tk, ok := pick(arg)
				if !ok {
					continue
				}
				ctx, cancel := bounded()
				if closed {
					var err error
					if op == 2 {
						_, err = h.Wait(ctx, tk)
					} else {
						_, err = h.WaitBuffer(ctx, tk)
					}
					cancel()
					closedErr("Wait", err)
					retire(tk)
					continue
				}
				var got []byte
				var err error
				if op == 2 {
					got, err = h.Wait(ctx, tk)
				} else {
					var b *Buffer
					b, err = h.WaitBuffer(ctx, tk)
					if err == nil {
						got = slices.Clone(b.Bytes())
						if ferr := b.Free(); ferr != nil {
							t.Fatalf("Free: %v", ferr)
						}
					}
				}
				timedOut := ctx.Err() != nil
				cancel()
				if timedOut {
					t.Fatalf("Wait on ticket %d did not return within %v", tk, opBound)
				}
				checkResult("Wait", tk, live[tk], got, err)
				retire(tk)
			case 4: // cancel: Wait with a context that is already done
				tk, ok := pick(arg)
				if !ok {
					continue
				}
				got, err := h.Wait(done(), tk)
				if closed {
					closedErr("cancel", err)
				} else if errors.Is(err, context.Canceled) {
					abandoned++
				} else {
					// The completion was already in: a real answer wins.
					checkResult("cancel", tk, live[tk], got, err)
				}
				retire(tk)
			case 5: // Wait on a spent or never-issued ticket
				var tk uint64
				if len(spent) > 0 && arg&1 == 0 {
					tk = spent[int(arg>>1)%len(spent)]
				} else {
					tk = 1<<40 + uint64(arg)
				}
				ctx, cancel := bounded()
				_, err := h.Wait(ctx, tk)
				cancel()
				if closed {
					closedErr("stale Wait", err)
				} else if !errors.Is(err, ErrUnknownTicket) {
					t.Fatalf("Wait on spent/unknown ticket %d: %v, want ErrUnknownTicket", tk, err)
				}
			case 6: // Submit with a done context
				_, err := h.Submit(done(), []byte{0, arg})
				if closed {
					closedErr("Submit(done)", err)
				} else if !errors.Is(err, context.Canceled) {
					t.Fatalf("Submit with a cancelled context: %v", err)
				}
			case 7: // Close (again)
				err := h.Close()
				if closed && !sameErr(err, closeErr) {
					t.Fatalf("second Close: %v, first %v", err, closeErr)
				}
				if !closed && err != nil {
					t.Fatalf("Close: %v", err)
				}
				closed, closeErr = true, err
			}
		}

		// Collect everything still live.
		for _, tk := range slices.Clone(order) {
			ctx, cancel := bounded()
			got, err := h.Wait(ctx, tk)
			cancel()
			if closed {
				closedErr("final Wait", err)
			} else {
				checkResult("final Wait", tk, live[tk], got, err)
			}
			retire(tk)
		}

		// I4: abandoned jobs return their permits when they land.
		if !closed {
			deadline := time.Now().Add(opBound)
			for {
				s.mu.Lock()
				held, semHeld := len(s.semTickets), len(s.sem)
				s.mu.Unlock()
				if held == 0 && semHeld == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("I4: %d tickets / %d permits still held with nothing live (%d abandoned)", held, semHeld, abandoned)
				}
				time.Sleep(100 * time.Microsecond)
			}
			if err := h.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			closed = true
		}

		s.mu.Lock()
		leftovers := len(s.sem) + len(s.semTickets) + len(s.completed) + len(s.takeIDs) + len(s.abandoned)
		for _, ch := range s.pending {
			if ch != nil {
				leftovers++
			}
		}
		s.mu.Unlock()
		if leftovers != 0 {
			t.Fatalf("after Close: sem %d semTickets %d completed %d takeIDs %d abandoned %d pending %v",
				len(s.sem), len(s.semTickets), len(s.completed), len(s.takeIDs), len(s.abandoned), s.pending)
		}
		// I1: every Rust buffer this sequence created has been freed.
		if live := Stats().LiveBytes; live > baseline {
			t.Fatalf("I1: Rust live bytes %d after Close, %d before Open", live, baseline)
		}
	})
}

func sameErr(a, b error) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Error() == b.Error()
}

type (
	fzOp8   uint8
	fzOp16  uint16
	fzOp32  uint32
	fzOp64  uint64
	fzOpInt int
	fzOpI8  int8
	fzOpI64 int64
	fzOpPtr uintptr
	fzOpF   float64
	fzOpS   string
)

// opcodeTypes covers every reflect integer kind, named and unnamed, plus the
// non-integer kinds that must be refused.
var opcodeTypes = []reflect.Type{
	reflect.TypeFor[int](), reflect.TypeFor[int8](), reflect.TypeFor[int16](),
	reflect.TypeFor[int32](), reflect.TypeFor[int64](),
	reflect.TypeFor[uint](), reflect.TypeFor[uint8](), reflect.TypeFor[uint16](),
	reflect.TypeFor[uint32](), reflect.TypeFor[uint64](), reflect.TypeFor[uintptr](),
	reflect.TypeFor[fzOp8](), reflect.TypeFor[fzOp16](), reflect.TypeFor[fzOp32](),
	reflect.TypeFor[fzOp64](), reflect.TypeFor[fzOpInt](), reflect.TypeFor[fzOpI8](),
	reflect.TypeFor[fzOpI64](), reflect.TypeFor[fzOpPtr](),
	reflect.TypeFor[float32](), reflect.TypeFor[float64](), reflect.TypeFor[fzOpF](),
	reflect.TypeFor[string](), reflect.TypeFor[fzOpS](), reflect.TypeFor[bool](),
	reflect.TypeFor[*uint32](), reflect.TypeFor[[]byte](), reflect.TypeFor[complex128](),
}

// opcodeOracle is what opcodeFromContext must answer for rv, derived from
// the value's mathematical meaning rather than from the implementation.
func opcodeOracle(rv reflect.Value) (uint32, bool) {
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n := rv.Int()
		if n < 0 || n > math.MaxUint32 {
			return 0, false
		}
		return uint32(n), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n := rv.Uint()
		if n > math.MaxUint32 {
			return 0, false
		}
		return uint32(n), true
	}
	return 0, false
}

func makeOpcodeValue(kind uint8, raw uint64) any {
	typ := opcodeTypes[int(kind)%len(opcodeTypes)]
	rv := reflect.New(typ).Elem()
	switch typ.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		rv.SetInt(int64(raw) >> (64 - typ.Bits())) // sign-extended, full range of the type
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		rv.SetUint(raw >> (64 - typ.Bits()))
	case reflect.Float32, reflect.Float64:
		rv.SetFloat(float64(raw % 1000)) // integral floats are still not integers
	case reflect.String:
		rv.SetString(fmt.Sprint(raw))
	case reflect.Bool:
		rv.SetBool(raw&1 == 1)
	case reflect.Pointer:
		v := uint32(raw)
		rv.Set(reflect.ValueOf(&v))
	case reflect.Complex128:
		rv.SetComplex(complex(float64(raw%7), 0))
	}
	return rv.Interface()
}

// FuzzOpcodeFromContext: every integer kind within [0, MaxUint32] is accepted
// exactly; everything else is refused, never truncated.
func FuzzOpcodeFromContext(f *testing.F) {
	for k := range len(opcodeTypes) {
		for _, v := range []uint64{0, 1, 0xFFFFFFFF, 1 << 32, math.MaxUint64, 1 << 63} {
			f.Add(uint8(k), v, uint32(0))
		}
	}
	f.Fuzz(func(t *testing.T, kind uint8, raw uint64, def uint32) {
		v := makeOpcodeValue(kind, raw)
		got, err := opcodeFromContext(v)
		want, ok := opcodeOracle(reflect.ValueOf(v))
		if ok != (err == nil) || (ok && got != want) {
			t.Fatalf("opcodeFromContext(%T %v) = %d, %v; want %d ok=%v", v, v, got, err, want, ok)
		}
		if err != nil && got != 0 {
			t.Fatalf("refused %T %v but returned opcode %d", v, v, got)
		}

		// Through extractCallHeader: a refused opcode is an error, not the
		// handle default and not opcode 0.
		ctx := context.WithValue(context.Background(), OpcodeContextKey, v)
		h, herr := extractCallHeader(ctx, ffi.FlagInlineCompletion, def)
		if ok {
			if herr != nil || h.Reserved != want {
				t.Fatalf("header for %T %v: reserved %d err %v, want %d", v, v, h.Reserved, herr, want)
			}
		} else if herr == nil {
			t.Fatalf("header for %T %v accepted, reserved %d", v, v, h.Reserved)
		}
		if h.Flags != ffi.FlagInlineCompletion {
			t.Fatalf("flags %#x changed", h.Flags)
		}
	})
}

// panicCarrier panics from TraceID or SpanID with a fuzz-chosen value.
type panicCarrier struct {
	where int // 0: TraceID, 1: SpanID, 2: neither
	val   any
	trace [16]byte
	span  [8]byte
}

func (c *panicCarrier) TraceID() [16]byte {
	if c.where == 0 {
		panic(c.val)
	}
	return c.trace
}

func (c *panicCarrier) SpanID() [8]byte {
	if c.where == 1 {
		panic(c.val)
	}
	return c.span
}

// errorPanics is a panic value whose Error method itself panics, so formatting
// the recovered value is a second panic inside the recover path.
type errorPanics struct{}

func (errorPanics) Error() string { panic("Error() panicked") }

type stringerPanics struct{}

func (stringerPanics) String() string { panic(errors.New("String() panicked")) }

func panicValue(sel uint8, s string) any {
	switch sel % 9 {
	case 0:
		return s
	case 1:
		return len(s)
	case 2:
		return errors.New(s)
	case 3:
		return errorPanics{}
	case 4:
		return stringerPanics{}
	case 5:
		return nil // Go >= 1.21: *runtime.PanicNilError, recover() is non-nil
	case 6:
		return &errorPanics{}
	case 7:
		return []byte(s)
	default:
		return fmt.Errorf("wrapped: %w", context.Canceled)
	}
}

// FuzzReadTraceCarrier: a carrier that panics, with any panic value, becomes
// an error from extractCallHeader and never escapes; a well-behaved carrier's
// ids are copied exactly; a typed-nil carrier is an error, not a crash.
func FuzzReadTraceCarrier(f *testing.F) {
	for w := range 3 {
		for sel := range 9 {
			f.Add(uint8(w), uint8(sel), "boom", []byte{1, 2, 3}, uint32(7))
		}
	}
	f.Add(uint8(3), uint8(0), "", []byte{}, uint32(0)) // typed-nil carrier

	f.Fuzz(func(t *testing.T, where, sel uint8, msg string, ids []byte, op uint32) {
		var c *panicCarrier
		if where%4 != 3 {
			c = &panicCarrier{where: int(where % 4), val: panicValue(sel, msg)}
			copy(c.trace[:], ids)
			if len(ids) > 16 {
				copy(c.span[:], ids[16:])
			}
		}
		ctx := context.WithValue(context.Background(), SpanContextKey, TraceCarrier(c))
		ctx = ContextWithOpcode(ctx, op)
		var h ffi.CallHeader
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic escaped extractCallHeader: %v", r)
				}
			}()
			h, err = extractCallHeader(ctx, 0, 0)
		}()
		switch {
		case c == nil || c.where < 2:
			if err == nil || !strings.Contains(err.Error(), "trace carrier") {
				t.Fatalf("panicking carrier: err %v", err)
			}
		default:
			if err != nil {
				t.Fatalf("well-behaved carrier: %v", err)
			}
			if h.TraceID != c.trace || h.SpanID != c.span || h.Reserved != op {
				t.Fatalf("header %+v does not carry trace %x span %x op %d", h, c.trace, c.span, op)
			}
		}
	})
}

// FuzzExtractCallHeaderDeadline: a live deadline becomes a positive relative
// timeout no longer than what remains; an expired or cancelled context is
// timeout 1 (expire at once), never 0 (no deadline, I3).
func FuzzExtractCallHeaderDeadline(f *testing.F) {
	f.Add(int64(0), false)
	f.Add(int64(-1), false)
	f.Add(int64(1), false)
	f.Add(int64(math.MaxInt64), false)
	f.Add(int64(math.MinInt64), false)
	f.Add(int64(int64(time.Hour)), true)
	f.Fuzz(func(t *testing.T, offset int64, cancel bool) {
		base := context.Background()
		before := time.Now()
		ctx, stop := context.WithDeadline(base, before.Add(time.Duration(offset)))
		defer stop()
		if cancel {
			stop()
		}
		h, err := extractCallHeader(ctx, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if h.TimeoutNS == 0 {
			t.Fatalf("deadline offset %d (cancel=%v) produced timeout 0 = no deadline", offset, cancel)
		}
		if ctx.Err() != nil && h.TimeoutNS != 1 {
			t.Fatalf("done context produced timeout %d, want 1", h.TimeoutNS)
		}
		if dl, _ := ctx.Deadline(); ctx.Err() == nil && h.TimeoutNS > uint64(dl.Sub(before)) {
			t.Fatalf("timeout %d exceeds the %v remaining", h.TimeoutNS, dl.Sub(before))
		}
	})
}

// extractCallHeader is the header submitInput builds, composed the way it
// now builds it: ids and opcode before the permit (callHeaderIDs, so a
// carrier runs while no permit is held), the relative timeout after it
// (stampTimeout). The targets above were written against the single
// function this replaced; they check the composed result.
func extractCallHeader(ctx context.Context, flags, defaultOpcode uint32) (ffi.CallHeader, error) {
	h, err := callHeaderIDs(ctx, flags, defaultOpcode)
	if err != nil {
		return h, err
	}
	stampTimeout(ctx, &h)
	return h, nil
}
