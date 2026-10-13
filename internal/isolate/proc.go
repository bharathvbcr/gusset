package isolate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
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

// waitDelay bounds cmd.Wait once the worker is dead: a grandchild that
// inherited one of the worker's stderr or stdout pipes cannot hold it open.
const waitDelay = 500 * time.Millisecond

// protocolDrain is how long the process reaper waits, after the worker has
// exited, for the protocol reader to finish on its own. A clean exit closes
// the write end and the reader sees EOF inside this window. Past it, a
// grandchild still holding the write end would leave every waiter parked,
// so the reaper unblocks the read. The reader then takes one more pass so
// bytes already queued are not discarded for that deadline.
const protocolDrain = 200 * time.Millisecond

// queuedFrameProbe is the second read after that unblock. Bytes already in
// the pipe return immediately; the probe only expires once a read would block,
// which is the grandchild still holding the write end.
const queuedFrameProbe = 50 * time.Millisecond

// cancelSendBudget bounds Wait's best-effort cancel. context.Background has
// no deadline and no Done channel, so a cancel that acquired the write lock
// on a full stdin pipe would block inside the write until Close and pin the
// lock for the life of the Proc. One second still lands the frame when the
// pipe is only briefly full. It is not the 24h poller arm, and a write that
// actually hits it poisons the Proc instead of holding the lock.
const cancelSendBudget = time.Second

// EnvProtocolFD names the environment variable Start sets for the worker: the
// descriptor number of the pipe the worker writes its protocol frames to. The
// protocol is off stdout so an engine's own prints (println!, a log line)
// cannot corrupt the framing.
const EnvProtocolFD = "GUSSET_ISOLATE_PROTOCOL_FD"

// ProtocolOut is the worker half's writer for Serve: the protocol pipe Start
// passed this process. Stdout stays the engine's own.
func ProtocolOut() (*os.File, error) {
	v := os.Getenv(EnvProtocolFD)
	fd, err := strconv.Atoi(v)
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("isolate: %s=%q is not a protocol descriptor; was this process started by isolate.Start?", EnvProtocolFD, v)
	}
	f := os.NewFile(uintptr(fd), "gusset-protocol")
	if f == nil {
		return nil, fmt.Errorf("isolate: protocol descriptor %d is not open", fd)
	}
	// The descriptor is inherited from Start with close-on-exec clear, so an
	// engine that execs would leave it open in the child and the host would
	// never see EOF. Closing it in this process is not what we want: Serve
	// still writes it. CLOEXEC drops it only across exec.
	syscall.CloseOnExec(fd)
	return f, nil
}

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
	stdin *os.File // write end; a deadline can interrupt a full-pipe write
	rd    *os.File // read end of the protocol pipe; closed to unwedge the reader
	sem   chan struct{}

	// writeMu serializes frames on stdin. A channel, not a mutex, so a send
	// whose context ends can return without waiting out a writer already
	// blocked in the pipe (I3).
	writeMu chan struct{}

	mu       sync.Mutex
	next     uint64
	tickets  map[uint64]*ticketState
	closed   bool
	poisoned bool
	dead     error // latched worker exit; nil while it runs
	waitErr  error // cmd.Wait's result; published before procExited is closed

	readerDone chan struct{}
	procExited chan struct{} // closed after the one cmd.Wait returns
	closeDone  chan struct{}
	closeErr   error
}

// Start runs cmd as a worker. cmd must run Serve reading its stdin and
// writing ProtocolOut; Start owns stdin and appends the protocol pipe to
// cmd.ExtraFiles, naming its descriptor in EnvProtocolFD. cmd.Stdout and
// cmd.Stderr stay as the caller set them. ctx bounds the handshake only: a
// worker that has not announced itself by then is killed.
func Start(ctx context.Context, cmd *exec.Cmd) (*Proc, error) {
	if ctx == nil {
		return nil, gusset.ErrNilContext
	}
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdin, ok := stdinPipe.(*os.File)
	if !ok {
		_ = stdinPipe.Close()
		return nil, fmt.Errorf("isolate: stdin pipe is %T, want *os.File", stdinPipe)
	}
	rd, wr, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, EnvProtocolFD+"="+strconv.Itoa(3+len(cmd.ExtraFiles)))
	cmd.ExtraFiles = append(cmd.ExtraFiles, wr)
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = waitDelay
	}
	err = cmd.Start()
	_ = wr.Close() // the child holds the only write end that matters
	if err != nil {
		_ = rd.Close()
		_ = stdin.Close()
		return nil, err
	}
	br := bufio.NewReader(rd)

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
		_ = rd.Close()
		_ = stdin.Close()
		_ = cmd.Wait()
		return nil, fmt.Errorf("isolate: worker handshake: %w", err)
	}

	p := &Proc{
		cmd:        cmd,
		stdin:      stdin,
		rd:         rd,
		sem:        make(chan struct{}, h.PoolSize),
		writeMu:    make(chan struct{}, 1),
		tickets:    make(map[uint64]*ticketState),
		readerDone: make(chan struct{}),
		procExited: make(chan struct{}),
		closeDone:  make(chan struct{}),
	}
	// Exactly one goroutine calls cmd.Wait. It runs beside the reader: Wait
	// is what starts WaitDelay, and a grandchild holding the protocol write
	// end would otherwise keep readFrame from ever returning, so Wait would
	// never be reached.
	go p.waitProc()
	go p.read(br)
	return p, nil
}

