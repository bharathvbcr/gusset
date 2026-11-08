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
	"strings"
	"sync"
	"sync/atomic"
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
// bytes already queued are not discarded for that deadline. The window
// restarts while frames are still being delivered: it ends only after a
// whole protocolDrain in which none was. That is bounded, because no ticket
// is registered once the worker has exited (refusalLocked), so at most the
// tickets in flight at the exit can each buy one more window.
const protocolDrain = 200 * time.Millisecond

// queuedFrameProbe is the read deadline after that unblock, re-armed after
// each frame it delivers. Bytes already in the pipe return immediately; the
// probe only expires once no frame has arrived for this long, which is the
// grandchild still holding the write end.
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
	// os.NewFile returns nil only for a negative descriptor, which fd < 3
	// already refused.
	f := os.NewFile(uintptr(fd), "gusset-protocol")
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
	// cancelled is set when Wait gave the ticket up and sent the cancel
	// frame. Discard abandons without cancelling and is not counted: the
	// work it left behind is expected to run on.
	cancelled bool
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

	// frames counts frames the reader has delivered. waitProc restarts its
	// drain window while it moves.
	frames atomic.Uint64

	readerDone chan struct{}
	procExited chan struct{} // closed after the one cmd.Wait returns
	closeDone  chan struct{}
	closeErr   error

	// retire is a buffer-1 signal for a supervisor, nil when this Proc has
	// none. deliver and poisonStream send once under mu. The supervisor's
	// goroutine kills; the goroutine inside writeLocked does not.
	retire      chan struct{}
	retireSent  bool
	retireCause string

	// pgid is the process group start created for the worker (its own pid),
	// or 0 when the group is not one this package owns. killTree signals it
	// only when it is set.
	pgid int

	// stall is the supervisor's liveness bound, 0 when there is none.
	// stallArmed is true while a judgement timer is pending, so a ticket
	// that is dropped and re-abandoned cannot stack a second one.
	stall      time.Duration
	stallArmed bool

	// stdinOnce makes the retirement close and poisonStream's close the same
	// close. A second Close on an *os.File errors and does not close the
	// descriptor twice; Once also keeps those two paths from racing.
	stdinOnce sync.Once

	// killGate is a test seam: when set, killIfAlive runs it before it looks at
	// whether the worker has exited. It lets a test hold the reader until
	// waitProc has reaped the leader, the order in which killIfAlive skips the
	// kill and the group is left to whoever else kills it. Nil outside tests;
	// atomic because the reader is already running when a test sets it.
	killGate atomic.Pointer[func()]
}

// Start runs cmd as a worker. cmd must run Serve reading its stdin and
// writing ProtocolOut; Start owns stdin and appends the protocol pipe to
// cmd.ExtraFiles, naming its descriptor in EnvProtocolFD. An existing
// EnvProtocolFD entry is replaced, so a factory that copied a previous
// command's environment cannot leave the child pointed at a dead descriptor.
// cmd.Stdout and cmd.Stderr stay as the caller set them. ctx bounds the
// handshake only: a worker that has not announced itself by then is killed.
func Start(ctx context.Context, cmd *exec.Cmd) (*Proc, error) {
	return start(ctx, cmd, nil, 0)
}

