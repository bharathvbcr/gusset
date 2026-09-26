package gusset

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// callResult is one completion moving from drainPipe to its waiter.
//
// It carries no *Buffer. An earlier design wrapped every large take() result in
// one here; takeIDs replaced that, and this struct is deliberately kept as small
// as it was then — a uint64 added to it measured +10% B/op on CallParallel,
// because a one-byte Call pays for the shape of the largest result.
type callResult struct {
	data []byte
	err  error
}

// Option configures Handle settings.
type Option func(*handleConfig)

type handleConfig struct {
	poolSize        uint32
	poolSizeInvalid bool
	callFlags       uint32
	// pipeOnly skips the shared-memory completion ring, so every completion
	// crosses the pipe. Unexported: it exists for tests and A/B benchmarks.
	pipeOnly      bool
	defaultOpcode uint32
	bufferBudget  int64
}

// MaxPoolSize mirrors gusset::pool::MAX_POOL_SIZE.
//
// Each worker is an OS thread with an 8 MiB stack, so the pool size is a bounded
// resource request, not a free dial. Rust refuses anything larger; this constant
// lets callers check before they ask.
const MaxPoolSize = 1024

// WithPoolSize sets the worker thread pool size for this handle.
//
// Values above MaxPoolSize are not silently clamped: Open returns an error, because
// a caller who asked for 10,000 workers and quietly received 1024 would keep the
// wrong capacity model.
func WithPoolSize(n int) Option {
	return func(c *handleConfig) {
		if n < 1 || n > MaxPoolSize {
			c.poolSizeInvalid = true
			return
		}
		c.poolSizeInvalid = false
		c.poolSize = uint32(n)
	}
}

// withPipeOnly keeps every completion on the pipe (no completion ring).
func withPipeOnly() Option {
	return func(c *handleConfig) { c.pipeOnly = true }
}

// WithDiagnosticEngine routes this handle's calls to Gusset's built-in diagnostic
// engine when no adopter engine is registered in Rust.
//
// The diagnostic engine selects its behaviour from the first input byte, including
// several deliberate panics, so it must never see untrusted data. Gusset's own panic
// zoo and pitfall suite use it; production callers must not. Without this option a
// handle with no registered engine refuses every submission instead of falling back
// to an implicit echo-or-panic engine.
func WithDiagnosticEngine() Option {
	return func(c *handleConfig) {
		c.callFlags |= ffi.FlagDiagnosticEngine
	}
}

// WithBufferBudget caps the bytes of live buffers allocated with NewBuffer on
// this handle; NewBuffer beyond it returns ErrBufferBudget. 0 (the default)
// means unlimited.
//
// Rust memory is invisible to Go's GC pacer and each *Buffer is a tiny Go
// object, so a caller that loops on NewBuffer without Free can reach an OOM
// long before any GC runs the AddCleanup backstop. The budget turns that into
// an error at the allocation that crossed it. Result buffers (WaitBuffer,
// CallBuffer) are not charged: refusing them would lose finished work.
func WithBufferBudget(bytes int64) Option {
	return func(c *handleConfig) {
		if bytes < 0 {
			bytes = 0
		}
		c.bufferBudget = bytes
	}
}

// WithOpcode sets the default engine dispatch opcode for this handle (R9).
// Dispatches to an engine registered with that opcode in Rust without payload byte mangling.
func WithOpcode(opcode uint32) Option {
	return func(c *handleConfig) {
		c.defaultOpcode = opcode
	}
}

// handleState owns the active Gusset runtime session resources (I4).
// Keeping state decoupled from Handle ensures runtime.AddCleanup on Handle
// can collect unreachable handles without a reference cycle with the drainPipe goroutine.
type handleState struct {
	ptr           unsafe.Pointer
	callFlags     uint32
	defaultOpcode uint32
	sem           chan struct{}
	// ring is the attached completion ring, if any (Owner nil when not).
	// drainPipe releases it once it has stopped reading.
	ring      ffi.Ring
	poisoned  atomic.Bool
	closed    atomic.Bool
	pipe      *os.File
	drainDone chan struct{}
	// bufBudget caps live NewBuffer bytes (0 = unlimited); bufBytes is the
	// current charge. See WithBufferBudget.
	bufBudget int64
	bufBytes  atomic.Int64
	// closeDone is closed once close has fully released the handle, so a
	// second concurrent Close waits for the first rather than returning while
	// workers are still being joined. closeErr is written before closeDone
	// closes, so every caller reports the same outcome.
	closeDone chan struct{}
	closeErr  error
	// drainExited is set, under mu, when drainPipe stops reading. After that
	// no completion can ever be delivered, so a new submission or waiter is
	// refused instead of parking forever.
	drainExited atomic.Bool
	mu          sync.Mutex
	cgoMu       sync.RWMutex
	pending     map[uint64]chan callResult
	completed   map[uint64]callResult
	semTickets  map[uint64]struct{}
	// abandoned holds tickets whose owner was released by its context before
	// the engine finished. The permit stays in semTickets — the worker is still
	// busy — and deliver reclaims both the result and the permit when the
	// completion finally lands.
	abandoned map[uint64]struct{}
	// takeIDs holds Rust buffer ids for large take() results until a waiter
	// consumes them. Kept off callResult so the completion channel stays the
	// same size as HEAD (a uint64 on that struct was +10% B/op on CallParallel).
	takeIDs map[uint64]uint64
}

