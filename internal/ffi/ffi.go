//go:generate go run gen.go

package ffi

/*
// Linking modes.
//
// The default resolves libgusset.a out of this checkout's target/ directory, which
// is how the repository's own build and tests work. That path does not exist for
// anyone consuming Gusset as a Go module: target/ is gitignored, so it is not in
// the module zip, and the module cache is read-only anyway. An adopter building
// against the published module gets "ld: library 'gusset' not found" plus two
// "search path not found" warnings naming a directory they have never heard of.
//
// Two supported ways out, both documented in docs/adoption.md:
//
//   - Build with -tags gusset_pkgconfig after `make install`, which writes
//     gusset.pc alongside the archive.
//   - Or point the linker at the archive directly:
//     CGO_LDFLAGS="-L/path/to/lib" go build ./...
//
// Under R14 an adopter links exactly one Rust staticlib, built from an umbrella
// crate pulling in both Gusset and their engine. Because the library name below is
// fixed, that umbrella crate must set [lib] name = "gusset" so its output is
// libgusset.a whatever the package is called.

#cgo CFLAGS: -I${SRCDIR}
#cgo !gusset_pkgconfig LDFLAGS: -L${SRCDIR}/../../target/release -L${SRCDIR}/../../target/debug -lgusset -lpthread -lm -ldl
#cgo gusset_pkgconfig pkg-config: gusset
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
	"context"
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
	if e == nil {
		return false
	}
	if t, ok := target.(*Error); ok {
		return e.Code == t.Code
	}
	// Rust reports cooperative cancel as FFI_ERR with "cancelled: {reason}".
	// Callers already use errors.Is(..., context.DeadlineExceeded / Canceled);
	// matching only *Error by code made a correctly cancelled job look unexpected.
	switch target {
	case context.DeadlineExceeded:
		return e.Msg == "cancelled: DeadlineExceeded"
	case context.Canceled:
		return e.Msg == "cancelled: Explicit"
	}
	return false
}

// Sentinel errors for matching with errors.Is.
var (
	ErrGeneric  = &Error{Code: FFI_ERR}
	ErrPanic    = &Error{Code: FFI_PANIC}
	ErrPoisoned = &Error{Code: FFI_POISONED}
	ErrBadArg   = &Error{Code: FFI_BAD_ARG}
)

func statusToError(st *C.FfiStatus) error {
	if st == nil {
		return nil
	}
	if st.code == C.FFI_OK {
		return nil
	}

	code := int(st.code)
	var msg string
	if st.msg != nil && st.msg_len > 0 {
		msgLen := st.msg_len
		if msgLen > 65536 {
			msgLen = 65536
		}
		b := C.GoBytes(unsafe.Pointer(st.msg), C.int(msgLen))
		msg = string(b)
	}

	var file string
	if st.file != nil && st.file_len > 0 {
		fileLen := st.file_len
		if fileLen > 4096 {
			fileLen = 4096
		}
		b := C.GoBytes(unsafe.Pointer(st.file), C.int(fileLen))
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

// AbiTypeCount is the number of repr(C) types whose layout is verified at init.
// Order: CallHeader, FfiStatus, AbiLayout, AllocStats.
const AbiTypeCount = 4

// FlagDiagnosticEngine opts a submission into the built-in diagnostic engine.
// See GUSSET_FLAG_DIAGNOSTIC_ENGINE in gusset.h.
const FlagDiagnosticEngine uint32 = C.GUSSET_FLAG_DIAGNOSTIC_ENGINE

// AbiLayout is the Go representation of AbiLayout.
type AbiLayout struct {
	Version uint32
	Sizes   [AbiTypeCount]uint32
	Aligns  [AbiTypeCount]uint32
}

// AllocStats is the Go representation of AllocStats.
type AllocStats struct {
	LiveBytes  uint64
	PeakBytes  uint64
	AllocCount uint64
}

// AbiLayout retrieves the runtime ABI layout from Rust.
// LocalLayout reports the sizes and alignments cgo actually compiled for the four
// ABI structs, in the same order as AbiLayout.
//
// internal/ffi/gusset.h is hand-maintained, not generated, so it can drift from the
// Rust definitions. Comparing Rust's self-reported layout against this — rather than
// only against hand-typed Go constants — is what catches that drift, and it stays
// correct on any pointer width.
func LocalLayout() ([AbiTypeCount]uint32, [AbiTypeCount]uint32) {
	var (
		header C.CallHeader
		status C.FfiStatus
		layout C.AbiLayout
		stats  C.AllocStats
	)
	sizes := [AbiTypeCount]uint32{
		uint32(unsafe.Sizeof(header)),
		uint32(unsafe.Sizeof(status)),
		uint32(unsafe.Sizeof(layout)),
		uint32(unsafe.Sizeof(stats)),
	}
	aligns := [AbiTypeCount]uint32{
		uint32(unsafe.Alignof(header)),
		uint32(unsafe.Alignof(status)),
		uint32(unsafe.Alignof(layout)),
		uint32(unsafe.Alignof(stats)),
	}
	return sizes, aligns
}

// AbiTypeNames labels the entries of AbiLayout for error messages.
var AbiTypeNames = [AbiTypeCount]string{"CallHeader", "FfiStatus", "AbiLayout", "AllocStats"}

func GetAbiLayout() AbiLayout {
	var cLayout C.AbiLayout
	C.gusset_abi_layout(&cLayout)
	out := AbiLayout{Version: uint32(cLayout.version)}
	for i := 0; i < AbiTypeCount; i++ {
		out.Sizes[i] = uint32(cLayout.sizes[i])
		out.Aligns[i] = uint32(cLayout.aligns[i])
	}
	return out
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
