package gusset

// Byte-level fuzzing of the completion-record reader (ticketReader) that sits
// on the Go side of the Rust completion pipe and ring (gusset.h,
// GUSSET_FLAG_INLINE_COMPLETION, GUSSET_RING_*).
//
// Every completion the runtime ever hands a waiter passes through this
// parser, so a record it mis-frames is a result handed to the wrong ticket, a
// waiter parked forever, or a permit never returned (I4). The targets:
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
//
// Default `go test` runs only the seed corpus; run each with
// `go test -run '^$' -fuzz '^FuzzTicketReaderPipeSplitReads$' -fuzztime 5m .`

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
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