// inlineResultBytes is the egress twin of the 4 KiB submit copy limit (R16).
// Results this size and under are copied onto the Go heap in drainPipe so a
// one-byte Call does not pay a Buffer wrapper and AddCleanup. Larger results
// keep take()'s Rust buffer id until a waiter consumes it: WaitBuffer wraps
// with no Go copy, Wait copies out and frees. Wrapping in drainPipe taxed
// Wait with a Buffer object it immediately destroyed (6 allocs/op vs 3).
const inlineResultBytes = 4096

// Handle represents an active Gusset runtime session (I4).
type Handle struct {
	state   *handleState
	cleanup runtime.Cleanup
}

// Open opens a new Gusset handle with bounded concurrency (I4).
func Open(opts ...Option) (*Handle, error) {
	cfg := handleConfig{poolSize: 4}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.poolSizeInvalid || cfg.poolSize > MaxPoolSize {
		return nil, errors.New("gusset: pool_size exceeds maximum 1024 (each worker is an OS thread with an 8 MiB stack)")
	}

	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	// Duplicate write descriptor so Rust owns an independent OS descriptor.
	//
	// Dup does not carry close-on-exec over, and a child process that inherits
	// the write end keeps the pipe open after Rust closes its copy: drainPipe
	// never sees EOF and Close blocks until that child exits. ForkLock stops a
	// concurrent exec from forking between the dup and the flag.
	syscall.ForkLock.RLock()
	writeFD, err := syscall.Dup(int(w.Fd()))
	if err == nil {
		syscall.CloseOnExec(writeFD)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, err
	}
	_ = w.Close()
	_ = syscall.SetNonblock(writeFD, true)

	hPtr, err := ffi.HandleOpen(cfg.poolSize, writeFD)
	if err != nil {
		_ = r.Close()
		_ = syscall.Close(writeFD)
		return nil, err
	}

	// Completions go through shared memory; the pipe only wakes a parked
	// reader. The ring reader needs non-blocking reads through the RawConn,
	// so without one it stays on the pipe.
	var ring ffi.Ring
	if _, rcErr := r.SyscallConn(); rcErr == nil && !cfg.pipeOnly {
		ring, err = ffi.HandleRing(hPtr)
		if err != nil {
			_ = ffi.HandleClose(hPtr) // owns and closes writeFD
			_ = r.Close()
			return nil, err
		}
	}

	state := &handleState{
		ptr:           hPtr,
		callFlags:     cfg.callFlags | ffi.FlagInlineCompletion,
		defaultOpcode: cfg.defaultOpcode,
		bufBudget:     cfg.bufferBudget,
		sem:           make(chan struct{}, cfg.poolSize),
		ring:          ring,
		pipe:          r,
		drainDone:     make(chan struct{}),
		closeDone:     make(chan struct{}),
		pending:       make(map[uint64]chan callResult),
		completed:     make(map[uint64]callResult),
		semTickets:    make(map[uint64]struct{}),
		abandoned:     make(map[uint64]struct{}),
		takeIDs:       make(map[uint64]uint64),
	}

	h := &Handle{state: state}

	// Register AddCleanup backstop (logs if app forgot to close).
	// Because drainPipe receives state and not h, h can be garbage collected
	// if the application drops all references to it without calling Close().
	//
	// close runs on its own goroutine. The runtime runs the cleanups of one
	// queued block one after another, and close joins worker threads —
	// unbounded for an engine that never calls JobContext::check — so running it
	// inline stalled every cleanup queued behind it, Gusset's own Buffer
	// backstops included.
	h.cleanup = runtime.AddCleanup(h, func(s *handleState) {
		slog.Warn("gusset: handle was garbage collected without explicit Close()")
		go func() { _ = s.close() }()
	}, state)

	// Start pipe reader goroutine (parks on netpoller)
	go drainPipe(state)

	return h, nil
}