// start is Start with an optional supervisor signal. retire is nil for a Proc
// that poisons in place. waitProc is the only cmd.Wait after a successful
// return; the handshake failure path reaps the process that never became a
// Proc. stall is the supervisor's StallTimeout; 0 turns the liveness rule off,
// as it is for a bare Proc.
func start(ctx context.Context, cmd *exec.Cmd, retire chan struct{}, stall time.Duration) (*Proc, error) {
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
	// Drop every previous value, then append the descriptor this Start just
	// created. Getenv returns the first match, so a stale entry left in front
	// would win over one appended after it.
	cmd.Env = replaceEnv(cmd.Env, EnvProtocolFD, strconv.Itoa(3+len(cmd.ExtraFiles)))
	cmd.ExtraFiles = append(cmd.ExtraFiles, wr)
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = waitDelay
	}
	ownProcessGroup(cmd)
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
	pgid := ownedGroup(cmd)
	if err != nil {
		killTree(cmd, pgid)
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
		retire:     retire,
		stall:      stall,
		pgid:       pgid,
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
// written, restarting that window while frames keep arriving, then unblocks
// a read that cannot reach EOF.
func (p *Proc) waitProc() {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.waitErr = err
	p.mu.Unlock()
	close(p.procExited)

	seen := p.frames.Load()
	timer := time.NewTimer(protocolDrain)
	defer timer.Stop()
	for {
		select {
		case <-p.readerDone:
			return
		case <-timer.C:
		}
		if n := p.frames.Load(); n != seen {
			seen = n
			timer.Reset(protocolDrain)
			continue
		}
		if derr := p.rd.SetReadDeadline(time.Now()); derr != nil {
			// A fd that cannot take a deadline still has to wake the reader.
			_ = p.rd.Close()
		}
		return
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
		p.frames.Add(1)
		if probed {
			// The probe is a gap limit, not a total: a frame just landed, so
			// the next one gets a whole window. Armed after deliver, so time
			// spent on the lock is not taken from it.
			_ = p.rd.SetReadDeadline(time.Now().Add(queuedFrameProbe))
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
		// A stored result is finished work: Wait still returns it, as a
		// poisoned in-process handle delivers one (DECISIONS 2026-09-22). Its
		// permit came back when it was stored. Close drops it.
		if t.res != nil && !t.abandoned && !p.closed {
			continue
		}
		delete(p.tickets, id)
	}
	p.mu.Unlock()
	close(p.readerDone)
}

// exited reports whether waitProc has reaped the worker. waitErr is published
// under mu before procExited is closed, so a caller holding mu that sees true
// also sees it.
func (p *Proc) exited() bool {
	select {
	case <-p.procExited:
		return true
	default:
		return false
	}
}

// killIfAlive ends a worker that is still running after the protocol stream
// failed. A worker that has already exited is reaped by waitProc; Kill here
// would only race that Wait.
func (p *Proc) killIfAlive() {
	if gate := p.killGate.Load(); gate != nil {
		(*gate)()
	}
	if !p.exited() {
		p.killTree()
	}
}

// ownProcessGroup puts the worker in a process group of its own, so a kill can
// reach the helpers it spawned and not only the worker. A command that already
// asked for a session (Setsid) is its own group leader and is left alone; one
// that asked to join another group (Pgid != 0) keeps that choice, and
// ownedGroup then reports no group this package may signal.
//
// Setpgid takes the worker out of the host's foreground process group. A
// terminal's Ctrl-C (SIGINT) and job-control signals go to the foreground
// group, so they no longer reach the worker directly: it learns the host is
// going away from EOF on stdin, and the host's Close or a retirement ends it.
func ownProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if cmd.SysProcAttr.Setsid || cmd.SysProcAttr.Pgid != 0 {
		return
	}
	cmd.SysProcAttr.Setpgid = true
}

// ownedGroup is the process group id killTree may signal: the started worker's
// own pid, when this package or the caller made it a group leader. It is 0
// when the pid is unknown, is 1 or less, or is the host's own group. A group
// signal to any of those is never sent.
func ownedGroup(cmd *exec.Cmd) int {
	if cmd.Process == nil || cmd.SysProcAttr == nil {
		return 0
	}
	if !cmd.SysProcAttr.Setsid && !(cmd.SysProcAttr.Setpgid && cmd.SysProcAttr.Pgid == 0) {
		return 0
	}
	pid := cmd.Process.Pid
	if pid <= 1 || pid == syscall.Getpgrp() {
		return 0
	}
	return pid
}

// killTree ends the worker and everything left in its group. The leader is
// always killed; the group only when pgid is one ownedGroup vouched for.
func killTree(cmd *exec.Cmd, pgid int) {
	if pgid > 1 && pgid != syscall.Getpgrp() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// killTree is the supervisor's and Close's kill. After the leader has exited
// the group may still hold descendants, and that is what the group signal is
// for. A group keeps its id from being reused only while it has members: once
// the leader is reaped and the group is empty, the number is free, and a signal
// sent then could reach an unrelated group given the same id (DECISIONS
// 2026-11-07 and 2026-11-08). Callers therefore signal soon after the exit and
// never hold a group id for later.
func (p *Proc) killTree() {
	killTree(p.cmd, p.pgid)
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
			// Once per generation. A later ErrPoisoned from a sibling job is
			// the same retirement, not another signal.
			p.notifyRetireLocked("engine poison")
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
	id, _, err := p.submitCounted(ctx, in)
	return id, err
}

// submitCounted is submit that also says whether the request was certainly not
// written to the worker. unsent is true for every refusal before the frame is
// sent (closed, dead, poisoned, a context that ended while queued for a permit,
// an oversized input). It is false once a write began, because a write that
// failed may still have delivered bytes. A supervisor retries on a replacement
// only when unsent is true.
func (p *Proc) submitCounted(ctx context.Context, in []byte) (id uint64, unsent bool, err error) {
	if err := p.refusal(); err != nil {
		return 0, true, err
	}
	if len(in) > maxInlineInput {
		return 0, true, gusset.ErrInputTooLarge
	}
	req := request{Op: opSubmit, In: in}
	if err := readContext(ctx, &req); err != nil {
		return 0, true, err
	}

	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return 0, true, ctx.Err()
	case <-p.readerDone:
		return 0, true, p.refusal()
	}
	// Stamped after the permit wait, so time queued here is not handed to
	// the worker as time the job may still run (R9, I3).
	req.TimeoutNS = relativeTimeout(ctx)

	p.mu.Lock()
	if err := p.refusalLocked(); err != nil {
		p.mu.Unlock()
		<-p.sem
		return 0, true, err
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
			return 0, false, ctx.Err()
		}
		select {
		case <-p.readerDone:
		case <-ctx.Done():
			return 0, false, ctx.Err()
		}
		if refusal := p.refusal(); refusal != nil {
			return 0, false, refusal
		}
		return 0, false, err
	}
	return req.ID, false, nil
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
// in the kernel. The notify is under the lock this function already holds.
// Killing the child, when a supervisor is attached, is that supervisor's
// goroutine: the caller is writeLocked, and the goroutine it arms only moves
// the write deadline.
func (p *Proc) poisonStream() {
	p.mu.Lock()
	if !p.closed {
		p.poisoned = true
	}
	p.notifyRetireLocked("cut frame")
	p.mu.Unlock()
	p.closeStdin()
}

// notifyRetireLocked sends once on the supervisor signal. The channel is
// buffer-1 and the send does not block; a second call is a no-op. The caller
// holds p.mu.
func (p *Proc) notifyRetireLocked(cause string) {
	if p.retire == nil || p.retireSent {
		return
	}
	p.retireSent = true
	if cause != "" {
		p.retireCause = cause
	}
	select {
	case p.retire <- struct{}{}:
	default:
	}
}

// judgeStallLocked is the liveness rule, beside the reader and not in it. It
// runs when Wait abandons a ticket. If every pool permit is now held by a
// cancelled ticket nothing can be submitted, so it arms one timer for stall.
// When the timer fires, the generation is unresponsive only if no frame was
// delivered since it was armed and the permits are still all held. A frame
// inside the window restarts the judgement: if the permits are all held again
// when the timer fires, the timer arms a fresh window itself, because with
// every permit stranded no Submit gets through and no Wait can abandon to arm
// it. The caller holds p.mu. Without a supervisor (retire nil) or a stall
// bound it does nothing.
func (p *Proc) judgeStallLocked() {
	if p.retire == nil || p.stall <= 0 || p.stallArmed || p.retireSent || !p.stalledLocked() {
		return
	}
	p.armStallLocked()
}

// armStallLocked starts one stall window. The caller holds p.mu and has
// checked that the generation is stalled and not yet signalled.
func (p *Proc) armStallLocked() {
	p.stallArmed = true
	seen := p.frames.Load()
	time.AfterFunc(p.stall, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.stallArmed = false
		if p.closed || p.dead != nil || p.retireSent || !p.stalledLocked() {
			return
		}
		if p.frames.Load() != seen {
			p.armStallLocked()
			return
		}
		p.notifyRetireLocked("unresponsive")
	})
}

