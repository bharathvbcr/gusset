package gusset

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// ticketReaderSpin is how long the drain reader polls the pipe after a
// completion before parking in the netpoller.
//
// Parking hands the wake-up to epoll, and on virtualized hosts waking an idle
// thread costs tens of microseconds — the same cost the Rust workers avoid by
// polling their queue (pool::queue). A caller that submits again right after
// its result lands gets its next completion read by a goroutine that is still
// running. The budget restarts only when a ticket arrives, so an idle handle
// polls once and then parks: no CPU at rest.
const ticketReaderSpin = 50 * time.Microsecond

// ticketReaderSpinBusy is the longer poll used while exactly one submitted job
// is in flight, when that completion is the next thing to happen. A serial caller running
// ~100 µs jobs outlasted the 50 µs window and paid the wake cost on every call
// (~58 µs of overhead against raw cgo). Capped: a job longer than this parks
// the reader as before, so a long-running engine never costs a busy core.
//
// It is the ceiling, not a fixed window: see ticketReader.gapEWMA.
const ticketReaderSpinBusy = 200 * time.Microsecond

// ticketReader reads completion records, several per system call.
//
// It used to be one io.ReadFull of 8 bytes per ticket, which under parallel
// load is one read syscall and one netpoller round per completion; a burst of
// completions now drains in a single read.
type ticketReader struct {
	f     *os.File
	rc    syscall.RawConn
	buf   [512]byte
	start int
	end   int
	// lastTicket is when the previous ticket was consumed.
	lastTicket time.Time
	// gapEWMA tracks the time between consecutive completions (1/8 weight
	// per sample). A lone job's poll lasts twice this, between
	// ticketReaderSpin and ticketReaderSpinBusy. A flat 200 us made the
	// reader spin through every GC pause of a 33 us-per-call stream of 64 KiB
	// results, keeping its P from the GC's idle mark workers (+10%), while
	// ~115 us jobs need the full window to avoid a park per call.
	gapEWMA time.Duration
	// inFlight reports whether a completion is imminent enough to poll for.
	inFlight func() bool
	// State for readFn, which is built once: a closure created per read
	// escapes through the RawConn interface and cost three allocations per
	// call (3 -> 6 allocs/op on CallNoop).
	dst    []byte
	n      int
	rerr   error
	readFn func(uintptr) bool
	// readMode selects readOnce's behaviour on EAGAIN (see readSpin etc.).
	readMode int

	// Completion ring (gusset_handle_ring); ring.Owner is nil without one.
	ring ffi.Ring
	// head is the next ring position to consume; only this reader moves it.
	head uint64
	mask uint64
	// ringData holds the last inline result taken from the ring: the slot
	// is handed back to the producers before next returns.
	ringData [ffi.InlineResultMax]byte
	// overflowSeen counts records read from the pipe while a ring is attached,
	// to compare with the ring's overflow counter.
	overflowSeen uint64
	// tokensOwed counts wake tokens a worker has written, or is about to
	// write, and this reader has not read yet.
	tokensOwed int
}

// readOnce behaviours on EAGAIN.
const (
	readSpin = iota // poll the pipe for the spin budget, then park (no ring)
	readPark        // park at once: the ring was already polled
	readNow         // return with nothing: never block
)

// attachRing switches the reader to the shared-memory ring.
func (tr *ticketReader) attachRing(r ffi.Ring) {
	tr.ring = r
	tr.mask = r.Capacity - 1
}

func newTicketReader(f *os.File) *ticketReader {
	tr := &ticketReader{f: f}
	if rc, err := f.SyscallConn(); err == nil {
		tr.rc = rc
	}
	tr.readFn = tr.readOnce
	return tr
}

