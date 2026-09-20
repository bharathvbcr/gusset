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

type callResult struct {
	data   []byte
	buffer *Buffer
	err    error
}

// Option configures Handle settings.
type Option func(*handleConfig)

type handleConfig struct {
	poolSize      uint32
	callFlags     uint32
	defaultOpcode uint32
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
		if n > 0 {
			c.poolSize = uint32(n)
		}
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
}

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
			for _, res := range s.completed {
				if res.buffer != nil {
					_ = res.buffer.Free()
				}
			}
			s.pending = make(map[uint64]chan callResult)
			s.completed = make(map[uint64]callResult)
			s.mu.Unlock()
			return
		}

		ticket := binary.NativeEndian.Uint64(buf[:])

		// Synchronize with handle close to eliminate UAF on s.ptr
		s.cgoMu.RLock()
		if s.closed.Load() || s.ptr == nil {
			s.cgoMu.RUnlock()
			s.mu.Lock()
			if ch, exists := s.pending[ticket]; exists {
				delete(s.pending, ticket)
				ch <- callResult{err: errors.New("gusset: handle closed")}
			}
			s.mu.Unlock()
			continue
		}

		// Retrieve result from Rust
		bufID, outBytes, takeErr := ffi.Take(s.ptr, ticket)
		var res callResult
		if takeErr != nil {
			if errors.Is(takeErr, ErrPanic) {
				s.poisoned.Store(true)
			}
			res = callResult{err: takeErr}
		} else if (bufID & (uint64(1) << 63)) != 0 {
			// Wrap the persistent Rust-owned buffer with zero memory copies (R16 return path)
			actualBufID := bufID &^ (uint64(1) << 63)
			res = callResult{buffer: newBufferFromRaw(s, actualBufID, outBytes)}
		} else {
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

		s.mu.Lock()
		ch, exists := s.pending[ticket]
		if exists {
			delete(s.pending, ticket)
			ch <- res
		} else {
			s.completed[ticket] = res
		}
		s.mu.Unlock()
	}
}

// Close gracefully cancels pending work, shuts down the pool, and releases resources.
func (h *Handle) Close() error {
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

	// 4. Drain any remaining permits from untaken submitted tickets and free unconsumed buffers
	s.mu.Lock()
	for _, res := range s.completed {
		if res.buffer != nil {
			_ = res.buffer.Free()
		}
	}
	s.completed = make(map[uint64]callResult)
	for ticket := range s.semTickets {
		delete(s.semTickets, ticket)
		<-s.sem
	}
	s.mu.Unlock()

	return err
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
	res, err := h.state.call(ctx, in)
	runtime.KeepAlive(h)
	return res, err
}

func (s *handleState) call(ctx context.Context, in []byte) ([]byte, error) {
	if s.poisoned.Load() {
		return nil, ErrPoisoned
	}
	if s.closed.Load() {
		return nil, errors.New("gusset: handle is closed")
	}

	// Acquire semaphore bounded by pool size (I4)
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.sem }()

	if s.closed.Load() {
		return nil, errors.New("gusset: handle is closed")
	}

	header := extractCallHeader(ctx, s.callFlags, s.defaultOpcode)
	s.cgoMu.RLock()
	if s.closed.Load() || s.ptr == nil {
		s.cgoMu.RUnlock()
		return nil, errors.New("gusset: handle is closed")
	}
	ticket, err := ffi.Submit(s.ptr, header, in, 0)
	s.cgoMu.RUnlock()

	if err != nil {
		if errors.Is(err, ErrPanic) {
			s.poisoned.Store(true)
		}
		return nil, err
	}

	ticketCh := make(chan callResult, 1)

	s.mu.Lock()
	if res, done := s.completed[ticket]; done {
		delete(s.completed, ticket)
		s.mu.Unlock()
		if res.err != nil {
			return nil, res.err
		}
		if res.buffer != nil {
			defer res.buffer.Free()
			b := res.buffer.Bytes()
			if b == nil && len(res.buffer.data) > 0 {
				return nil, errors.New("gusset: handle is closed")
			}
			out := make([]byte, len(b))
			copy(out, b)
			return out, nil
		}
		return res.data, nil
	}
	if s.closed.Load() {
		s.mu.Unlock()
		return nil, errors.New("gusset: handle is closed")
	}
	s.pending[ticket] = ticketCh
	s.mu.Unlock()

	select {
	case res := <-ticketCh:
		if res.err != nil {
			return nil, res.err
		}
		if res.buffer != nil {
			defer res.buffer.Free()
			b := res.buffer.Bytes()
			if b == nil && len(res.buffer.data) > 0 {
				return nil, errors.New("gusset: handle is closed")
			}
			out := make([]byte, len(b))
			copy(out, b)
			return out, nil
		}
		return res.data, nil
	case <-ctx.Done():
		// R9: Cancel task and wait for ticket to drain before releasing semaphore (I4)
		if !s.closed.Load() {
			s.cgoMu.RLock()
			if s.ptr != nil {
				_ = ffi.Cancel(s.ptr, ticket)
			}
			s.cgoMu.RUnlock()
		}
		res := <-ticketCh
		if res.buffer != nil {
			_ = res.buffer.Free()
		}
		if res.err != nil && errors.Is(res.err, ErrPanic) {
			s.poisoned.Store(true)
			return nil, res.err
		}
		return nil, ctx.Err()
	}
}

