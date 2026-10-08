package isolate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/bharathvbcr/gusset"
)

var (
	contextDeadlineExceeded = context.DeadlineExceeded
	contextCanceled         = context.Canceled
)

// Caller is the byte-oriented Handle contract both modes keep: *gusset.Handle
// in process, *Proc out of process. The Buffer methods (NewBuffer,
// CallBuffer, WaitBuffer) are not in it: a *gusset.Buffer is Rust memory
// mapped into this process, and the out-of-process equivalent is the shared
// memory segment iceoryx2 would provide.
type Caller interface {
	Call(ctx context.Context, in []byte) ([]byte, error)
	Submit(ctx context.Context, in any) (uint64, error)
	Wait(ctx context.Context, ticket uint64) ([]byte, error)
	Discard(ticket uint64) error
	Close() error
}

var (
	_ Caller = (*gusset.Handle)(nil)
	_ Caller = (*Proc)(nil)
)

// Serve is the worker half. It announces poolSize, then runs each submit
// frame read from r on h and writes its result to w, in completion order. It
// returns when r ends — the host closed the pipe or exited — after cancelling
// and answering everything still in flight, or on the first malformed frame.
// The caller still owns h and closes it.
//
// poolSize must be h's pool size: the host bounds its in-flight submissions
// by it, so the worker's own semaphore never queues (R11).
func Serve(r io.Reader, w io.Writer, h Caller, poolSize int) error {
	if poolSize < 1 || poolSize > gusset.MaxPoolSize {
		return fmt.Errorf("isolate: pool size %d outside [1, %d]", poolSize, gusset.MaxPoolSize)
	}
	var wmu sync.Mutex
	send := func(resp *response) error {
		wmu.Lock()
		defer wmu.Unlock()
		return writeFrame(w, resp, maxResponseFrame)
	}
	if err := send(&response{Op: opHello, Version: protocolVersion, PoolSize: poolSize}); err != nil {
		return err
	}

	var (
		mu      sync.Mutex
		cancels = make(map[uint64]context.CancelFunc)
		wg      sync.WaitGroup
		sendErr error
	)
	stop := func() {
		mu.Lock()
		for _, cancel := range cancels {
			cancel()
		}
		mu.Unlock()
		wg.Wait()
	}

	br := bufio.NewReader(r)
	for {
		var req request
		if err := readFrame(br, maxRequestFrame, &req); err != nil {
			stop()
			if errors.Is(err, io.EOF) {
				return sendErr
			}
			return err
		}
		switch req.Op {
		case opSubmit:
			ctx, cancel := requestContext(req)
			mu.Lock()
			if _, dup := cancels[req.ID]; dup {
				mu.Unlock()
				cancel()
				stop()
				return fmt.Errorf("isolate: duplicate in-flight id %d", req.ID)
			}
			cancels[req.ID] = cancel
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, err := run(ctx, h, req.In)
				mu.Lock()
				delete(cancels, req.ID)
				mu.Unlock()
				cancel()
				if err := send(&response{ID: req.ID, Out: out, Err: encodeError(err)}); err != nil {
					mu.Lock()
					sendErr = errors.Join(sendErr, err)
					mu.Unlock()
				}
			}()
		case opCancel:
			mu.Lock()
			if cancel, ok := cancels[req.ID]; ok {
				cancel()
			}
			mu.Unlock()
		default:
			stop()
			return fmt.Errorf("isolate: unknown op %q", req.Op)
		}
	}
}

// run is Call through Submit and Wait, so a cancel frame reaches a job that is
// already running: Wait with a cancelled context cancels it in Rust.
func run(ctx context.Context, h Caller, in []byte) ([]byte, error) {
	ticket, err := h.Submit(ctx, in)
	if err != nil {
		return nil, err
	}
	return h.Wait(ctx, ticket)
}

// wireTrace is a TraceCarrier rebuilt from a request's trace field.
type wireTrace [24]byte

func (t wireTrace) TraceID() (id [16]byte) { copy(id[:], t[:16]); return id }
func (t wireTrace) SpanID() (id [8]byte)   { copy(id[:], t[16:]); return id }

func requestContext(req request) (context.Context, context.CancelFunc) {
	ctx := context.Background()
	if req.Opcode != nil {
		ctx = gusset.ContextWithOpcode(ctx, *req.Opcode)
	}
	if req.Trace != nil {
		ctx = context.WithValue(ctx, gusset.SpanContextKey, wireTrace(*req.Trace))
	}
	if req.TimeoutNS > 0 {
		return context.WithTimeout(ctx, time.Duration(min(req.TimeoutNS, uint64(1<<63-1))))
	}
	return context.WithCancel(ctx)
}