// stalledLocked reports whether cancelled tickets hold every permit.
func (p *Proc) stalledLocked() bool {
	n := 0
	for _, t := range p.tickets {
		if t.cancelled && t.res == nil {
			n++
		}
	}
	return n >= cap(p.sem)
}

func (p *Proc) retireReason() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retireCause != "" {
		return p.retireCause
	}
	return "poison"
}

// closeStdin closes the worker's stdin once. poisonStream, retirement, and
// Close all end here; the second and third calls do not close the descriptor
// again.
func (p *Proc) closeStdin() {
	if p == nil || p.stdin == nil {
		return
	}
	p.stdinOnce.Do(func() { _ = p.stdin.Close() })
}

// usable reports whether a new submission can still be sent to this process.
// A supervisor uses it to avoid handing a caller a generation that has
// already been poisoned or reaped. The caller may hold the supervisor lock;
// this takes p.mu and nothing else.
func (p *Proc) usable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.closed && !p.poisoned && p.dead == nil && !p.exited()
}

func (p *Proc) refusal() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refusalLocked()
}

// refusalLocked is the closed, dead, poisoned order of the in-process submit:
// closed first, so a caller whose policy is "on poison, close and reopen" is
// not sent back to close a handle it already closed. A worker that has exited
// refuses new work before the reader has latched it: the drain window is for
// answers already owed, and a ticket registered now could only be answered by
// a descendant still holding the pipes, which would also keep extending it.
func (p *Proc) refusalLocked() error {
	switch {
	case p.closed:
		return gusset.ErrClosed
	case p.dead != nil:
		return p.dead
	case p.exited():
		return workerExitError(nil, p.waitErr)
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
	t.cancelled = true
	p.judgeStallLocked()
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
	for id, t := range p.tickets {
		if t.res != nil {
			// Stored and never collected; a closed Proc returns ErrClosed to
			// its Wait. Its permit came back when it was stored.
			delete(p.tickets, id)
			continue
		}
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
	p.closeStdin()
	select {
	case <-p.readerDone:
	case <-timer.C:
		// The group, not only the worker: the reader's killIfAlive skips a
		// worker that waitProc has already reaped, so a helper the worker
		// spawned would outlive this Close if the kill here left it alone.
		p.killTree()
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

// replaceEnv drops every entry of key and appends key=value. The worker's
// getenv returns the first match, so a stale descriptor left in front of a
// newly appended one would be the one the child opens.
func replaceEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		k, _, ok := strings.Cut(e, "=")
		if ok && k == key {
			continue
		}
		out = append(out, e)
	}
	return append(out, key+"="+value)
}