// drainPipe reads completion records from the pipe and routes each result.
func drainPipe(s *handleState) {
	defer close(s.drainDone)
	tr := newTicketReader(s.pipe)
	if s.ring.Owner != nil {
		tr.attachRing(s.ring)
		// Runs before drainDone closes: close waits on drainDone, so by the
		// time it returns nothing reads the ring any more.
		defer ffi.RingRelease(s.ring.Owner)
	}
	// Only a lone in-flight job earns the longer poll: under parallel load a
	// completion is always pending, and polling between them took a core the
	// workers needed (1 ms parallel jobs +18% on 4 vCPUs).
	tr.inFlight = func() bool { return len(s.sem) == 1 }

	for {
		ticket, inlineData, inline, err := tr.next()
		if err != nil {
			// Pipe closed on handle shutdown or EOF
			s.mu.Lock()
			for _, ch := range s.pending {
				ch <- callResult{err: errors.New("gusset: handle closed")}
			}
			toFree := make([]uint64, 0, len(s.takeIDs))
			for _, id := range s.takeIDs {
				toFree = append(toFree, id)
			}
			s.pending = make(map[uint64]chan callResult)
			s.completed = make(map[uint64]callResult)
			s.drainExited.Store(true)
			s.takeIDs = make(map[uint64]uint64)
			// Permits held by abandoned tickets are returned by close, which
			// drains every entry in semTickets. Clearing the set here only stops
			// a late completion from being treated as abandoned after the drain
			// reader has already given up on the pipe.
			s.abandoned = make(map[uint64]struct{})
			s.mu.Unlock()

			for _, id := range toFree {
				_ = s.bufFree(id)
			}
			return
		}

		// A small success arrived whole in its record: no gusset_take, no
		// registry buffer, no gusset_buf_free, and no cgoMu. The bytes alias
		// the reader's buffer, so they are copied out before the next read.
		if inline {
			var out []byte
			if len(inlineData) > 0 {
				out = make([]byte, len(inlineData))
				copy(out, inlineData)
			}
			s.deliver(ticket, callResult{data: out}, 0)
			continue
		}

		// Synchronize with handle close to eliminate UAF on s.ptr, without
		// ever blocking. This goroutine used to wait on cgoMu.RLock behind
		// Close's exclusive lock, so for the whole worker join nobody read the
		// pipe. On a small pipe (macOS falls back to 512 bytes, 64 tickets,
		// under memory pressure) workers then sat in the 10 s write backoff and
		// Close took that long. During a close every result is discarded
		// anyway, so keep reading and hand each waiter "closed".
		if !s.enterCgo() {
			s.deliver(ticket, callResult{err: errors.New("gusset: handle closed")}, 0)
			continue
		}

		// Retrieve result from Rust
		bufID, outBytes, takeErr := ffi.Take(s.ptr, ticket)
		var res callResult
		var takeID uint64
		switch {
		case takeErr != nil:
			if errors.Is(takeErr, ErrPanic) {
				s.poisoned.Store(true)
			}
			res = callResult{err: takeErr}
		case (bufID&ffi.TakeOwnedFlag) != 0 || (bufID > 0 && len(outBytes) > inlineResultBytes):
			// Keep take()'s Rust buffer until a waiter consumes it. WaitBuffer
			// wraps with no Go copy; Wait copies out and frees. Wrapping here
			// forced every Wait of a large result to allocate a *Buffer it
			// immediately destroyed. The id lives in takeIDs, not callResult,
			// so a one-byte Call does not pay a larger completion object.
			res = callResult{data: outBytes}
			takeID = bufID &^ ffi.TakeOwnedFlag
		default:
			var out []byte
			if len(outBytes) > 0 {
				out = make([]byte, len(outBytes))
				copy(out, outBytes)
			}
			if bufID > 0 {
				_ = ffi.BufFree(s.ptr, bufID)
			}
			res = callResult{data: out}
		}
		s.cgoMu.RUnlock()

		s.deliver(ticket, res, takeID)
	}
}

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
			tr.lastTicket = time.Now()
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
	tr.lastTicket = time.Now()
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
		return 0, nil, false, 0, fmt.Errorf("gusset: corrupt completion record (length %d)", n)
	}
	size = 16 + (int(n)+7)&^7
	if len(b) < size {
		return 0, nil, false, 0, nil
	}
	return w &^ ffi.InlineRecordFlag, b[16 : 16+int(n)], true, size, nil
}

