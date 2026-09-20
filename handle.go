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
	data []byte
	err  error
}

// Option configures Handle settings.
type Option func(*handleConfig)

type handleConfig struct {
	poolSize uint32
}

// WithPoolSize sets the worker thread pool size for this handle.
func WithPoolSize(n int) Option {
	return func(c *handleConfig) {
		if n > 0 {
			c.poolSize = uint32(n)
		}
	}
}

// Handle represents an active Gusset runtime session (I4).
type Handle struct {
	ptr       unsafe.Pointer
	sem       chan struct{}
	poisoned  atomic.Bool
	closed    atomic.Bool
	pipe      *os.File
	drainDone chan struct{}
	mu        sync.Mutex
	pending   map[uint64]chan callResult
	completed map[uint64]callResult
	cleanup   runtime.Cleanup
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

	h := &Handle{
		ptr:       hPtr,
		sem:       make(chan struct{}, cfg.poolSize),
		pipe:      r,
		drainDone: make(chan struct{}),
		pending:   make(map[uint64]chan callResult),
		completed: make(map[uint64]callResult),
	}

	// Register AddCleanup backstop (logs if app forgot to close)
	h.cleanup = runtime.AddCleanup(h, func(p unsafe.Pointer) {
		slog.Warn("gusset: handle was garbage collected without explicit Close()")
		_ = ffi.HandleClose(p)
	}, hPtr)

	// Start pipe reader goroutine (parks on netpoller)
	go h.drainPipe()

	return h, nil
}

// drainPipe reads 8-byte completion tickets from the pipe.
func (h *Handle) drainPipe() {
	defer close(h.drainDone)
	var buf [8]byte

	for {
		_, err := io.ReadFull(h.pipe, buf[:])
		if err != nil {
			// Pipe closed on handle shutdown or EOF
			h.mu.Lock()
			for _, ch := range h.pending {
				ch <- callResult{err: errors.New("gusset: handle closed")}
			}
			h.pending = make(map[uint64]chan callResult)
			h.mu.Unlock()
			return
		}

		ticket := binary.NativeEndian.Uint64(buf[:])

		// Retrieve result from Rust
		bufID, outBytes, takeErr := ffi.Take(h.ptr, ticket)
		var res callResult
		if takeErr != nil {
			if errors.Is(takeErr, ErrPanic) {
				h.poisoned.Store(true)
			}
			res = callResult{err: takeErr}
		} else {
			var out []byte
			if len(outBytes) > 0 {
				out = make([]byte, len(outBytes))
				copy(out, outBytes)
			}
			if bufID > 0 {
				_ = ffi.BufFree(h.ptr, bufID)
			}
			res = callResult{data: out}
		}

		h.mu.Lock()
		ch, exists := h.pending[ticket]
		if exists {
			delete(h.pending, ticket)
			ch <- res
		} else {
			h.completed[ticket] = res
		}
		h.mu.Unlock()
	}
}

// Close gracefully cancels pending work, shuts down the pool, and releases resources.
func (h *Handle) Close() error {
	if h.closed.Swap(true) {
		return nil
	}

	h.cleanup.Stop()
	_ = ffi.CancelAll(h.ptr)
	err := ffi.HandleClose(h.ptr)
	// Wait for pipe drain reader to receive EOF from closed write fd
	<-h.drainDone
	_ = h.pipe.Close()

	runtime.KeepAlive(h)
	return err
}

// Call executes a unit of work synchronously within the caller's context deadline.
//
// Invariant: callers park on the Go semaphore, never blocking on an OS thread (I4).
func (h *Handle) Call(ctx context.Context, in []byte) ([]byte, error) {
	if h.poisoned.Load() {
		return nil, ErrPoisoned
	}
	if h.closed.Load() {
		return nil, errors.New("gusset: handle is closed")
	}

	// Acquire semaphore bounded by pool size (I4)
	select {
	case h.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-h.sem }()

	header := extractCallHeader(ctx)
	ticket, err := ffi.Submit(h.ptr, header, in, 0)
	if err != nil {
		if errors.Is(err, ErrPanic) {
			h.poisoned.Store(true)
		}
		runtime.KeepAlive(h)
		return nil, err
	}

	ticketCh := make(chan callResult, 1)

	h.mu.Lock()
	if res, done := h.completed[ticket]; done {
		delete(h.completed, ticket)
		h.mu.Unlock()
		runtime.KeepAlive(h)
		return res.data, res.err
	}
	h.pending[ticket] = ticketCh
	h.mu.Unlock()

	select {
	case res := <-ticketCh:
		runtime.KeepAlive(h)
		return res.data, res.err
	case <-ctx.Done():
		// R9: Cancel task and drain ticket so result is never leaked
		_ = ffi.Cancel(h.ptr, ticket)
		select {
		case <-ticketCh:
		default:
		}
		runtime.KeepAlive(h)
		return nil, ctx.Err()
	}
}

// Submit submits a job asynchronously (accepts either []byte or *Buffer) and returns a ticket.
func (h *Handle) Submit(ctx context.Context, in any) (uint64, error) {
	if h.poisoned.Load() {
		return 0, ErrPoisoned
	}
	if h.closed.Load() {
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
	case h.sem <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	header := extractCallHeader(ctx)
	ticket, err := ffi.Submit(h.ptr, header, rawInput, bufferID)
	if err != nil {
		<-h.sem
		if errors.Is(err, ErrPanic) {
			h.poisoned.Store(true)
		}
		runtime.KeepAlive(h)
		return 0, err
	}

	runtime.KeepAlive(h)
	return ticket, nil
}

// Wait waits for completion of an asynchronously submitted job ticket.
func (h *Handle) Wait(ctx context.Context, ticket uint64) ([]byte, error) {
	defer func() { <-h.sem }()

	h.mu.Lock()
	if res, done := h.completed[ticket]; done {
		delete(h.completed, ticket)
		h.mu.Unlock()
		runtime.KeepAlive(h)
		return res.data, res.err
	}
	ticketCh := make(chan callResult, 1)
	h.pending[ticket] = ticketCh
	h.mu.Unlock()

	select {
	case res := <-ticketCh:
		runtime.KeepAlive(h)
		return res.data, res.err
	case <-ctx.Done():
		_ = ffi.Cancel(h.ptr, ticket)
		select {
		case <-ticketCh:
		default:
		}
		runtime.KeepAlive(h)
		return nil, ctx.Err()
	}
}
