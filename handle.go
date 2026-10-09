package gusset

import (
	"context"
	"errors"
	"fmt"
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
	// drainErr is why drainPipe stopped, when it stopped with the handle
	// still open. Stored before drainExited; see closedErr.
	drainErr atomic.Pointer[drainExitError]
	mu       sync.Mutex
	cgoMu    sync.RWMutex
	// pending maps a ticket to its waiter's channel. deliver, and drainPipe's
	// exit when it sends "handle closed", set the entry to nil as they send;
	// the waiter deletes it once it has collected (see collectLocked), so a
	// ticket being collected still reads as busy.
	pending map[uint64]chan callResult
	// waitChans holds empty result channels for reuse (guarded by mu). A
	// buffered channel of callResult is two allocations, the header and the
	// buffer, and they were the only two a Call made. A channel is recycled
	// only once it is empty and no longer referenced by pending: every send
	// happens under mu and replaces the channel's pending entry with nil
	// first, so nothing can send to one here. At most cap(sem) waiters exist
	// at once (each holds a permit), so the list never outgrows that.
	waitChans  []chan callResult
	completed  map[uint64]callResult
	semTickets map[uint64]struct{}
	// abandoned holds tickets whose owner was released by its context before
	// the engine finished. The permit stays in semTickets — the worker is still
	// busy — and deliver reclaims both the result and the permit when the
	// completion finally lands.
	abandoned map[uint64]struct{}
	// parkedWaiters counts waiters blocked on their result channel. A delivery
	// to one readies it onto the reader's P, so drainPipe yields after it; with
	// none parked the reader keeps polling (see ticketReader.pollPause).
	parkedWaiters atomic.Int32
	// quietProcs caches quietAt(runtime.GOMAXPROCS(0)), whose read takes the
	// scheduler's global lock. drainPipe refreshes it every gomaxprocsRefresh.
	quietProcs atomic.Bool
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

// backstopClosers bounds how many GC-backstop closes run at once.
//
// close blocks in cgo while Rust joins the handle's workers, and Go gives
// every goroutine blocked in cgo its own M, which it keeps afterwards: Go
// never returns idle Ms to the OS. One GC that found a burst of N dropped
// handles started N closes and left N threads behind for the life of the
// process. A close waiting here is a parked goroutine and holds no M, so a
// burst of any size now costs at most cap(backstopClosers). Explicit Close is
// not affected; this is only the leak backstop's path.
var backstopClosers = make(chan struct{}, 4)

func backstopClose(s *handleState) {
	backstopClosers <- struct{}{}
	defer func() { <-backstopClosers }()
	_ = s.close()
}

// Open opens a new Gusset handle with bounded concurrency (I4).
func Open(opts ...Option) (*Handle, error) {
	cfg := handleConfig{poolSize: 4}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.poolSizeErr != nil {
		return nil, cfg.poolSizeErr
	}
	if cfg.bufferBudget < 0 {
		return nil, fmt.Errorf("gusset: buffer budget must not be negative (got %d); 0 means unlimited", cfg.bufferBudget)
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
		waitChans:     make([]chan callResult, 0, cfg.poolSize),
		completed:     make(map[uint64]callResult),
		semTickets:    make(map[uint64]struct{}),
		abandoned:     make(map[uint64]struct{}),
		takeIDs:       make(map[uint64]uint64),
	}

	state.quietProcs.Store(quietAt(runtime.GOMAXPROCS(0)))

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
		go backstopClose(s)
	}, state)

	// Start pipe reader goroutine (parks on netpoller)
	go drainPipe(state)

	return h, nil
}

// Close cancels pending work, joins the pool, and releases resources.
//
// The join is bounded by the Rust close budget (30s). Workers that have
// already finished, or that observe cancellation, are joined and Close
// returns nil. A worker that never returns produces an error instead of
// wedging the caller; the Rust pool stays allocated until that worker exits,
// so the error is not permission to treat the engine's memory as freed.
func (h *Handle) Close() error {
	if h == nil || h.state == nil {
		return ErrNilHandle
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
	toFree := s.takeUnclaimedLocked()
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
// worker join (up to the 30s budget), ignoring its own context. TryRLock
// fails exactly when that writer is holding or waiting, which is exactly
// "closing".
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
