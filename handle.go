package gusset

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
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
	defaultOpcode   uint32
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
	poisoned      atomic.Bool
	closed        atomic.Bool
	pipe          *os.File
	drainDone     chan struct{}
	mu            sync.Mutex
	cgoMu         sync.RWMutex
	pending       map[uint64]chan callResult
	completed     map[uint64]callResult
	semTickets    map[uint64]struct{}
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

	// Duplicate write descriptor so Rust owns an independent OS descriptor
	writeFD, err := syscall.Dup(int(w.Fd()))
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

	state := &handleState{
		ptr:           hPtr,
		callFlags:     cfg.callFlags,
		defaultOpcode: cfg.defaultOpcode,
		sem:           make(chan struct{}, cfg.poolSize),
		pipe:          r,
		drainDone:     make(chan struct{}),
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
	h.cleanup = runtime.AddCleanup(h, func(s *handleState) {
		slog.Warn("gusset: handle was garbage collected without explicit Close()")
		_ = s.close()
	}, state)

	// Start pipe reader goroutine (parks on netpoller)
	go drainPipe(state)

	return h, nil
}

// drainPipe reads 8-byte completion tickets from the pipe.
func drainPipe(s *handleState) {
	defer close(s.drainDone)
	var buf [8]byte

	for {
		_, err := io.ReadFull(s.pipe, buf[:])
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

		ticket := binary.NativeEndian.Uint64(buf[:])

		// Synchronize with handle close to eliminate UAF on s.ptr
		s.cgoMu.RLock()
		if s.closed.Load() || s.ptr == nil {
			s.cgoMu.RUnlock()
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
		case (bufID&(uint64(1)<<63)) != 0 || (bufID > 0 && len(outBytes) > inlineResultBytes):
			// Keep take()'s Rust buffer until a waiter consumes it. WaitBuffer
			// wraps with no Go copy; Wait copies out and frees. Wrapping here
			// forced every Wait of a large result to allocate a *Buffer it
			// immediately destroyed. The id lives in takeIDs, not callResult,
			// so a one-byte Call does not pay a larger completion object.
			res = callResult{data: outBytes}
			takeID = bufID &^ (uint64(1) << 63)
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
		return nil
	}

	// 1. Cancel in-flight jobs in Rust memory (R9, I3)
	s.cgoMu.RLock()
	if s.ptr != nil {
		_ = ffi.CancelAll(s.ptr)
	}
	s.cgoMu.RUnlock()

	// 2. Wait for all active CGO operations to finish before deallocating handle
	s.cgoMu.Lock()
	err := ffi.HandleClose(s.ptr)
	s.ptr = nil
	s.cgoMu.Unlock()

	// 3. Wait for pipe drain reader to receive EOF from closed write fd
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
	ticket, err := s.submit(ctx, in)
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
	ticket, err := h.state.submit(ctx, in)
	if err != nil {
		runtime.KeepAlive(h)
		return nil, err
	}
	out, err := h.state.waitBuffer(ctx, ticket)
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

func (s *handleState) submit(ctx context.Context, in any) (uint64, error) {
	if s.poisoned.Load() {
		return 0, ErrPoisoned
	}
	if s.closed.Load() {
		return 0, errors.New("gusset: handle is closed")
	}

	var rawInput []byte
	var bufferID uint64

	switch v := in.(type) {
	case []byte:
		if len(v) > inlineResultBytes {
			return 0, errors.New("gusset: []byte input exceeds 4096-byte copy limit; use NewBuffer")
		}
		rawInput = v
	case *Buffer:
		if v == nil {
			return 0, errors.New("gusset: buffer is nil")
		}
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
	case nil:
	default:
		return 0, errors.New("gusset: input must be []byte or *Buffer")
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
		return 0, ErrPoisoned
	}

	header, err := extractCallHeader(ctx, s.callFlags, s.defaultOpcode)
	if err != nil {
		<-s.sem
		return 0, err
	}
	s.cgoMu.RLock()
	if s.closed.Load() || s.ptr == nil {
		s.cgoMu.RUnlock()
		<-s.sem
		return 0, errors.New("gusset: handle is closed")
	}
	if s.poisoned.Load() {
		s.cgoMu.RUnlock()
		<-s.sem
		return 0, ErrPoisoned
	}
	ticket, err := ffi.Submit(s.ptr, header, rawInput, bufferID)
	if err != nil {
		s.cgoMu.RUnlock()
		<-s.sem
		if errors.Is(err, ErrPanic) {
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
		return nil, err
	}
	if takeID != 0 {
		// Close takes cgoMu exclusively before HandleClose frees Rust memory.
		// Copying without that lock raced Close: the view was still readable and
		// the destination was filled from freed pages (GOGC=1
		// TestStress_ConcurrentCallAndCloseRace).
		s.cgoMu.RLock()
		if s.closed.Load() || s.ptr == nil {
			s.cgoMu.RUnlock()
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
		return nil, err
	}

	// Wrap under cgoMu so Close cannot HandleClose the take buffer between
	// waitInternal returning the view and newBufferFromRaw installing it.
	s.cgoMu.RLock()
	if s.closed.Load() || s.ptr == nil {
		s.cgoMu.RUnlock()
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
		buf, err := s.newBuffer(len(res.data))
		if err != nil {
			return nil, err
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

	if s.closed.Load() {
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
		if !s.closed.Load() {
			s.cgoMu.RLock()
			if s.ptr != nil {
				_ = ffi.Cancel(s.ptr, ticket)
			}
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
