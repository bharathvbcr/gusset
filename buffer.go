package gusset

import (
	"errors"
	"log/slog"
	"runtime"
	"sync/atomic"
	"unsafe"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// Buffer wraps a 64-byte aligned Rust-owned buffer (R16).
type Buffer struct {
	id      uint64
	hPtr    unsafe.Pointer
	data    []byte
	freed   atomic.Bool
	cleanup runtime.Cleanup
}

// NewBuffer allocates a 64-byte aligned buffer in Rust-owned memory (R16).
// Suitable for inputs larger than 4 KiB to avoid double copying across FFI.
func (h *Handle) NewBuffer(n int) (*Buffer, error) {
	if n <= 0 {
		return nil, errors.New("gusset: buffer size must be greater than zero")
	}

	id, slice, err := ffi.BufAlloc(h.ptr, n)
	if err != nil {
		runtime.KeepAlive(h)
		return nil, err
	}

	buf := &Buffer{
		id:   id,
		hPtr: h.ptr,
		data: slice,
	}

	// AddCleanup backstop if caller forgets to explicitly Free
	buf.cleanup = runtime.AddCleanup(buf, func(info struct {
		hPtr unsafe.Pointer
		id   uint64
	}) {
		slog.Warn("gusset: Buffer was garbage collected without explicit Free()")
		_ = ffi.BufFree(info.hPtr, info.id)
	}, struct {
		hPtr unsafe.Pointer
		id   uint64
	}{hPtr: h.ptr, id: id})

	runtime.KeepAlive(h)
	return buf, nil
}

// Bytes returns the slice view of the buffer memory.
func (b *Buffer) Bytes() []byte {
	return b.data
}

// ID returns the internal buffer ticket identifier.
func (b *Buffer) ID() uint64 {
	return b.id
}

// Free explicitly releases the Rust-owned buffer memory (R4).
func (b *Buffer) Free() error {
	if b.freed.Swap(true) {
		return nil
	}
	b.cleanup.Stop()
	return ffi.BufFree(b.hPtr, b.id)
}