// fillMode reads more pipe bytes after the buffered ones, with the given
// EAGAIN behaviour.
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
func (tr *ticketReader) nextPipe() (ticket uint64, data []byte, inline bool, err error) {
	if err := tr.need(8); err != nil {
		return 0, nil, false, err
	}
	w := binary.NativeEndian.Uint64(tr.buf[tr.start : tr.start+8])
	if w&ffi.InlineRecordFlag == 0 {
		tr.start += 8
		tr.lastTicket = time.Now()
		return w, nil, false, nil
	}
	if err := tr.need(16); err != nil {
		return 0, nil, false, err
	}
	n := binary.NativeEndian.Uint64(tr.buf[tr.start+8 : tr.start+16])
	if n > uint64(ffi.InlineResultMax) {
		// Rust never writes this; a stream that does is not ours to parse.
		return 0, nil, false, fmt.Errorf("gusset: corrupt completion record (length %d)", n)
	}
	size := 16 + (int(n)+7)&^7
	if err := tr.need(size); err != nil {
		return 0, nil, false, err
	}
	data = tr.buf[tr.start+16 : tr.start+16+int(n)]
	tr.start += size
	tr.lastTicket = time.Now()
	return w &^ ffi.InlineRecordFlag, data, true, nil
}

// need buffers at least k unread bytes (k <= len(buf)). A record is one
// atomic write under PIPE_BUF, but a read may still end partway through one
// when the buffer fills, so the tail is kept and completed by the next read.
func (tr *ticketReader) need(k int) error {
	for tr.end-tr.start < k {
		if tr.start > 0 {
			copy(tr.buf[:], tr.buf[tr.start:tr.end])
			tr.end -= tr.start
			tr.start = 0
		}
		n, err := tr.fill(tr.buf[tr.end:])
		tr.end += n
		if err != nil && tr.end-tr.start < k {
			return err
		}
	}
	return nil
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
		return ticketReaderSpinBusy
	}
	return ticketReaderSpin
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

// deliver routes one completion: to its waiter, to the completed map for a
// waiter yet to arrive, or straight to the bin when the owner has abandoned the
// ticket.
//
// Abandonment is the only path that also returns the pool permit. A waiter that
// gave up on its deadline deliberately left the permit behind, because the
// worker was still executing; this is the moment the work actually stops, so
// this is the moment the permit is free (I4).
func (s *handleState) deliver(ticket uint64, res callResult, takeID uint64) {
	s.mu.Lock()
	if _, gone := s.abandoned[ticket]; gone {
		delete(s.abandoned, ticket)
		heldPermit := false
		if _, held := s.semTickets[ticket]; held {
			delete(s.semTickets, ticket)
			heldPermit = true
		}
		s.mu.Unlock()

		// Poison is not latched here. drainPipe latches it off the take error
		// before it calls deliver, so it is already set for an abandoned panic
		// as much as for a collected one (I2). A second store here read as a
		// safety net but was unkillable by any mutation, which is how a line
		// that guards nothing comes to look like a line that guards something.
		s.discardTake(takeID)
		if heldPermit {
			// Never blocks: the permit for this ticket is held by definition.
			<-s.sem
		}
		return
	}

	if takeID != 0 {
		s.takeIDs[ticket] = takeID
	}
	if ch, exists := s.pending[ticket]; exists {
		delete(s.pending, ticket)
		ch <- res
	} else {
		s.completed[ticket] = res
	}
	s.mu.Unlock()
}

// Close gracefully cancels pending work, shuts down the pool, and releases resources.
func (h *Handle) Close() error {
	if h == nil || h.state == nil {
		return errors.New("gusset: handle is nil")
	}
	h.cleanup.Stop()
	err := h.state.close()
	runtime.KeepAlive(h)
	return err
}

func (s *handleState) close() error {
	if s.closed.Swap(true) {
		<-s.closeDone
		return s.closeErr
	}
	var err error
	defer func() {
		s.closeErr = err
		close(s.closeDone)
	}()

	// 1. Cancel in-flight jobs in Rust memory (R9, I3)
	s.cgoMu.RLock()
	if s.ptr != nil {
		_ = ffi.CancelAll(s.ptr)
	}
	s.cgoMu.RUnlock()

	// 2. Wait for all active CGO operations to finish before deallocating handle
	s.cgoMu.Lock()
	err = ffi.HandleClose(s.ptr)
	s.ptr = nil
	s.cgoMu.Unlock()

	// 3. Stop the drain reader. It used to wait for EOF, which needs every
	// copy of the write end closed: a child forked outside Go's ForkLock (from
	// Rust or C) that inherited it, or a panic inside gusset_handle_close
	// before it closed the descriptor, left Close — and every concurrent
	// Close and every Submit parked on a permit — blocked forever. The workers
	// are joined by now and any ticket still unread would be answered
	// "closed" anyway, so a read deadline ends the reader either way.
	_ = s.pipe.SetReadDeadline(time.Now())
	<-s.drainDone
	_ = s.pipe.Close()

	// 4. Drain any remaining permits from untaken submitted tickets and free unconsumed buffers.
	// semTickets covers abandoned tickets too: their permits were deliberately
	// left with the work, and the work is over now that the pool has joined.
	s.mu.Lock()
	toFree := make([]uint64, 0, len(s.takeIDs))
	for _, id := range s.takeIDs {
		toFree = append(toFree, id)
	}
	s.completed = make(map[uint64]callResult)
	s.takeIDs = make(map[uint64]uint64)
	s.abandoned = make(map[uint64]struct{})
	for ticket := range s.semTickets {
		delete(s.semTickets, ticket)
		<-s.sem
	}
	s.mu.Unlock()

	for _, id := range toFree {
		_ = s.bufFree(id)
	}

	return err
}

