package isolate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/bharathvbcr/gusset"
)

// closeBudget matches the in-process Close join budget (DECISIONS
// 2026-09-30). Out of process the budget can end in a kill: the worker's
// memory is its own, so ending it frees nothing the host still reads.
const closeBudget = 30 * time.Second

// ErrBufferUnsupported is Submit's answer to a *gusset.Buffer. A Buffer is
// Rust memory in this process; carrying one to a worker needs the shared
// memory transport the spike stands in for.
var ErrBufferUnsupported = errors.New("isolate: *gusset.Buffer input needs a shared-memory transport; pass []byte")

// errProcClosed is what a waiter released by Close gets, with the text an
// in-process waiter released by the drain reader sees.
var errProcClosed error = &remoteError{msg: "gusset: handle closed", base: gusset.ErrClosed}

type result struct {
	out []byte
	err error
}

type ticketState struct {
	res       *result
	waiter    chan result
	abandoned bool
}

// Proc is the out-of-process Handle: the Caller contract served by a worker
// process running Serve. It keeps the in-process semantics a caller can
// observe — one waiter per ticket, a deadline that bounds the caller and not
// the work, abandoned and discarded tickets spent, poison latched after a
// panic, permits returned when a result lands — and adds one: a worker that
// dies, of anything, poisons this Proc and leaves the host running.
type Proc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	sem   chan struct{}

	wmu sync.Mutex // serializes frames on stdin

	mu       sync.Mutex
	next     uint64
	tickets  map[uint64]*ticketState
	closed   bool
	poisoned bool
	dead     error // latched worker exit; nil while it runs

	readerDone chan struct{}
	closeDone  chan struct{}
	closeErr   error
}

