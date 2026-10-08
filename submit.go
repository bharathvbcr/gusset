package gusset

import (
	"context"
	"errors"
	"runtime"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

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
	ticket, err := s.submitInput(ctx, in, nil)
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
	ticket, err := h.state.submitInput(ctx, nil, in)
	if err != nil {
		runtime.KeepAlive(h)
		return nil, err
	}
	out, err := h.state.waitBuffer(ctx, ticket)
	if out != nil {
		out.owner = h
	}
	runtime.KeepAlive(h)
	return out, err
}

// Submit submits a job asynchronously (accepts either []byte or *Buffer) and returns a ticket.
//
// Collect every ticket with Wait or WaitBuffer, or give it up with Discard. A
// finished job returns its pool permit before anyone Waits (DECISIONS
// 2026-09-30), so a caller that never collects is not slowed down: its results
// accumulate on the handle until Wait, Discard or Close, without bound. A
// result over 4 KiB is held as Rust memory, which Go's GC pacer and
// GOMEMLIMIT do not see, so a fire-and-forget loop can exhaust memory long
// before the Go heap looks large.
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

// submit is the Submit(any) entry: it sorts the input by type and hands a
// typed pair to submitInput. Call and CallBuffer go to submitInput directly:
// passing a []byte through `any` boxed its slice header, one heap allocation
// on every Call (CallNoop 2 -> 3 allocs/op).
func (s *handleState) submit(ctx context.Context, in any) (uint64, error) {
	switch v := in.(type) {
	case []byte:
		return s.submitInput(ctx, v, nil)
	case *Buffer:
		if v == nil {
			return 0, errors.New("gusset: buffer is nil")
		}
		return s.submitInput(ctx, nil, v)
	case nil:
		return s.submitInput(ctx, nil, nil)
	default:
		return 0, errors.New("gusset: input must be []byte or *Buffer")
	}
}

// submitInput submits either inline bytes (buf == nil) or a Buffer.
func (s *handleState) submitInput(ctx context.Context, raw []byte, buf *Buffer) (uint64, error) {
	// Closed before poisoned: a poisoned handle that was then closed used to
	// answer ErrPoisoned, sending a caller whose policy is "on poison, close
	// and reopen" back to close a handle it had already closed.
	if s.closed.Load() || s.drainExited.Load() {
		return 0, ErrClosed
	}
	if s.poisoned.Load() {
		return 0, errHandlePoisoned
	}

	var rawInput []byte
	var bufferID uint64

	if buf == nil {
		if len(raw) > inlineResultBytes {
			return 0, errors.New("gusset: []byte input exceeds 4096-byte copy limit; use NewBuffer")
		}
		rawInput = raw
	} else {
		v := buf
		if v.state == nil {
			return 0, errors.New("gusset: buffer is not initialized")
		}
		if v.freed.Load() || v.state.closed.Load() {
			// This handle closed since the check above: that is ErrClosed. A
			// freed buffer, or one from another closed handle, is not: the
			// handle being called is still open.
			if v.state == s && s.closed.Load() {
				return 0, errBufferClosed
			}
			return 0, errors.New("gusset: buffer is freed or closed")
		}
		if v.state != s {
			return 0, errors.New("gusset: buffer belongs to a different handle")
		}
		bufferID = v.id
		if bufferID == 0 {
			// A Go-heap result buffer (see waitBuffer): no Rust id to pass, so
			// its bytes travel inline. Id 0 used to mean "no input", which
			// silently ran the engine on nothing. Data and liveness are read
			// in a way a concurrent Free cannot tear: freed was checked above,
			// and an id-0 buffer's data is never written after construction
			// (Free leaves it; the memory is Go's). A Free racing this Submit
			// therefore sends the real bytes, never the nil that used to
			// arrive as empty input. No lock: any mutex on this path made
			// *Buffer escape, which boxed every Submit input on the heap
			// (SubmitWait 2 -> 3 allocs/op).
			data := v.data
			if len(data) > inlineResultBytes {
				return 0, errors.New("gusset: buffer without a Rust id exceeds the 4096-byte copy limit")
			}
			rawInput = data
		}
	}

	// Trace carrier and opcode before the permit: the carrier is caller code,
	// and a Goexit inside it cannot be recovered (see callHeaderIDs).
	header, err := callHeaderIDs(ctx, s.callFlags, s.defaultOpcode)
	if err != nil {
		return 0, err
	}

	// Acquire semaphore slot. drainDone closes when the reader stops for good;
	// after that no completion can be delivered, and a submitter parked here
	// on a full pool used to stay parked until Close — forever, with a
	// context that has no deadline.
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-s.drainDone:
		return 0, ErrClosed
	}

	// In Go, select chooses pseudo-randomly when multiple channels are ready.
	// If the context is already cancelled or expired, fail fast and release permit.
	if err := ctx.Err(); err != nil {
		<-s.sem
		return 0, err
	}

	// drainExited again: a permit returned by a waiter told "handle closed"
	// wakes a parked submitter after the reader is gone, and admitting it ran
	// a job whose completion nobody would ever read.
	if s.closed.Load() || s.drainExited.Load() {
		<-s.sem
		return 0, ErrClosed
	}
	if s.poisoned.Load() {
		<-s.sem
		return 0, errHandlePoisoned
	}

	stampTimeout(ctx, &header)
	if !s.enterCgo() {
		<-s.sem
		return 0, ErrClosed
	}
	if s.poisoned.Load() {
		s.cgoMu.RUnlock()
		<-s.sem
		return 0, errHandlePoisoned
	}
	ticket, err := ffi.Submit(s.ptr, header, rawInput, bufferID)
	if err != nil {
		s.cgoMu.RUnlock()
		<-s.sem
		if errors.Is(err, ErrPanic) || errors.Is(err, ErrPoisoned) {
			s.poisoned.Store(true)
		}
		return 0, err
	}

	s.mu.Lock()
	s.semTickets[ticket] = struct{}{}
	// The reader can deliver this ticket before we get here: the worker ran
	// between gusset_submit returning and this insert. deliver stored the
	// result and left the permit, because the ticket was not in semTickets
	// yet. Give the permit back now so a fire-and-forget Submit does not hold
	// it for the life of the handle.
	releaseNow := false
	if _, done := s.completed[ticket]; done {
		delete(s.semTickets, ticket)
		releaseNow = true
	}
	s.mu.Unlock()
	s.cgoMu.RUnlock()
	if releaseNow {
		<-s.sem
	}

	return ticket, nil
}
