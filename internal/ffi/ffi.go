package ffi

/*
#cgo CFLAGS: -I${SRCDIR}
#cgo LDFLAGS: -L${SRCDIR}/../../target/release -L${SRCDIR}/../../target/debug -lgusset -lpthread -lm -ldl
#cgo noescape gusset_abi_layout
#cgo nocallback gusset_abi_layout
#cgo noescape gusset_init
#cgo nocallback gusset_init
#cgo noescape gusset_shutdown
#cgo nocallback gusset_shutdown
#cgo noescape gusset_handle_open
#cgo nocallback gusset_handle_open
#cgo noescape gusset_handle_close
#cgo nocallback gusset_handle_close
#cgo noescape gusset_submit
#cgo nocallback gusset_submit
#cgo noescape gusset_take
#cgo nocallback gusset_take
#cgo noescape gusset_cancel
#cgo nocallback gusset_cancel
#cgo noescape gusset_cancel_all
#cgo nocallback gusset_cancel_all
#cgo noescape gusset_status_free
#cgo nocallback gusset_status_free
#cgo noescape gusset_alloc_stats
#cgo nocallback gusset_alloc_stats
#cgo noescape gusset_drain_logs
#cgo nocallback gusset_drain_logs
#cgo noescape gusset_buf_alloc
#cgo nocallback gusset_buf_alloc
#cgo noescape gusset_buf_free
#cgo nocallback gusset_buf_free
#include "gusset.h"
*/
import "C"
import (
	"fmt"
	"unsafe"
)

const (
	FFI_OK       = int(C.FFI_OK)
	FFI_ERR      = int(C.FFI_ERR)
	FFI_PANIC    = int(C.FFI_PANIC)
	FFI_POISONED = int(C.FFI_POISONED)
	FFI_BAD_ARG  = int(C.FFI_BAD_ARG)
)

// Error represents an error returned across the FFI boundary.
type Error struct {
	Code int
	Msg  string
	File string
	Line int
}

func (e *Error) Error() string {
	if e.File != "" && e.Line > 0 {
		return fmt.Sprintf("gusset error [%d]: %s (at %s:%d)", e.Code, e.Msg, e.File, e.Line)
	}
	return fmt.Sprintf("gusset error [%d]: %s", e.Code, e.Msg)
}

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return e.Code == t.Code
}

// Sentinel errors for matching with errors.Is.
var (
	ErrGeneric  = &Error{Code: FFI_ERR}
	ErrPanic    = &Error{Code: FFI_PANIC}
	ErrPoisoned = &Error{Code: FFI_POISONED}
	ErrBadArg   = &Error{Code: FFI_BAD_ARG}
)

func statusToError(st *C.FfiStatus) error {
	if st.code == C.FFI_OK {
		return nil
	}

	code := int(st.code)
	var msg string
	if st.msg != nil && st.msg_len > 0 {
		b := C.GoBytes(unsafe.Pointer(st.msg), C.int(st.msg_len))
		msg = string(b)
	}

	var file string
	if st.file != nil && st.file_len > 0 {
		b := C.GoBytes(unsafe.Pointer(st.file), C.int(st.file_len))
		file = string(b)
	}
	line := int(st.line)

	C.gusset_status_free(st)

	return &Error{
		Code: code,
		Msg:  msg,
		File: file,
		Line: line,
	}
}

// CallHeader is the Go representation of the 40-byte CallHeader.
type CallHeader struct {
	TraceID   [16]byte
	SpanID    [8]byte
	TimeoutNS uint64
	Flags     uint32
	Reserved  uint32
}

// AbiLayout is the Go representation of AbiLayout.
type AbiLayout struct {
	Version uint32
	Sizes   [3]uint32
	Aligns  [3]uint32
}

// AllocStats is the Go representation of AllocStats.
type AllocStats struct {
	LiveBytes  uint64
	PeakBytes  uint64
	AllocCount uint64
}

// AbiLayout retrieves the runtime ABI layout from Rust.
func GetAbiLayout() AbiLayout {
	var cLayout C.AbiLayout
	C.gusset_abi_layout(&cLayout)
	return AbiLayout{
		Version: uint32(cLayout.version),
		Sizes: [3]uint32{
			uint32(cLayout.sizes[0]),
			uint32(cLayout.sizes[1]),
			uint32(cLayout.sizes[2]),
		},
		Aligns: [3]uint32{
			uint32(cLayout.aligns[0]),
			uint32(cLayout.aligns[1]),
			uint32(cLayout.aligns[2]),
		},
	}
}

// Init initializes the Gusset runtime.
func Init() error {
	code := C.gusset_init()
	if code != C.FFI_OK {
		return &Error{Code: int(code), Msg: "initialization failed"}
	}
	return nil
}

// Shutdown shuts down the Gusset runtime.
func Shutdown(drainMS uint32) error {
	code := C.gusset_shutdown(C.uint32_t(drainMS))
	if code != C.FFI_OK {
		return &Error{Code: int(code), Msg: "shutdown failed"}
	}
	return nil
}