// waitProc is the only cmd.Wait after a successful Start. Once the worker
// has exited, it gives the reader protocolDrain to deliver frames already
// written, then unblocks a read that cannot reach EOF.
func (p *Proc) waitProc() {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.waitErr = err
	p.mu.Unlock()
	close(p.procExited)

	timer := time.NewTimer(protocolDrain)
	select {
	case <-p.readerDone:
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	case <-timer.C:
		if derr := p.rd.SetReadDeadline(time.Now()); derr != nil {
			// A fd that cannot take a deadline still has to wake the reader.
			_ = p.rd.Close()
		}
	}
}

// read delivers results until the protocol stream ends, then latches the
// worker's death and releases every waiter with it. It does not call
// cmd.Wait; waitProc owns that, including after a Kill from this loop.
func (p *Proc) read(initial *bufio.Reader) {
	// bufio keeps a sticky error and serves it before the next file read, so
	// a past deadline drops a frame already sitting in the pipe. streamSrc
	// holds those bytes across a fresh reader.
	src := &streamSrc{r: p.rd}
	br := detachBufio(initial, src)
	var cause error
	probed := false
	for {
		var resp response
		err := readFrame(br, maxResponseFrame, &resp)
		if err != nil && !probed && errors.Is(err, os.ErrDeadlineExceeded) {
			probed = true
			var incomplete *frameError
			errors.As(err, &incomplete)
			br = detachBufio(br, src)
			if incomplete != nil && len(incomplete.consumed) > 0 {
				kept := make([]byte, len(incomplete.consumed)+len(src.pending))
				copy(kept, incomplete.consumed)
				copy(kept[len(incomplete.consumed):], src.pending)
				src.pending = kept
			}
			_ = p.rd.SetReadDeadline(time.Now().Add(queuedFrameProbe))
			continue
		}
		if err != nil {
			cause = err
			p.killIfAlive()
			break
		}
		if !p.deliver(resp) {
			cause = fmt.Errorf("isolate: worker answered unknown id %d", resp.ID)
			p.killIfAlive()
			break
		}
	}
	_ = p.rd.Close()
	<-p.procExited

	p.mu.Lock()
	waitErr := p.waitErr
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

// killIfAlive ends a worker that is still running after the protocol stream
// failed. A worker that has already exited is reaped by waitProc; Kill here
// would only race that Wait.
func (p *Proc) killIfAlive() {
	select {
	case <-p.procExited:
	default:
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	}
}

// streamSrc is the protocol pipe plus bytes peeled out of a bufio.Reader
// before that reader is thrown away. A new bufio.Reader does not inherit the
// sticky deadline.
type streamSrc struct {
	pending []byte
	r       io.Reader
}

func (s *streamSrc) Read(p []byte) (int, error) {
	if len(s.pending) > 0 {
		n := copy(p, s.pending)
		s.pending = s.pending[n:]
		return n, nil
	}
	return s.r.Read(p)
}

// detachBufio copies br's unread bytes onto src and returns a reader with no
// sticky error. Peek does not consume, so the copy is the only record of them.
func detachBufio(br *bufio.Reader, src *streamSrc) *bufio.Reader {
	if n := br.Buffered(); n > 0 {
		if peeked, _ := br.Peek(n); len(peeked) > 0 {
			kept := make([]byte, len(peeked)+len(src.pending))
			copy(kept, peeked)
			copy(kept[len(peeked):], src.pending)
			src.pending = kept
		}
	}
	return bufio.NewReader(src)
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

	if err := p.send(ctx, &req); err != nil {
		p.mu.Lock()
		if t, ok := p.tickets[req.ID]; ok && t.res == nil {
			if t.waiter != nil {
				t.waiter <- result{err: err}
				t.waiter = nil
			}
			delete(p.tickets, req.ID)
			<-p.sem
		}
		p.mu.Unlock()
		// The caller's deadline is the answer when the write itself could not
		// finish in time, whether or not the reader has latched the worker.
		// A write that did not fail that way still waits for the reader: the
		// refusal carries the worker's exit, which is the caller's error.
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		select {
		case <-p.readerDone:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		if refusal := p.refusal(); refusal != nil {
			return 0, refusal
		}
		return 0, err
	}
	return req.ID, nil
}

// send writes one frame. A context that ends while this call is waiting for
// writeMu, or during the write, returns ctx.Err() without waiting out another
// writer. A write that hits that deadline may have been partial: the Proc is
// poisoned and stdin is closed so the stream cannot continue and every writer
// blocked in the pipe unblocks (I3, I4).
func (p *Proc) send(ctx context.Context, req *request) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case p.writeMu <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.writeMu }()
	return p.writeLocked(ctx, req)
}