// Start runs cmd as a worker. cmd must run Serve on its stdin and stdout;
// Start owns both, and leaves cmd.Stderr as the caller set it. ctx bounds the
// handshake only: a worker that has not announced itself by then is killed.
func Start(ctx context.Context, cmd *exec.Cmd) (*Proc, error) {
	if ctx == nil {
		return nil, gusset.ErrNilContext
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	br := bufio.NewReader(stdout)

	hello := make(chan error, 1)
	var h response
	go func() { hello <- readFrame(br, maxResponseFrame, &h) }()
	select {
	case err = <-hello:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err == nil && (h.Op != opHello || h.Version != protocolVersion) {
		err = fmt.Errorf("isolate: worker announced op %q version %d, want %q version %d", h.Op, h.Version, opHello, protocolVersion)
	}
	if err == nil && (h.PoolSize < 1 || h.PoolSize > gusset.MaxPoolSize) {
		err = fmt.Errorf("isolate: worker announced pool size %d outside [1, %d]", h.PoolSize, gusset.MaxPoolSize)
	}
	if err != nil {
		_ = cmd.Process.Kill()
		_ = stdin.Close()
		_ = cmd.Wait()
		return nil, fmt.Errorf("isolate: worker handshake: %w", err)
	}

	p := &Proc{
		cmd:        cmd,
		stdin:      stdin,
		sem:        make(chan struct{}, h.PoolSize),
		tickets:    make(map[uint64]*ticketState),
		readerDone: make(chan struct{}),
		closeDone:  make(chan struct{}),
	}
	go p.read(br)
	return p, nil
}

// read delivers results until the worker's stdout ends, then latches the
// worker's death and releases every waiter with it.
func (p *Proc) read(br *bufio.Reader) {
	var cause error
	for {
		var resp response
		if err := readFrame(br, maxResponseFrame, &resp); err != nil {
			cause = err
			break
		}
		if !p.deliver(resp) {
			cause = fmt.Errorf("isolate: worker answered unknown id %d", resp.ID)
			_ = p.cmd.Process.Kill()
			break
		}
	}
	waitErr := p.cmd.Wait()

	p.mu.Lock()
	if !p.closed {
		p.dead = workerExitError(cause, waitErr)
	}
	for id, t := range p.tickets {
		if t.waiter != nil {
			err := p.dead
			if err == nil {
				err = errProcClosed
			}
			t.waiter <- result{err: err}
		}
		delete(p.tickets, id)
	}
	p.mu.Unlock()
	close(p.readerDone)
}

// workerExitError reports a worker that ended while the Proc was open. It
// matches gusset.ErrPoisoned: like a handle whose job panicked, this one takes
// no more work, and the recovery is the same — Close it and start another.
func workerExitError(cause, waitErr error) error {
	state := "exited"
	if waitErr != nil {
		state = waitErr.Error()
	}
	msg := "worker process " + state
	if cause != nil && !errors.Is(cause, io.EOF) {
		msg += " (" + cause.Error() + ")"
	}
	return &gusset.Error{
		Code: gusset.ErrPoisoned.Code,
		Msg:  msg + "; Close it and start a new worker",
	}
}

func (p *Proc) deliver(resp response) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.tickets[resp.ID]
	if !ok || t.res != nil {
		return false
	}
	// The work has stopped, so its permit comes back whether or not anyone
	// has waited (DECISIONS 2026-09-30), abandoned tickets included (I4).
	<-p.sem
	res := result{out: resp.Out}
	if resp.Err != nil {
		res.err = resp.Err.decode()
		if errors.Is(res.err, gusset.ErrPanic) || errors.Is(res.err, gusset.ErrPoisoned) {
			p.poisoned = true
		}
	}
	switch {
	case t.abandoned:
		delete(p.tickets, resp.ID)
	case t.waiter != nil:
		t.waiter <- res
		delete(p.tickets, resp.ID)
	default:
		t.res = &res
	}
	return true
}

// Call submits in and waits for its result.
func (p *Proc) Call(ctx context.Context, in []byte) ([]byte, error) {
	if p == nil {
		return nil, gusset.ErrNilHandle
	}
	if ctx == nil {
		return nil, gusset.ErrNilContext
	}
	ticket, err := p.submit(ctx, in)
	if err != nil {
		return nil, err
	}
	return p.Wait(ctx, ticket)
}

// Submit sends a job to the worker and returns its ticket. in is []byte (at
// most 4096 bytes, as in process) or nil.
func (p *Proc) Submit(ctx context.Context, in any) (uint64, error) {
	if p == nil {
		return 0, gusset.ErrNilHandle
	}
	if ctx == nil {
		return 0, gusset.ErrNilContext
	}
	switch v := in.(type) {
	case []byte:
		return p.submit(ctx, v)
	case nil:
		return p.submit(ctx, nil)
	case *gusset.Buffer:
		if v == nil {
			return 0, gusset.ErrNilBuffer
		}
		return 0, ErrBufferUnsupported
	default:
		return 0, errors.New("gusset: input must be []byte or *Buffer")
	}
}

func (p *Proc) submit(ctx context.Context, in []byte) (uint64, error) {
	if err := p.refusal(); err != nil {
		return 0, err
	}
	if len(in) > maxInlineInput {
		return 0, gusset.ErrInputTooLarge
	}
	req := request{Op: opSubmit, In: in}
	if err := readContext(ctx, &req); err != nil {
		return 0, err
	}

	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-p.readerDone:
		return 0, p.refusal()
	}
	// Stamped after the permit wait, so time queued here is not handed to
	// the worker as time the job may still run (R9, I3).
	req.TimeoutNS = relativeTimeout(ctx)

	p.mu.Lock()
	if err := p.refusalLocked(); err != nil {
		p.mu.Unlock()
		<-p.sem
		return 0, err
	}
	p.next++
	req.ID = p.next
	p.tickets[req.ID] = &ticketState{}
	p.mu.Unlock()

	if err := p.send(&req); err != nil {
		p.mu.Lock()
		if t, ok := p.tickets[req.ID]; ok && t.res == nil {
			delete(p.tickets, req.ID)
			<-p.sem
		}
		p.mu.Unlock()
		// A failed write means the worker is gone; the reader latches why.
		<-p.readerDone
		if refusal := p.refusal(); refusal != nil {
			return 0, refusal
		}
		return 0, err
	}
	return req.ID, nil
}

func (p *Proc) send(req *request) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	return writeFrame(p.stdin, req, maxRequestFrame)
}

func (p *Proc) refusal() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refusalLocked()
}

// refusalLocked is the closed, dead, poisoned order of the in-process submit:
// closed first, so a caller whose policy is "on poison, close and reopen" is
// not sent back to close a handle it already closed.
func (p *Proc) refusalLocked() error {
	switch {
	case p.closed:
		return gusset.ErrClosed
	case p.dead != nil:
		return p.dead
	case p.poisoned:
		return &gusset.Error{
			Code: gusset.ErrPoisoned.Code,
			Msg:  "handle is poisoned: a job panicked; Close it and open a new handle",
		}
	}
	return nil
}

