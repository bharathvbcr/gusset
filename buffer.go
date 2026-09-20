package gusset

import (
	"errors"
	"log/slog"
	"runtime"
	"sync/atomic"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// Buffer wraps a 64-byte aligned Rust-owned buffer (R16).
type Buffer struct {
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
	buf, err := h.state.newBuffer(n)
	runtime.KeepAlive(h)
	return buf, err
}

func (s *handleState) newBuffer(n int) (*Buffer, error) {
	if n <= 0 {
		return nil, errors.New("gusset: buffer size must be greater than zero")
	}
	if s.closed.Load() {
		return nil, errors.New("gusset: handle is closed")
	}

	s.cgoMu.RLock()
	if s.closed.Load() || s.ptr == nil {
		s.cgoMu.RUnlock()
		return nil, errors.New("gusset: handle is closed")
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
	if b.freed.Load() || b.state.closed.Load() {
		return nil
	}
	return b.data
}

// ID returns the internal buffer ticket identifier.
func (b *Buffer) ID() uint64 {
	return b.id
}

// Free explicitly releases the Rust-owned buffer memory (R4).
// If the parent handle is already closed, safely returns nil without use-after-free.
func (b *Buffer) Free() error {
	if b.freed.Swap(true) {
		return nil
	}
	if b.id > 0 {
		b.cleanup.Stop()
	}
	err := b.state.bufFree(b.id)
	// Drop our own view of the released memory so nothing here can resurrect it.
	b.data = nil
	return err
}