func (p *Proc) writeLocked(ctx context.Context, req *request) error {
	if err := ctx.Err(); err != nil {
		// The lock was acquired after the context ended, and nothing was written.
		return err
	}
	// A deadline only interrupts a write that is already in the poller. Arm
	// one before Write, and let cancellation move it into the past: a mutex
	// around the write used to hide both from the caller.
	arm := false
	if d, ok := ctx.Deadline(); ok {
		if err := p.stdin.SetWriteDeadline(d); err != nil {
			return err
		}
		arm = true
	} else if ctx.Done() != nil {
		if err := p.stdin.SetWriteDeadline(time.Now().Add(24 * time.Hour)); err != nil {
			return err
		}
		arm = true
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	if ctx.Done() != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
				_ = p.stdin.SetWriteDeadline(time.Unix(1, 0))
			case <-stop:
			}
		}()
	}
	err := writeFrame(p.stdin, req, maxRequestFrame)
	close(stop)
	wg.Wait()
	if arm {
		_ = p.stdin.SetWriteDeadline(time.Time{})
	}
	if err != nil && (ctx.Err() != nil || errors.Is(err, os.ErrDeadlineExceeded)) {
		p.poisonStream()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return context.DeadlineExceeded
	}
	return err
}

// poisonStream latches poison and closes stdin. A partial frame must not be
// followed by another, and closing the pipe is what unblocks a write stuck
// in the kernel. Closing an already-closed pipe is an error from os.File and
// does not close the descriptor twice.
func (p *Proc) poisonStream() {
	p.mu.Lock()
	if !p.closed {
		p.poisoned = true
	}
	p.mu.Unlock()
	_ = p.stdin.Close()
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
	// closed before abandoned: close marks every ticket abandoned under the
	// same lock that sets closed, and a Wait that arrives in that window
	// must match ErrClosed, not ErrUnknownTicket. The same order as refusal.
	case p.closed:
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
	// Best effort, and not on this goroutine. The cancel takes writeMu; doing
	// that here would make the caller's deadline wait on a writer already in
	// the pipe (I3). Its own budget is cancelSendBudget: Background never
	// arms a write deadline, so this goroutine would otherwise pin writeMu
	// until Close. Waiting out the budget on the lock, with nothing written,
	// does not poison; a write that hits the deadline does.
	go func() {
		cctx, cancel := context.WithTimeout(context.Background(), cancelSendBudget)
		defer cancel()
		_ = p.send(cctx, &request{Op: opCancel, ID: ticket})
	}()
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

	// The budget starts before anything that can block. stdin is closed
	// without writeMu: a writer parked on a full pipe holds it, and Close
	// ending the pipe is what releases that writer.
	timer := time.NewTimer(budget)
	defer timer.Stop()
	_ = p.stdin.Close()
	select {
	case <-p.readerDone:
	case <-timer.C:
		_ = p.cmd.Process.Kill()
		_ = p.rd.Close()
		<-p.readerDone
		p.closeErr = fmt.Errorf("isolate: worker did not exit within %v of Close and was killed", budget)
	}
	return p.closeErr
}

func readContext(ctx context.Context, req *request) error {
	if sc, ok := ctx.Value(gusset.SpanContextKey).(gusset.TraceCarrier); ok && sc != nil {
		// `sc != nil` is true for an interface holding a typed nil. gusset's
		// readTraceCarrier recovers the same panic; it is unexported, and a
		// second public entry point is not part of this contract, so the
		// recover is duplicated here. A reflect nil check would refuse a
		// carrier whose methods are nil-safe.
		var trace [24]byte
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("gusset: trace carrier %T panicked: %v", sc, r)
				}
			}()
			tid, sid := sc.TraceID(), sc.SpanID()
			copy(trace[:16], tid[:])
			copy(trace[16:], sid[:])
			return nil
		}()
		if err != nil {
			return err
		}
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