// Wait waits for ticket's result. On ctx.Done it cancels the job
// best-effort, gives up the ticket and returns ctx.Err() at once; the result,
// when it lands, is dropped.
func (p *Proc) Wait(ctx context.Context, ticket uint64) ([]byte, error) {
	if p == nil {
		return nil, gusset.ErrNilHandle
	}
	if ctx == nil {
		return nil, gusset.ErrNilContext
	}
	p.mu.Lock()
	t, ok := p.tickets[ticket]
	switch {
	case !ok && p.closed:
		p.mu.Unlock()
		return nil, gusset.ErrClosed
	case !ok && p.dead != nil:
		p.mu.Unlock()
		return nil, p.dead
	case !ok || t.abandoned:
		p.mu.Unlock()
		return nil, gusset.ErrUnknownTicket
	case t.waiter != nil:
		p.mu.Unlock()
		return nil, gusset.ErrTicketBusy
	case t.res != nil:
		delete(p.tickets, ticket)
		p.mu.Unlock()
		return t.res.out, t.res.err
	}
	ch := make(chan result, 1)
	t.waiter = ch
	p.mu.Unlock()

	select {
	case r := <-ch:
		return r.out, r.err
	case <-ctx.Done():
	}
	p.mu.Lock()
	select {
	case r := <-ch: // delivered while the deadline fired
		p.mu.Unlock()
		return r.out, r.err
	default:
	}
	t.waiter = nil
	t.abandoned = true
	p.mu.Unlock()
	// Best effort: a lost cancel only means the work runs out its own
	// deadline, and the permit stays with it until it does.
	_ = p.send(&request{Op: opCancel, ID: ticket})
	return nil, ctx.Err()
}

// Discard gives up ticket's result without cancelling the work.
func (p *Proc) Discard(ticket uint64) error {
	if p == nil {
		return gusset.ErrNilHandle
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return gusset.ErrClosed
	}
	t, ok := p.tickets[ticket]
	switch {
	case !ok || t.abandoned:
		return gusset.ErrUnknownTicket
	case t.waiter != nil:
		return gusset.ErrTicketBusy
	case t.res != nil:
		delete(p.tickets, ticket)
	default:
		t.abandoned = true
	}
	return nil
}

// Close ends the worker: it releases every waiter with ErrClosed, closes the
// worker's stdin so Serve cancels and answers its in-flight work, and waits
// for the worker to exit. A worker still running after the close budget is
// killed and Close reports it. A second Close waits for the first and returns
// its result.
func (p *Proc) Close() error {
	if p == nil {
		return gusset.ErrNilHandle
	}
	return p.close(closeBudget)
}

func (p *Proc) close(budget time.Duration) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.closeDone
		return p.closeErr
	}
	p.closed = true
	for _, t := range p.tickets {
		if t.waiter != nil {
			t.waiter <- result{err: errProcClosed}
			t.waiter = nil
		}
		t.abandoned = true
	}
	p.mu.Unlock()
	defer close(p.closeDone)

	p.wmu.Lock()
	_ = p.stdin.Close()
	p.wmu.Unlock()

	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-p.readerDone:
	case <-timer.C:
		_ = p.cmd.Process.Kill()
		<-p.readerDone
		p.closeErr = fmt.Errorf("isolate: worker did not exit within %v of Close and was killed", budget)
	}
	return p.closeErr
}

func readContext(ctx context.Context, req *request) error {
	if sc, ok := ctx.Value(gusset.SpanContextKey).(gusset.TraceCarrier); ok && sc != nil {
		var trace [24]byte
		tid, sid := sc.TraceID(), sc.SpanID()
		copy(trace[:16], tid[:])
		copy(trace[16:], sid[:])
		req.Trace = &trace
	}
	if v := ctx.Value(gusset.OpcodeContextKey); v != nil {
		op, ok := v.(uint32)
		if !ok {
			return fmt.Errorf("isolate: opcode context value must be uint32 (gusset.ContextWithOpcode), got %T", v)
		}
		req.Opcode = &op
	}
	return nil
}

func relativeTimeout(ctx context.Context) uint64 {
	if ctx.Err() != nil {
		return 1
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	if remaining := time.Until(deadline); remaining > 0 {
		return uint64(remaining.Nanoseconds())
	}
	return 1
}