// next returns the next completion: its ticket and, for an inline record
// (GUSSET_FLAG_INLINE_COMPLETION), the result bytes, which alias the reader's
// buffers and are valid only until the following call. inline is false for a
// bare ticket, whose outcome is collected with gusset_take.
func (tr *ticketReader) next() (ticket uint64, data []byte, inline bool, err error) {
	if tr.ring.Owner == nil {
		return tr.nextPipe()
	}
	for {
		if tr.ringReady() {
			return tr.popRing()
		}
		// Whole records already read from the pipe: overflow, or wake tokens.
		if t, d, in, size, err := tr.parseBuffered(); err != nil {
			return 0, nil, false, err
		} else if size > 0 {
			tr.start += size
			if t == 0 && !in {
				if tr.tokensOwed > 0 {
					tr.tokensOwed--
				}
				continue
			}
			tr.overflowSeen++
			tr.completed()
			return t, d, in, nil
		}
		// Bytes are in the pipe, or about to be: take what is there without
		// blocking. Tokens must not pile up unread, or the pipe would fill.
		if tr.overflowPending() || tr.tokensOwed > 0 {
			before := tr.end
			if err := tr.fillMode(readNow); err != nil {
				return 0, nil, false, err
			}
			if tr.end != before {
				continue
			}
		}
		if err := tr.waitRing(); err != nil {
			return 0, nil, false, err
		}
	}
}

// ringSlot is the slot at the reader's position.
func (tr *ticketReader) ringSlot() unsafe.Pointer {
	return unsafe.Add(tr.ring.Slots, uintptr(tr.head&tr.mask)*ffi.RingSlotBytes)
}

// ringReady reports whether the slot at the reader's position is published.
func (tr *ticketReader) ringReady() bool {
	return atomic.LoadUint64((*uint64)(tr.ringSlot())) == tr.head+1
}

// popRing takes the published slot at the reader's position and hands it
// back to the producers.
func (tr *ticketReader) popRing() (uint64, []byte, bool, error) {
	slot := tr.ringSlot()
	rec := unsafe.Add(slot, ffi.RingSlotOffRecord)
	w := atomic.LoadUint64((*uint64)(rec))
	var data []byte
	inline := w&ffi.InlineRecordFlag != 0
	if inline {
		n := atomic.LoadUint64((*uint64)(unsafe.Add(rec, 8)))
		if n > uint64(ffi.InlineResultMax) {
			return 0, nil, false, fmt.Errorf("gusset: corrupt ring record (length %d)", n)
		}
		// Ordered after the acquire load of the sequence in ringReady.
		copy(tr.ringData[:n], unsafe.Slice((*byte)(unsafe.Add(rec, 16)), n))
		data = tr.ringData[:n]
		w &^= ffi.InlineRecordFlag
	}
	atomic.StoreUint64((*uint64)(slot), tr.head+tr.ring.Capacity)
	tr.head++
	tr.completed()
	return w, data, inline, nil
}

// overflowPending reports records the workers sent through the pipe that
// this reader has not read yet.
func (tr *ticketReader) overflowPending() bool {
	ov := atomic.LoadUint64((*uint64)(unsafe.Add(tr.ring.Shared, ffi.RingOffOverflow)))
	return int64(ov-tr.overflowSeen) > 0
}

// waitRing polls the ring until something arrives or the spin budget runs
// out, then parks on the pipe behind the waiting flag (gusset.h).
func (tr *ticketReader) waitRing() error {
	if !tr.lastTicket.IsZero() {
		budget := tr.spinBudget()
		for i := 0; ; i++ {
			if tr.ringReady() || tr.overflowPending() {
				return nil
			}
			if i&7 == 7 && time.Since(tr.lastTicket) >= budget {
				break
			}
			// The caller that submitted is usually runnable on this P;
			// yielding lets it run, and costs no system call.
			runtime.Gosched()
		}
	}
	waiting := (*uint32)(unsafe.Add(tr.ring.Shared, ffi.RingOffWaiting))
	atomic.StoreUint32(waiting, 1)
	var err error
	if !tr.ringReady() && !tr.overflowPending() {
		err = tr.fillMode(readPark)
	}
	// A 0 here means a worker took the flag and owes a token (possibly
	// already read into the buffer, which then pays it off).
	if atomic.SwapUint32(waiting, 0) == 0 {
		tr.tokensOwed++
	}
	return err
}

