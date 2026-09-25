package gusset

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

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
	// owner keeps the Handle reachable for as long as this Buffer is. The
	// Handle's AddCleanup closes it when it becomes unreachable, and closing
	// frees every Rust buffer of that handle — so a caller who kept only the
	// Buffer (exactly what the Bytes doc tells them to do) had its memory
	// released underneath a live slice. No cycle: the Handle's cleanup argument
	// is its state, not the Handle.
	owner *Handle
	// budgeted is the bytes this buffer holds against WithBufferBudget,
	// returned exactly once by Free or the cleanup backstop.
	budgeted int64
}

// ErrBufferBudget is returned by NewBuffer when the handle's live
// caller-allocated buffers would exceed WithBufferBudget.
var ErrBufferBudget = errors.New("gusset: handle buffer budget exhausted; Free buffers before allocating more")

type bufferCleanupInfo struct {
	state *handleState
	id    uint64
	// budgeted is the bytes this buffer holds against WithBufferBudget.
	budgeted int64
}

// NewBuffer allocates a 64-byte aligned buffer in Rust-owned memory (R16).
// Suitable for inputs larger than 4 KiB to avoid double copying across FFI.
func (h *Handle) NewBuffer(n int) (*Buffer, error) {
	if h == nil || h.state == nil {
		return nil, errors.New("gusset: handle is nil")
	}
	buf, err := h.state.newBuffer(n)
	if buf != nil {
		buf.owner = h
	}
	runtime.KeepAlive(h)
	return buf, err
}

func (s *handleState) newBuffer(n int) (*Buffer, error) {
	return s.allocBuffer(n, true)
}