// shutdownCause reports a cancellation caused by Shutdown as ErrShutdown.
//
// Shutdown cancels every job through the same flag a caller's cancel uses, and
// an engine reports it in its own words ("cancelled: Explicit"), which matched
// context.Canceled. A caller whose context is still live did not cancel
// anything; telling it so would send it retrying work the runtime is refusing.
func shutdownCause(ctx context.Context, err error) error {
	if shutdownStarted.Load() && ctx.Err() == nil && errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w (%v)", ErrShutdown, err)
	}
	return err
}

// enterCgo takes cgoMu for reading without ever blocking, and only while the
// handle is open. On true the caller holds the read lock and must RUnlock.
//
// Only close takes cgoMu exclusively, and it sets closed first. A reader that
// passed an earlier closed check and then called RLock parked behind close's
// unbounded worker join, ignoring its own context. TryRLock fails exactly when
// that writer is holding or waiting, which is exactly "closing".
func (s *handleState) enterCgo() bool {
	if s.closed.Load() || !s.cgoMu.TryRLock() {
		return false
	}
	if s.closed.Load() || s.ptr == nil {
		s.cgoMu.RUnlock()
		return false
	}
	return true
}

func (s *handleState) popTakeIDLocked(ticket uint64) uint64 {
	id := s.takeIDs[ticket]
	delete(s.takeIDs, ticket)
	return id
}

// discardTake releases a take() buffer nobody is going to consume.
func (s *handleState) discardTake(takeID uint64) {
	if takeID != 0 {
		_ = s.bufFree(takeID)
	}
}

func (s *handleState) releaseSem(ticket uint64) {
	s.mu.Lock()
	if _, ok := s.semTickets[ticket]; ok {
		delete(s.semTickets, ticket)
		<-s.sem
	}
	s.mu.Unlock()
}

// Call executes a unit of work synchronously within the caller's context deadline.
//
// Invariant: callers park on the Go semaphore, never blocking on an OS thread (I4).
func (h *Handle) Call(ctx context.Context, in []byte) ([]byte, error) {
	if h == nil || h.state == nil {
		return nil, errors.New("gusset: handle is nil")
	}
	if ctx == nil {
		return nil, errors.New("gusset: nil context")
	}
	res, err := h.state.call(ctx, in)
	runtime.KeepAlive(h)
	return res, err
}

func (s *handleState) call(ctx context.Context, in []byte) ([]byte, error) {
	ticket, err := s.submitInput(ctx, in, nil)
	if err != nil {
		return nil, err
	}
	return s.wait(ctx, ticket)
}

// CallBuffer is Call for a Rust-owned input buffer, returning a Rust-owned
// output buffer: the whole zero-copy round trip in one call.
//
// Call cannot express this. Its input is a []byte, and a []byte over 4 KiB is
// refused precisely because copying it would run on the cgo thread — so the
// payload sizes zero-copy exists for are exactly the ones Call rejects. Without
// this, every adopter following the zero-copy path hand-rolls Submit plus
// WaitBuffer, including the KeepAlive that stops the input Buffer's AddCleanup
// backstop from freeing Rust memory while Rust is still resolving its id.
//
// The caller owns both buffers. Free the input when the call returns, and the
// output when done with it; AddCleanup is a backstop on each, not a plan.
func (h *Handle) CallBuffer(ctx context.Context, in *Buffer) (*Buffer, error) {
	if h == nil || h.state == nil {
		return nil, errors.New("gusset: handle is nil")
	}
	if ctx == nil {
		return nil, errors.New("gusset: nil context")
	}
	if in == nil {
		return nil, errors.New("gusset: buffer is nil")
	}
	if in.state == nil {
		return nil, errors.New("gusset: buffer is not initialized")
	}
	defer runtime.KeepAlive(in)
	ticket, err := h.state.submitInput(ctx, nil, in)
	if err != nil {
		runtime.KeepAlive(h)
		return nil, err
	}
	out, err := h.state.waitBuffer(ctx, ticket)
	if out != nil {
		out.owner = h
	}
	runtime.KeepAlive(h)
	return out, err
}