// parseBuffered parses one whole record from bytes already read, without
// reading more. size is 0 when no whole record is buffered.
func (tr *ticketReader) parseBuffered() (ticket uint64, data []byte, inline bool, size int, err error) {
	b := tr.buf[tr.start:tr.end]
	if len(b) < 8 {
		return 0, nil, false, 0, nil
	}
	w := binary.NativeEndian.Uint64(b)
	if w&ffi.InlineRecordFlag == 0 {
		return w, nil, false, 8, nil
	}
	if len(b) < 16 {
		return 0, nil, false, 0, nil
	}
	n := binary.NativeEndian.Uint64(b[8:16])
	if n > uint64(ffi.InlineResultMax) {
		// Rust never writes this; a stream that does is not ours to parse.
		return 0, nil, false, 0, fmt.Errorf("gusset: corrupt completion record (length %d)", n)
	}
	size = 16 + (int(n)+7)&^7
	if len(b) < size {
		return 0, nil, false, 0, nil
	}
	return w &^ ffi.InlineRecordFlag, b[16 : 16+int(n)], true, size, nil
}

// fillMode reads more pipe bytes after the buffered ones, with the given
// EAGAIN behaviour. A record is one atomic write under PIPE_BUF, but a read
// may still end partway through one when the buffer fills, so the unread tail
// moves to the front and the next read completes it.
func (tr *ticketReader) fillMode(mode int) error {
	if tr.start > 0 {
		copy(tr.buf[:], tr.buf[tr.start:tr.end])
		tr.end -= tr.start
		tr.start = 0
	}
	tr.readMode = mode
	n, err := tr.fill(tr.buf[tr.end:])
	tr.readMode = readSpin
	tr.end += n
	return err
}

// nextPipe is next without a ring: every completion comes through the pipe.
//
// A read error is returned only once the bytes read before it hold no whole
// record, so a record that arrived with the error is still delivered.
func (tr *ticketReader) nextPipe() (ticket uint64, data []byte, inline bool, err error) {
	var readErr error
	for {
		t, d, in, size, err := tr.parseBuffered()
		if err != nil {
			return 0, nil, false, err
		}
		if size > 0 {
			tr.start += size
			tr.completed()
			return t, d, in, nil
		}
		if readErr != nil {
			return 0, nil, false, readErr
		}
		readErr = tr.fillMode(readSpin)
	}
}

// fill reads what is available, polling briefly before letting the netpoller
// park the goroutine. Returns io.EOF when every write end is closed.
func (tr *ticketReader) fill(p []byte) (int, error) {
	if tr.rc == nil {
		return io.ReadAtLeast(tr.f, p, 1)
	}
	tr.dst, tr.n, tr.rerr = p, 0, nil
	err := tr.rc.Read(tr.readFn)
	tr.dst = nil
	if err != nil {
		return tr.n, err // deadline set by close, or the file was closed
	}
	return tr.n, tr.rerr
}

func (tr *ticketReader) spinBudget() time.Duration {
	if tr.inFlight != nil && tr.inFlight() {
		return min(max(2*tr.gapEWMA, ticketReaderSpin), ticketReaderSpinBusy)
	}
	return ticketReaderSpin
}

// completed stamps a consumed completion and folds the gap since the last
// one into gapEWMA. Gaps past the busy ceiling are idle time, not job
// length, and are left out.
func (tr *ticketReader) completed() {
	now := time.Now()
	if !tr.lastTicket.IsZero() {
		if gap := now.Sub(tr.lastTicket); gap < 4*ticketReaderSpinBusy {
			tr.gapEWMA += (gap - tr.gapEWMA) / 8
		}
	}
	tr.lastTicket = now
}

// readOnce is the RawConn.Read callback: true when done, false to park.
func (tr *ticketReader) readOnce(fd uintptr) bool {
	for {
		m, e := syscall.Read(int(fd), tr.dst)
		switch {
		case m > 0:
			tr.n = m
			return true
		case e == nil && m == 0:
			tr.rerr = io.EOF
			return true
		case e == syscall.EINTR:
			continue
		case e == syscall.EAGAIN:
			switch tr.readMode {
			case readNow:
				return true // tr.n stays 0
			case readPark:
				return false
			}
			if !tr.lastTicket.IsZero() && time.Since(tr.lastTicket) < tr.spinBudget() {
				runtime.Gosched()
				continue
			}
			return false // park until readable
		default:
			tr.rerr = e
			return true
		}
	}
}