// reserveBudget claims n bytes of the handle's buffer budget, if one is set.
func (s *handleState) reserveBudget(n int64) bool {
	if s.bufBudget <= 0 {
		return true
	}
	for {
		cur := s.bufBytes.Load()
		if cur+n > s.bufBudget {
			return false
		}
		if s.bufBytes.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

func (s *handleState) releaseBudget(n int64) {
	if n > 0 {
		s.bufBytes.Add(-n)
	}
}

// allocBuffer allocates a Rust buffer. budgeted buffers count against
// WithBufferBudget; a finished result re-wrapped for WaitBuffer does not, since
// refusing it would lose work that already completed.
func (s *handleState) allocBuffer(n int, budgeted bool) (*Buffer, error) {
	if n <= 0 {
		return nil, errors.New("gusset: buffer size must be greater than zero")
	}
	if n > MaxBufferBytes {
		return nil, fmt.Errorf("gusset: buffer size exceeds maximum %d bytes", MaxBufferBytes)
	}
	if s.closed.Load() {
		return nil, errors.New("gusset: handle is closed")
	}
	if s.poisoned.Load() {
		return nil, errHandlePoisoned
	}

	var charge int64
	if budgeted && s.bufBudget > 0 {
		if !s.reserveBudget(int64(n)) {
			return nil, ErrBufferBudget
		}
		charge = int64(n)
	}
	if !s.enterCgo() {
		s.releaseBudget(charge)
		return nil, errors.New("gusset: handle is closed")
	}
	if s.poisoned.Load() {
		s.cgoMu.RUnlock()
		s.releaseBudget(charge)
		return nil, errHandlePoisoned
	}
	id, slice, err := ffi.BufAlloc(s.ptr, n)
	s.cgoMu.RUnlock()

	if err != nil {
		s.releaseBudget(charge)
		if errors.Is(err, ErrPoisoned) {
			s.poisoned.Store(true)
		}
		return nil, err
	}

	return newBufferCharged(s, id, slice, charge), nil
}

func newBufferFromRaw(s *handleState, id uint64, slice []byte) *Buffer {
	return newBufferCharged(s, id, slice, 0)
}

func newBufferCharged(s *handleState, id uint64, slice []byte, charge int64) *Buffer {
	buf := &Buffer{
		id:       id,
		state:    s,
		data:     slice,
		budgeted: charge,
	}

	if id > 0 {
		// AddCleanup backstop if caller forgets to explicitly Free.
		buf.cleanup = runtime.AddCleanup(buf, func(info bufferCleanupInfo) {
			releaseForgottenBuffer(info)
		}, bufferCleanupInfo{state: s, id: id, budgeted: charge})
	}

	return buf
}

// newHeapBuffer copies a small result into 64-byte aligned Go memory.
//
// Id 0: nothing to free in Rust, and submit sends its bytes inline. Only used
// for results within the 4 KiB inline limit.
func newHeapBuffer(s *handleState, data []byte) *Buffer {
	const align = 64
	backing := make([]byte, len(data)+align-1)
	off := 0
	if rem := int(uintptr(unsafe.Pointer(unsafe.SliceData(backing))) % align); rem != 0 {
		off = align - rem
	}
	view := backing[off : off+len(data) : off+len(data)]
	copy(view, data)
	return &Buffer{state: s, data: view}
}

// releaseForgottenBuffer frees a buffer whose Go owner became unreachable
// without an explicit Free.
func releaseForgottenBuffer(info bufferCleanupInfo) {
	_ = info.state.bufFreeCleanup(info.id)
	info.state.releaseBudget(info.budgeted)
}

func (s *handleState) bufFreeCleanup(id uint64) error {
	if id == 0 {
		return nil
	}
	// Never blocks. Cleanups in one queued block run one after another, and a
	// close in progress holds cgoMu exclusively across an unbounded worker
	// join; parking here stalled every cleanup queued behind this one. Close
	// frees every buffer of the handle anyway, so there is nothing to do.
	if !s.enterCgo() {
		return nil
	}
	defer s.cgoMu.RUnlock()
	slog.Warn("gusset: Buffer was garbage collected without explicit Free()")
	return ffi.BufFree(s.ptr, id)
}

func (s *handleState) bufFree(id uint64) error {
	if id == 0 {
		return nil
	}
	if !s.enterCgo() {
		return nil // close frees every buffer of the handle
	}
	defer s.cgoMu.RUnlock()
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
//
// Do not write to a Buffer while a job submitted with it as input is still
// running: from Submit until that ticket's Wait/WaitBuffer returns (for an
// abandoned ticket, until Close). The engine reads the same memory as a Rust
// &[u8], and a concurrent write is a data race on both sides of the boundary —
// an engine that validates its input and then re-reads it can act on bytes it
// never validated. Reading is always fine.
func (b *Buffer) Bytes() []byte {
	if b == nil || b.state == nil {
		return nil
	}
	// Close frees every buffer of this handle while it holds cgoMu. Observing
	// the slice under the same lock means Bytes either returns before that
	// free begins, or it sees the handle closed and returns nil.
	if b.id == 0 {
		// Go memory (an empty or heap-carried result): nothing Close frees.
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.freed.Load() || b.state.closed.Load() {
			return nil
		}
		return b.data
	}
	if !b.state.enterCgo() {
		return nil
	}
	defer b.state.cgoMu.RUnlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.freed.Load() || b.state.closed.Load() {
		return nil
	}
	return b.data
}

// ID returns the Rust buffer id, or 0 for a buffer that owns no Rust memory: an
// empty result, or a small result carried in Go memory (see WaitBuffer). A
// 0-id Buffer passed to Submit or CallBuffer sends its bytes inline.
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
	charge := b.budgeted
	// Drop our own view before releasing the lock, so a Bytes that acquires it
	// next sees freed and never copies this header out.
	// An id-0 buffer's data is Go memory and is never written after
	// construction, so Submit can read it after an atomic freed check without
	// a lock (see submitInput). Only a Rust view is withdrawn here.
	if b.id != 0 {
		b.data = nil
	}
	b.mu.Unlock()

	// bufFree takes cgoMu. Do not hold b.mu across that: Bytes acquires cgoMu
	// and then b.mu, and the opposite order deadlocks.
	var err error
	if state != nil {
		err = state.bufFree(id)
		state.releaseBudget(charge)
	}
	return err
}