// Submit submits a job asynchronously (accepts either []byte or *Buffer) and returns a ticket.
func (h *Handle) Submit(ctx context.Context, in any) (uint64, error) {
	if h == nil || h.state == nil {
		return 0, errors.New("gusset: handle is nil")
	}
	if ctx == nil {
		return 0, errors.New("gusset: nil context")
	}
	ticket, err := h.state.submit(ctx, in)
	runtime.KeepAlive(h)
	// A *Buffer argument is consumed for its id alone, so after that read nothing
	// references it and its AddCleanup backstop becomes eligible to run — freeing
	// the Rust buffer while, or before, Rust resolves the id. Keeping it alive until
	// submit has returned closes that window.
	runtime.KeepAlive(in)
	return ticket, err
}

// submit is the Submit(any) entry: it sorts the input by type and hands a
// typed pair to submitInput. Call and CallBuffer go to submitInput directly:
// passing a []byte through `any` boxed its slice header, one heap allocation
// on every Call (CallNoop 2 -> 3 allocs/op).
func (s *handleState) submit(ctx context.Context, in any) (uint64, error) {
	switch v := in.(type) {
	case []byte:
		return s.submitInput(ctx, v, nil)
	case *Buffer:
		if v == nil {
			return 0, errors.New("gusset: buffer is nil")
		}
		return s.submitInput(ctx, nil, v)
	case nil:
		return s.submitInput(ctx, nil, nil)
	default:
		return 0, errors.New("gusset: input must be []byte or *Buffer")
	}
}

// submitInput submits either inline bytes (buf == nil) or a Buffer.
func (s *handleState) submitInput(ctx context.Context, raw []byte, buf *Buffer) (uint64, error) {
	// Closed before poisoned: a poisoned handle that was then closed used to
	// answer ErrPoisoned, sending a caller whose policy is "on poison, close
	// and reopen" back to close a handle it had already closed.
	if s.closed.Load() || s.drainExited.Load() {
		return 0, errors.New("gusset: handle is closed")
	}
	if s.poisoned.Load() {
		return 0, errHandlePoisoned
	}

	var rawInput []byte
	var bufferID uint64

	if buf == nil {
		if len(raw) > inlineResultBytes {
			return 0, errors.New("gusset: []byte input exceeds 4096-byte copy limit; use NewBuffer")
		}
		rawInput = raw
	} else {
		v := buf
		if v.state == nil {
			return 0, errors.New("gusset: buffer is not initialized")
		}
		if v.freed.Load() || v.state.closed.Load() {
			return 0, errors.New("gusset: buffer is freed or closed")
		}
		if v.state != s {
			return 0, errors.New("gusset: buffer belongs to a different handle")
		}
		bufferID = v.id
		if bufferID == 0 {
			// A Go-heap result buffer (see waitBuffer): no Rust id to pass, so
			// its bytes travel inline. Id 0 used to mean "no input", which
			// silently ran the engine on nothing. Data and liveness are read
			// in a way a concurrent Free cannot tear: freed was checked above,
			// and an id-0 buffer's data is never written after construction
			// (Free leaves it; the memory is Go's). A Free racing this Submit
			// therefore sends the real bytes, never the nil that used to
			// arrive as empty input. No lock: any mutex on this path made
			// *Buffer escape, which boxed every Submit input on the heap
			// (SubmitWait 2 -> 3 allocs/op).
			data := v.data
			if len(data) > inlineResultBytes {
				return 0, errors.New("gusset: buffer without a Rust id exceeds the 4096-byte copy limit")
			}
			rawInput = data
		}
	}

	// Acquire semaphore slot
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	// In Go, select chooses pseudo-randomly when multiple channels are ready.
	// If the context is already cancelled or expired, fail fast and release permit.
	if err := ctx.Err(); err != nil {
		<-s.sem
		return 0, err
	}

	if s.closed.Load() {
		<-s.sem
		return 0, errors.New("gusset: handle is closed")
	}
	if s.poisoned.Load() {
		<-s.sem
		return 0, errHandlePoisoned
	}

	header, err := extractCallHeader(ctx, s.callFlags, s.defaultOpcode)
	if err != nil {
		<-s.sem
		return 0, err
	}
	if !s.enterCgo() {
		<-s.sem
		return 0, errors.New("gusset: handle is closed")
	}
	if s.poisoned.Load() {
		s.cgoMu.RUnlock()
		<-s.sem
		return 0, errHandlePoisoned
	}
	ticket, err := ffi.Submit(s.ptr, header, rawInput, bufferID)
	if err != nil {
		s.cgoMu.RUnlock()
		<-s.sem
		if errors.Is(err, ErrPanic) || errors.Is(err, ErrPoisoned) {
			s.poisoned.Store(true)
		}
		return 0, err
	}

	s.mu.Lock()
	s.semTickets[ticket] = struct{}{}
	s.mu.Unlock()
	s.cgoMu.RUnlock()

	return ticket, nil
}