// HandleOpen opens a new Rust handle.
func HandleOpen(poolSize uint32, pipeWriteFD int) (unsafe.Pointer, error) {
	var cHandle *C.GussetHandle
	var st C.FfiStatus
	code := C.gusset_handle_open(C.uint32_t(poolSize), C.int32_t(pipeWriteFD), &cHandle, &st)
	if code != C.FFI_OK {
		return nil, statusToError(&st)
	}
	return unsafe.Pointer(cHandle), nil
}

// HandleClose closes a Rust handle.
func HandleClose(h unsafe.Pointer) error {
	var st C.FfiStatus
	code := C.gusset_handle_close((*C.GussetHandle)(h), &st)
	if code != C.FFI_OK {
		return statusToError(&st)
	}
	return nil
}

// Submit submits a task.
func Submit(h unsafe.Pointer, header CallHeader, input []byte, bufferID uint64) (uint64, error) {
	var cHeader C.CallHeader
	for i := 0; i < 16; i++ {
		cHeader.trace_id[i] = C.uint8_t(header.TraceID[i])
	}
	for i := 0; i < 8; i++ {
		cHeader.span_id[i] = C.uint8_t(header.SpanID[i])
	}
	cHeader.timeout_ns = C.uint64_t(header.TimeoutNS)
	cHeader.flags = C.uint32_t(header.Flags)
	cHeader.reserved = C.uint32_t(header.Reserved)

	var inPtr *C.uint8_t
	var inLen C.size_t
	if len(input) > 0 {
		inPtr = (*C.uint8_t)(unsafe.Pointer(&input[0]))
		inLen = C.size_t(len(input))
	}

	var ticket C.uint64_t
	var st C.FfiStatus
	code := C.gusset_submit(
		(*C.GussetHandle)(h),
		&cHeader,
		inPtr,
		inLen,
		C.uint64_t(bufferID),
		&ticket,
		&st,
	)
	if code != C.FFI_OK {
		return 0, statusToError(&st)
	}
	return uint64(ticket), nil
}

// Take retrieves a job result.
func Take(h unsafe.Pointer, ticket uint64) (uint64, []byte, error) {
	var bufID C.uint64_t
	var outPtr *C.uint8_t
	var outLen C.size_t
	var st C.FfiStatus

	code := C.gusset_take(
		(*C.GussetHandle)(h),
		C.uint64_t(ticket),
		&bufID,
		&outPtr,
		&outLen,
		&st,
	)
	if code != C.FFI_OK {
		return 0, nil, statusToError(&st)
	}

	var slice []byte
	if outLen > 0 && outPtr != nil {
		slice = unsafe.Slice((*byte)(unsafe.Pointer(outPtr)), int(outLen))
	}

	return uint64(bufID), slice, nil
}

// Cancel cancels a task by ticket.
func Cancel(h unsafe.Pointer, ticket uint64) error {
	var st C.FfiStatus
	code := C.gusset_cancel((*C.GussetHandle)(h), C.uint64_t(ticket), &st)
	if code != C.FFI_OK {
		return statusToError(&st)
	}
	return nil
}

// CancelAll cancels all tasks on handle.
func CancelAll(h unsafe.Pointer) error {
	var st C.FfiStatus
	code := C.gusset_cancel_all((*C.GussetHandle)(h), &st)
	if code != C.FFI_OK {
		return statusToError(&st)
	}
	return nil
}

// AllocStats retrieves allocator statistics.
func GetAllocStats() AllocStats {
	var cStats C.AllocStats
	C.gusset_alloc_stats(&cStats)
	return AllocStats{
		LiveBytes:  uint64(cStats.live_bytes),
		PeakBytes:  uint64(cStats.peak_bytes),
		AllocCount: uint64(cStats.alloc_count),
	}
}

// DrainLogs drains logs from the internal buffer.
func DrainLogs(buf []byte) int {
	if len(buf) == 0 {
		return 0
	}
	var written C.size_t
	C.gusset_drain_logs((*C.uint8_t)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &written)
	return int(written)
}

// BufAlloc allocates a 64-byte aligned buffer in Rust memory.
func BufAlloc(h unsafe.Pointer, len int) (uint64, []byte, error) {
	var id C.uint64_t
	var ptr *C.uint8_t
	var st C.FfiStatus

	code := C.gusset_buf_alloc((*C.GussetHandle)(h), C.size_t(len), &id, &ptr, &st)
	if code != C.FFI_OK {
		return 0, nil, statusToError(&st)
	}

	slice := unsafe.Slice((*byte)(unsafe.Pointer(ptr)), len)
	return uint64(id), slice, nil
}

// BufFree frees a Rust-owned buffer by id.
func BufFree(h unsafe.Pointer, id uint64) error {
	var st C.FfiStatus
	code := C.gusset_buf_free((*C.GussetHandle)(h), C.uint64_t(id), &st)
	if code != C.FFI_OK {
		return statusToError(&st)
	}
	return nil
}
