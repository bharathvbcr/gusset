// Package isolate is the Phase 4 spike (docs/ipc.md): the same Handle
// contract served from a worker process, so a fault the in-process firewall
// cannot catch — SIGKILL from the OOM killer, SIGABRT, a segfault in unsafe
// engine code, a GPU driver reset — ends the worker and not the Go service.
//
// It is internal on purpose. The transport is length-prefixed JSON frames on
// the worker's stdin and stdout (the house process contract), standing in for
// the iceoryx2 shared-memory transport docs/ipc.md specifies, whose Go binding
// does not exist yet. No public entry point is added; DECISIONS.md 2026-10-08
// records what the spike found and what a public one would still need.
package isolate

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/bharathvbcr/gusset"
)

// protocolVersion is checked in the worker's hello frame. A host and worker
// built from different revisions refuse each other instead of misreading a
// frame (the I6 rule, applied to the process contract).
const protocolVersion = 1

const (
	opHello  = "hello"
	opSubmit = "submit"
	opCancel = "cancel"
)

// maxInlineInput is the in-process copy limit (R16), kept so a payload one
// mode accepts the other does not refuse.
const maxInlineInput = 4096

// Frame bounds. A request carries at most maxInlineInput bytes, base64 in
// JSON, so 64 KiB is generous. A response carries one result of at most
// gusset.MaxBufferBytes, which base64 inflates by 4/3.
const (
	maxRequestFrame  = 64 << 10
	maxResponseFrame = gusset.MaxBufferBytes/3*4 + 64<<10
)

type request struct {
	Op string `json:"op"`
	ID uint64 `json:"id,omitempty"`
	In []byte `json:"in,omitempty"`
	// TimeoutNS is relative to the moment the host stamped it, as in the
	// in-process CallHeader (R9): 0 means no deadline, 1 already expired.
	// Absolute monotonic readings never cross a process boundary either.
	TimeoutNS uint64  `json:"timeout_ns,omitempty"`
	Opcode    *uint32 `json:"opcode,omitempty"`
	// Trace is the context's TraceCarrier, trace id then span id, so R9
	// correlation survives the hop.
	Trace *[24]byte `json:"trace,omitempty"`
}

type response struct {
	Op       string     `json:"op,omitempty"`
	ID       uint64     `json:"id,omitempty"`
	Out      []byte     `json:"out,omitempty"`
	Err      *wireError `json:"err,omitempty"`
	Version  int        `json:"version,omitempty"`
	PoolSize int        `json:"pool_size,omitempty"`
}

// wireError carries a worker-side error. A *gusset.Error crosses as its
// fields, so errors.Is by code, and the cancel-message matches
// (context.DeadlineExceeded, ErrShutdown), work on the host unchanged.
// Anything else crosses as a kind plus its text.
type wireError struct {
	Code int    `json:"code,omitempty"`
	Msg  string `json:"msg"`
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
	Kind string `json:"kind,omitempty"`
}

const (
	kindFFI      = ""
	kindClosed   = "closed"
	kindShutdown = "shutdown"
	kindDeadline = "deadline"
	kindCanceled = "canceled"
	kindOther    = "other"
)

func encodeError(err error) *wireError {
	if err == nil {
		return nil
	}
	var fe *gusset.Error
	if errors.As(err, &fe) {
		return &wireError{Code: fe.Code, Msg: fe.Msg, File: fe.File, Line: fe.Line, Kind: kindFFI}
	}
	kind := kindOther
	switch {
	case errors.Is(err, gusset.ErrClosed):
		kind = kindClosed
	case errors.Is(err, gusset.ErrShutdown):
		kind = kindShutdown
	case errors.Is(err, contextDeadlineExceeded):
		kind = kindDeadline
	case errors.Is(err, contextCanceled):
		kind = kindCanceled
	}
	return &wireError{Msg: err.Error(), Kind: kind}
}

func (w *wireError) decode() error {
	switch w.Kind {
	case kindFFI:
		return &gusset.Error{Code: w.Code, Msg: w.Msg, File: w.File, Line: w.Line}
	case kindClosed:
		return &remoteError{msg: w.Msg, base: gusset.ErrClosed}
	case kindShutdown:
		return &remoteError{msg: w.Msg, base: gusset.ErrShutdown}
	case kindDeadline:
		return &remoteError{msg: w.Msg, base: contextDeadlineExceeded}
	case kindCanceled:
		return &remoteError{msg: w.Msg, base: contextCanceled}
	default:
		return &remoteError{msg: w.Msg}
	}
}

// remoteError keeps the worker's text and the sentinel it matched there.
type remoteError struct {
	msg  string
	base error
}

func (e *remoteError) Error() string { return e.msg }
func (e *remoteError) Unwrap() error { return e.base }

// writeFrame writes v as a 4-byte big-endian length and a JSON body.
func writeFrame(w io.Writer, v any, limit int) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(body) > limit {
		return fmt.Errorf("isolate: frame of %d bytes exceeds the %d-byte limit", len(body), limit)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// readFrame reads one frame into v, refusing a declared length over limit
// before allocating it. A clean EOF between frames is io.EOF; EOF inside one
// is io.ErrUnexpectedEOF.
func readFrame(r *bufio.Reader, limit int, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if uint64(n) > uint64(limit) {
		return fmt.Errorf("isolate: frame of %d bytes exceeds the %d-byte limit", n, limit)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	return json.Unmarshal(body, v)
}