// Wait waits for completion of an asynchronously submitted job ticket.
func (h *Handle) Wait(ctx context.Context, ticket uint64) ([]byte, error) {
	if h == nil || h.state == nil {
		return nil, errors.New("gusset: handle is nil")
	}
	if ctx == nil {
		return nil, errors.New("gusset: nil context")
	}
	res, err := h.state.wait(ctx, ticket)
	runtime.KeepAlive(h)
	return res, err
}

// WaitBuffer waits for completion of an asynchronously submitted job ticket,
// returning a zero-copy *Buffer backed by Rust-owned memory.
//
// If the job completed with inline bytes rather than an allocated buffer, a *Buffer
// is allocated to hold the data.
// The caller is responsible for calling buf.Free() when done, though runtime.AddCleanup
// acts as a safety net if the buffer is garbage collected.
func (h *Handle) WaitBuffer(ctx context.Context, ticket uint64) (*Buffer, error) {
	if h == nil || h.state == nil {
		return nil, errors.New("gusset: handle is nil")
	}
	if ctx == nil {
		return nil, errors.New("gusset: nil context")
	}
	buf, err := h.state.waitBuffer(ctx, ticket)
	if buf != nil {
		buf.owner = h
	}
	runtime.KeepAlive(h)
	return buf, err
}

// ErrUnknownTicket reports a ticket this handle is not waiting on: never submitted
// here, already awaited, or issued by a different handle.
var ErrUnknownTicket = errors.New("gusset: unknown or already-awaited ticket")

// ErrTicketBusy reports that another goroutine is already waiting on this ticket.
var ErrTicketBusy = errors.New("gusset: ticket already has a waiter")

func (s *handleState) wait(ctx context.Context, ticket uint64) ([]byte, error) {
	res, takeID, err := s.waitInternal(ctx, ticket)
	if err != nil {
		return nil, shutdownCause(ctx, err)
	}
	if takeID != 0 {
		// Close takes cgoMu exclusively before HandleClose frees Rust memory.
		// Copying without that lock raced Close: the view was still readable and
		// the destination was filled from freed pages (GOGC=1
		// TestStress_ConcurrentCallAndCloseRace).
		if !s.enterCgo() {
			return nil, errors.New("gusset: handle is closed")
		}
		out := make([]byte, len(res.data))
		copy(out, res.data)
		s.cgoMu.RUnlock()
		_ = s.bufFree(takeID)
		return out, nil
	}
	return res.data, nil
}

func (s *handleState) waitBuffer(ctx context.Context, ticket uint64) (*Buffer, error) {
	res, takeID, err := s.waitInternal(ctx, ticket)
	if err != nil {
		return nil, shutdownCause(ctx, err)
	}

	// Wrap under cgoMu so Close cannot HandleClose the take buffer between
	// waitInternal returning the view and newBufferFromRaw installing it.
	if !s.enterCgo() {
		s.discardTake(takeID)
		return nil, errors.New("gusset: handle is closed")
	}
	if takeID != 0 {
		buf := newBufferFromRaw(s, takeID, res.data)
		s.cgoMu.RUnlock()
		return buf, nil
	}
	s.cgoMu.RUnlock()

	if len(res.data) > 0 {
		buf, err := s.allocBuffer(len(res.data), false)
		if err != nil {
			if s.closed.Load() {
				return nil, errors.New("gusset: handle is closed")
			}
			// This result already exists and has left completed; any error
			// here would lose it for good. Poison refuses new work only (I2),
			// and NewBuffer's Rust allocation is poison-checked, so a sibling's
			// panic used to turn a finished result into ErrPoisoned. Carry it
			// on the Go heap instead, 64-byte aligned like a Rust buffer.
			return newHeapBuffer(s, res.data), nil
		}
		b := buf.Bytes()
		if b == nil {
			_ = buf.Free()
			return nil, errors.New("gusset: handle is closed")
		}
		copy(b, res.data)
		return buf, nil
	}
	if s.closed.Load() {
		return nil, errors.New("gusset: handle is closed")
	}
	// A job returning an empty output (0 bytes) produces a valid empty Buffer
	return newBufferFromRaw(s, 0, nil), nil
}

