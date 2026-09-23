package gusset

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// MaxBufferBytes is the largest single Rust-owned buffer this process will
// allocate (1 GiB). Matching MAX_BUFFER_BYTES in pool::sys. Larger payloads
// belong in Phase 4 isolation, not in-process posix_memalign.
const MaxBufferBytes = 1 << 30

// Buffer wraps a 64-byte aligned Rust-owned buffer (R16).
type Buffer struct {
	// mu covers freed and data together. Free writes both; Bytes reads both.
	// An atomic on freed alone still races the slice header: Bytes can load
	// freed==false and then read data while Free nils it.
	mu      sync.Mutex
	id      uint64
	state   *handleState
	data    []byte
	freed   atomic.Bool
	cleanup runtime.Cleanup
}

type bufferCleanupInfo struct {
	state *handleState
	id    uint64
}

// NewBuffer allocates a 64-byte aligned buffer in Rust-owned memory (R16).
// Suitable for inputs larger than 4 KiB to avoid double copying across FFI.
func (h *Handle) NewBuffer(n int) (*Buffer, error) {
	if h == nil || h.state == nil {
		return nil, errors.New("gusset: handle is nil")
	}
	buf, err := h.state.newBuffer(n)
	runtime.KeepAlive(h)
	return buf, err
}

func (s *handleState) newBuffer(n int) (*Buffer, error) {
	if n <= 0 {
		return nil, errors.New("gusset: buffer size must be greater than zero")
	}
	if n > MaxBufferBytes {
		return nil, fmt.Errorf("gusset: buffer size exceeds maximum %d bytes", MaxBufferBytes)
	}
	if s.poisoned.Load() {
		return nil, ErrPoisoned
	}
	if s.closed.Load() {
		return nil, errors.New("gusset: handle is closed")
	}

	s.cgoMu.RLock()
	if s.closed.Load() || s.ptr == nil {
		s.cgoMu.RUnlock()
		return nil, errors.New("gusset: handle is closed")
	}
	if s.poisoned.Load() {
		s.cgoMu.RUnlock()
		return nil, ErrPoisoned
	}
	id, slice, err := ffi.BufAlloc(s.ptr, n)
	s.cgoMu.RUnlock()

	if err != nil {
		return nil, err
	}

	return newBufferFromRaw(s, id, slice), nil
}

func newBufferFromRaw(s *handleState, id uint64, slice []byte) *Buffer {
	buf := &Buffer{
		id:    id,
		state: s,
		data:  slice,
	}

	if id > 0 {
		// AddCleanup backstop if caller forgets to explicitly Free
		buf.cleanup = runtime.AddCleanup(buf, func(info bufferCleanupInfo) {
			_ = info.state.bufFreeCleanup(info.id)
		}, bufferCleanupInfo{state: s, id: id})
	}

	return buf
}

func (s *handleState) bufFreeCleanup(id uint64) error {
	if id == 0 {
		return nil
	}
	s.cgoMu.RLock()
	defer s.cgoMu.RUnlock()
	if s.closed.Load() || s.ptr == nil {
		return nil
	}
	slog.Warn("gusset: Buffer was garbage collected without explicit Free()")
	return ffi.BufFree(s.ptr, id)
}

func (s *handleState) bufFree(id uint64) error {
	if id == 0 {
		return nil
	}
	s.cgoMu.RLock()
	defer s.cgoMu.RUnlock()
	if s.closed.Load() || s.ptr == nil {
		return nil
	}
	return ffi.BufFree(s.ptr, id)
}

// Bytes returns the slice view of the Rust-owned buffer memory.
//
// Returns nil once the buffer has been freed or its handle closed, so the API can
// never hand out a window onto released Rust memory. A slice obtained from an
// earlier call is not retracted by this check: the Go garbage collector does not
// trace Rust memory, and holding the slice does not keep the Buffer alive. Callers
// must not use a previously obtained slice after Free or Handle.Close, and should
// keep the *Buffer reachable (runtime.KeepAlive) for as long as they use its bytes.
func (b *Buffer) Bytes() []byte {
	if b == nil || b.state == nil {
		return nil
	}
	// Close frees every buffer of this handle while it holds cgoMu. Observing
	// the slice under the same lock means Bytes either returns before that
	// free begins, or it sees the handle closed and returns nil.
	b.state.cgoMu.RLock()
	defer b.state.cgoMu.RUnlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.freed.Load() || b.state.closed.Load() {
		return nil
	}
	return b.data
}

// ID returns the internal buffer ticket identifier.
func (b *Buffer) ID() uint64 {
	if b == nil || b.state == nil {
		return 0
	}
	return b.id
}

// Free explicitly releases the Rust-owned buffer memory (R4).
// If the parent handle is already closed, safely returns nil without use-after-free.
func (b *Buffer) Free() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	if b.freed.Swap(true) {
		b.mu.Unlock()
		return nil
	}
	if b.id > 0 {
		b.cleanup.Stop()
	}
	id := b.id
	state := b.state
	// Drop our own view before releasing the lock, so a Bytes that acquires it
	// next sees freed and never copies this header out.
	b.data = nil
	b.mu.Unlock()

	// bufFree takes cgoMu. Do not hold b.mu across that: Bytes acquires cgoMu
	// and then b.mu, and the opposite order deadlocks.
	var err error
	if state != nil {
		err = state.bufFree(id)
	}
	return err
}