// Submit submits a job asynchronously (accepts either []byte or *Buffer) and returns a ticket.
func (h *Handle) Submit(ctx context.Context, in any) (uint64, error) {
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
		rawInput = v
	case *Buffer:
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

	if s.closed.Load() {
		<-s.sem
		return 0, errors.New("gusset: handle is closed")
	}

	header := extractCallHeader(ctx, s.callFlags, s.defaultOpcode)
	s.cgoMu.RLock()
	if s.closed.Load() || s.ptr == nil {
		s.cgoMu.RUnlock()
		<-s.sem
		return 0, errors.New("gusset: handle is closed")
	}
	ticket, err := ffi.Submit(s.ptr, header, rawInput, bufferID)
	s.cgoMu.RUnlock()

	if err != nil {
		<-s.sem
		if errors.Is(err, ErrPanic) {
			s.poisoned.Store(true)
		}
		return 0, err
	}

	s.mu.Lock()
	s.semTickets[ticket] = struct{}{}
	s.mu.Unlock()

	return ticket, nil
}

// Wait waits for completion of an asynchronously submitted job ticket.
func (h *Handle) Wait(ctx context.Context, ticket uint64) ([]byte, error) {
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
	res, err := s.waitInternal(ctx, ticket)
	if err != nil {
		return nil, err
	}
	if res.buffer != nil {
		defer res.buffer.Free()
		b := res.buffer.Bytes()
		if b == nil && len(res.buffer.data) > 0 {
			return nil, errors.New("gusset: handle is closed")
		}
		out := make([]byte, len(b))
		copy(out, b)
		return out, nil
	}
	return res.data, nil
}

func (s *handleState) waitBuffer(ctx context.Context, ticket uint64) (*Buffer, error) {
	res, err := s.waitInternal(ctx, ticket)
	if err != nil {
		return nil, err
	}
	if res.buffer != nil {
		if res.buffer.Bytes() == nil && len(res.buffer.data) > 0 {
			_ = res.buffer.Free()
			return nil, errors.New("gusset: handle is closed")
		}
		return res.buffer, nil
	}
	if len(res.data) > 0 {
		buf, err := s.newBuffer(len(res.data))
		if err != nil {
			return nil, err
		}
		copy(buf.Bytes(), res.data)
		return buf, nil
	}
	return nil, errors.New("gusset: job did not produce a buffer output")
}

func (s *handleState) waitInternal(ctx context.Context, ticket uint64) (callResult, error) {
	defer s.releaseSem(ticket)

	s.mu.Lock()
	if res, done := s.completed[ticket]; done {
		delete(s.completed, ticket)
		s.mu.Unlock()
		if res.err != nil {
			return callResult{}, res.err
		}
		return res, nil
	}
	if s.closed.Load() {
		s.mu.Unlock()
		return callResult{}, errors.New("gusset: handle is closed")
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
		return callResult{}, ErrUnknownTicket
	}

	// Refuse a second waiter rather than displacing the first.
	//
	// Assigning s.pending[ticket] unconditionally replaced the first waiter's
	// channel. The completion was then delivered to the second waiter and the first
	// blocked forever, because nothing held a reference to its channel any more. A
	// result can be moved out exactly once, so a ticket can have exactly one waiter.
	if _, busy := s.pending[ticket]; busy {
		s.mu.Unlock()
		return callResult{}, ErrTicketBusy
	}

	ticketCh := make(chan callResult, 1)
	s.pending[ticket] = ticketCh
	s.mu.Unlock()

	select {
	case res := <-ticketCh:
		if res.err != nil {
			return callResult{}, res.err
		}
		return res, nil
	case <-ctx.Done():
		if !s.closed.Load() {
			s.cgoMu.RLock()
			if s.ptr != nil {
				_ = ffi.Cancel(s.ptr, ticket)
			}
			s.cgoMu.RUnlock()
		}
		res := <-ticketCh
		if res.buffer != nil {
			_ = res.buffer.Free()
		}
		if res.err != nil && errors.Is(res.err, ErrPanic) {
			s.poisoned.Store(true)
			return callResult{}, res.err
		}
		return callResult{}, ctx.Err()
	}
}