func (s *handleState) waitInternal(ctx context.Context, ticket uint64) (callResult, uint64, error) {
	s.mu.Lock()
	if res, done := s.completed[ticket]; done {
		delete(s.completed, ticket)
		takeID := s.popTakeIDLocked(ticket)
		s.mu.Unlock()
		s.releaseSem(ticket)
		if res.err != nil {
			s.discardTake(takeID)
			return callResult{}, 0, res.err
		}
		return res, takeID, nil
	}

	// An abandoned ticket is spent. Its result is already promised to the bin,
	// so a second waiter could only park for a completion it will never be
	// handed — the same forever-park the abandonment exists to remove.
	if _, gone := s.abandoned[ticket]; gone {
		s.mu.Unlock()
		return callResult{}, 0, ErrUnknownTicket
	}

	if s.closed.Load() || s.drainExited.Load() {
		s.mu.Unlock()
		return callResult{}, 0, errors.New("gusset: handle is closed")
	}

	// Refuse a ticket this handle is not holding.
	//
	// Wait used to register any ticket in the pending map and then, on ctx.Done,
	// block unconditionally for a completion that was never coming. A ticket that
	// was never submitted here — a typo, a stale id, one from another handle — parked
	// the caller forever and its context deadline did nothing at all. semTickets is
	// exactly the set of live tickets this handle issued and has not yet handed back.
	if _, live := s.semTickets[ticket]; !live {
		s.mu.Unlock()
		return callResult{}, 0, ErrUnknownTicket
	}

	// Refuse a second waiter rather than displacing the first.
	//
	// Assigning s.pending[ticket] unconditionally replaced the first waiter's
	// channel. The completion was then delivered to the second waiter and the first
	// blocked forever, because nothing held a reference to its channel any more. A
	// result can be moved out exactly once, so a ticket can have exactly one waiter.
	//
	// The permit still belongs to the owner. A deferred releaseSem on this
	// return used to consume it, so a third Submit could enter while the job
	// was still running (I4).
	if _, busy := s.pending[ticket]; busy {
		s.mu.Unlock()
		return callResult{}, 0, ErrTicketBusy
	}

	ticketCh := make(chan callResult, 1)
	s.pending[ticket] = ticketCh
	s.mu.Unlock()

	select {
	case res := <-ticketCh:
		s.mu.Lock()
		takeID := s.popTakeIDLocked(ticket)
		s.mu.Unlock()
		s.releaseSem(ticket)
		if res.err != nil {
			s.discardTake(takeID)
			return callResult{}, 0, res.err
		}
		return res, takeID, nil

	case <-ctx.Done():
		// Ask Rust to stop. This is all cancellation can be: the flag is only
		// read by an engine that calls JobContext::check, and an engine that
		// never does — a tokenizer, a regex scan, a proof verifier — runs to
		// completion regardless.
		// Never blocks: a close in progress cancels every job itself, and
		// parking behind its worker join would hold this caller past the
		// deadline it is returning for.
		if s.enterCgo() {
			_ = ffi.Cancel(s.ptr, ticket)
			s.cgoMu.RUnlock()
		}

		// Detach rather than wait for the completion.
		//
		// This receive used to be unconditional, which made the caller's
		// deadline a statement about the engine rather than about Gusset: an
		// 80 ms deadline on a 1.5 s non-cooperative job returned after 1.5 s.
		// Bounding the caller is the whole contract, so the caller leaves now
		// and drainPipe disposes of the result when it lands.
		//
		// The permit stays behind deliberately. The worker is still executing,
		// and handing the permit back here would let a further submission run
		// alongside it — in-flight work above the pool size, which is exactly
		// the OS-thread bound I4 sells. deliver returns the permit at the
		// moment the work actually stops.
		s.mu.Lock()
		delete(s.pending, ticket)
		select {
		case res := <-ticketCh:
			// Raced: the completion landed between ctx firing and this lock, so
			// there is nothing to abandon and the permit is free now.
			takeID := s.popTakeIDLocked(ticket)
			s.mu.Unlock()
			s.releaseSem(ticket)
			s.discardTake(takeID)
			// A panic that arrived in the race window is reported as the panic,
			// not as the deadline: the caller asked what happened to its work
			// and a real answer exists. drainPipe has already latched the
			// poison.
			if res.err != nil && errors.Is(res.err, ErrPanic) {
				return callResult{}, 0, res.err
			}
			return callResult{}, 0, ctx.Err()
		default:
		}
		s.abandoned[ticket] = struct{}{}
		s.mu.Unlock()
		return callResult{}, 0, ctx.Err()
	}
}
