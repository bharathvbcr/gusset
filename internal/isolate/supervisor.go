package isolate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/gusset"
)

// supervisorIDCeiling is the exclusive end reserve_id uses (1<<63). An id at
// the ceiling is refused and the counter does not advance, so a later success
// cannot wrap onto a live id.
const supervisorIDCeiling uint64 = 1 << 63

// Policy is how many generations Supervise may start after failures, and the
// window those failures are counted in. MaxRestarts is a count of restarts,
// not of processes: the first successful process is not one of them.
// MaxRestarts 0 runs one process and does not replace it.
//
// StartTimeout and StallTimeout are liveness bounds. Zero means the default
// below; a negative value is refused.
//
// StartTimeout bounds one handshake, from the factory's command starting to
// the worker's hello. A worker that has not announced itself by then is killed
// and reaped, and the failed attempt spends one restart like any other failed
// hello. Default 30 seconds (defaultStartTimeout).
//
// StallTimeout bounds a worker that is alive and silent. A generation is
// retired as "unresponsive", spending one restart, when every pool permit is
// held by a ticket whose caller already gave up (Wait's context ended and the
// cancel frame was sent) and no frame has been delivered for StallTimeout
// since the last of those permits was stranded. A worker answers a cancel,
// so a generation that is still silent after the cancel is wedged: hung in
// the engine, or stalled inside a frame. A frame arriving inside the window
// restarts the judgement, so a slow job that answers is never retired, and
// neither is a worker with a free permit or one whose callers are still
// waiting inside their own deadlines. A job that ignores its cancel for longer
// than StallTimeout is indistinguishable from a wedge and is retired. Default
// 30 seconds (defaultStallTimeout). A caller that passes context.Background
// and never abandons is not bounded by this rule; Close is its bound.
type Policy struct {
	MaxRestarts  int
	Window       time.Duration
	StartTimeout time.Duration
	StallTimeout time.Duration
}

const (
	defaultStartTimeout = 30 * time.Second
	defaultStallTimeout = 30 * time.Second
)

// DefaultPolicy is 3 restarts inside one minute. A zero Policy is not this
// value. Window 0 is refused on its own, and applying the default in that
// case would hide the refusal.
var DefaultPolicy = Policy{MaxRestarts: 3, Window: time.Minute}

var (
	errZeroPolicy  = errors.New("isolate: zero Policy is refused; pass DefaultPolicy or an explicit budget")
	errNegRestarts = errors.New("isolate: MaxRestarts must not be negative")
	errZeroWindow  = errors.New("isolate: Window must be positive")
	errNegTimeout  = errors.New("isolate: StartTimeout and StallTimeout must not be negative")
	errNilFactory  = errors.New("isolate: nil factory")
	errNoCommand   = errors.New("isolate: factory returned no command")
	errTicketSpace = errors.New("isolate: ticket id space exhausted; refusing to wrap onto a live id")
)

// Factory builds one generation's command. Each call returns a new *exec.Cmd;
// a command cannot be started twice. The command's environment may already
// contain EnvProtocolFD; Start replaces that entry.
type Factory func() (*exec.Cmd, error)

// Supervisor is a Caller that retires a worker generation and starts another
// within Policy. It is not a *Proc: a Proc stays poisoned after its process
// dies, and the supervisor routes around that Proc instead of clearing it.
// Retirement does not call Proc.Close, so a result the reader already stored
// is still returned. Supervisor.Close is what closes the live generation and
// drops the retired ones.
//
// Every worker runs in a process group of its own (Setpgid), and a retirement
// kills that group, so helpers the worker spawned do not outlive it.
//
// Pdeathsig is not set. It fires when the thread that started the process
// dies, which can happen while this process is still alive, and macOS has no
// such attribute. A host SIGKILL can still leave the child running.
//
// A retired generation is not replaced until a caller needs one. Retirement
// charges one restart, which pays for that replacement; a replacement that then
// fails its hello is the restart already paid for and is not charged again.
type Supervisor struct {
	ctx     context.Context
	cancel  context.CancelFunc
	factory Factory
	policy  Policy
	// now is the restart window's clock: time.Now, except in a test that must
	// slide the window without sleeping. Set before first use, never after.
	now func() time.Time

	mu            sync.Mutex
	live          *generation
	draining      *generation
	attempt       *startAttempt
	everAttempted bool
	prepaid       bool // a retirement charged the restart its replacement will use
	restarts      []time.Time
	stop          bool
	closed        bool
	closeErr      error
	done          chan struct{}

	next    uint64
	tickets map[uint64]supTicket
	kept    map[*generation]struct{}
}

// generation is one worker process. Caller-facing tickets hold it until Wait,
// Discard, or Supervisor.Close, including after it is retired, so a stored
// result still has a proc to read.
type generation struct {
	proc           *Proc
	settled        chan struct{} // closed when retire has finished its decision and the replacement attempt
	retired        bool
	readerFinished bool
	refs           int
}

type supTicket struct {
	gen   *generation
	child uint64
}

// startAttempt is one Start shared by every caller waiting for a generation.
// The goroutine that runs it uses the supervisor lifetime context. A request
// that stops waiting does not cancel it.
type startAttempt struct {
	done    chan struct{}
	g       *generation
	err     error
	prepaid bool // the restart this attempt uses was charged when its predecessor died
}

// Supervise returns a supervisor that starts nothing until the first Call or
// Submit. ctx is the supervisor's lifetime. Cancelling it stops further
// starts and aborts a handshake already in progress; Close is what joins the
// live process. policy's zero value is refused.
func Supervise(ctx context.Context, factory Factory, policy Policy) (*Supervisor, error) {
	if ctx == nil {
		return nil, gusset.ErrNilContext
	}
	if factory == nil {
		return nil, errNilFactory
	}
	if policy == (Policy{}) {
		return nil, errZeroPolicy
	}
	if policy.MaxRestarts < 0 {
		return nil, errNegRestarts
	}
	if policy.Window <= 0 {
		return nil, errZeroWindow
	}
	if policy.StartTimeout < 0 || policy.StallTimeout < 0 {
		return nil, errNegTimeout
	}
	if policy.StartTimeout == 0 {
		policy.StartTimeout = defaultStartTimeout
	}
	if policy.StallTimeout == 0 {
		policy.StallTimeout = defaultStallTimeout
	}
	ctx, cancel := context.WithCancel(ctx)
	return &Supervisor{
		ctx:     ctx,
		cancel:  cancel,
		factory: factory,
		policy:  policy,
		now:     time.Now,
		next:    1,
		tickets: make(map[uint64]supTicket),
		kept:    make(map[*generation]struct{}),
		done:    make(chan struct{}),
	}, nil
}

// ErrBudgetExhausted is what a supervisor with no restarts left returns. It
// also matches gusset.ErrPoisoned, as it always has, so a caller that only
// tests for poison still sees it. The reverse does not hold: a poisoned worker
// is not a spent budget, so a caller that must tell them apart tests
// ErrBudgetExhausted first.
var ErrBudgetExhausted = errors.New("isolate: restart budget exhausted")

// budgetError is the budget refusal: a *gusset.Error carrying ErrPoisoned's
// code and the long-standing message, plus ErrBudgetExhausted.
type budgetError struct{ poisoned *gusset.Error }

func (e *budgetError) Error() string   { return e.poisoned.Error() }
func (e *budgetError) Unwrap() []error { return []error{ErrBudgetExhausted, e.poisoned} }

func budgetExhausted(p Policy) error {
	return &budgetError{poisoned: &gusset.Error{
		Code: gusset.ErrPoisoned.Code,
		Msg:  fmt.Sprintf("isolate: restart budget exhausted (%d restarts in %s)", p.MaxRestarts, p.Window),
	}}
}

// supervisedRefusal removes a worker refusal's advice to "Close it". That is
// advice for a caller holding a Proc; the supervisor owns Close and replaces a
// retired generation itself, so the caller is told only what happened. The
// error still matches whatever it matched.
func supervisedRefusal(err error) error {
	if err == nil || !errors.Is(err, gusset.ErrPoisoned) {
		return err
	}
	msg, advice, found := strings.Cut(err.Error(), "; Close it and")
	if !found || advice == "" {
		return err
	}
	return &remoteError{msg: msg + "; the supervisor replaces a retired worker on the next call", base: err}
}

// Call submits in and waits. The generation it submits to is the one ensure
// returned; a retirement during the call is not a second Start, and the
// payload is not written again.
func (s *Supervisor) Call(ctx context.Context, in []byte) ([]byte, error) {
	if s == nil {
		return nil, gusset.ErrNilHandle
	}
	id, err := s.Submit(ctx, in)
	if err != nil {
		return nil, err
	}
	return s.Wait(ctx, id)
}

// Submit sends one job. It starts a worker only to obtain a generation: a
// factory error or a failed hello spends one restart and is returned, and that
// is not retried. When the generation it bound turns out to be dying and refuses the
// job before anything was written, Submit asks for a generation once more and
// sends to that; it never retries a job a worker may have received, and the
// retry honours ctx. A refusal it still returns does not tell the caller to
// Close. Ticket ids are the supervisor's. The child id stays inside this
// process and is what Proc.Wait, Proc.Discard, and the cancel frame use.
func (s *Supervisor) Submit(ctx context.Context, in any) (uint64, error) {
	if s == nil {
		return 0, gusset.ErrNilHandle
	}
	if ctx == nil {
		return 0, gusset.ErrNilContext
	}
	if err := validateInput(in); err != nil {
		return 0, err
	}
	g, err := s.ensure(ctx)
	if err != nil {
		return 0, err
	}
	return s.submitVia(ctx, in, g)
}

// submitVia sends in to g and registers the ticket. A generation that refuses
// the job unsent because it is dying is replaced by one more ensure and one
// more attempt; there is no second retry.
func (s *Supervisor) submitVia(ctx context.Context, in any, g *generation) (uint64, error) {
	payload, _ := in.([]byte)
	child, unsent, err := g.proc.submitCounted(ctx, payload)
	if err != nil && unsent && errors.Is(err, gusset.ErrPoisoned) && ctx.Err() == nil {
		var gerr error
		if g, gerr = s.ensure(ctx); gerr != nil {
			return 0, supervisedRefusal(gerr)
		}
		child, _, err = g.proc.submitCounted(ctx, payload)
	}
	if err != nil {
		return 0, supervisedRefusal(err)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = g.proc.Discard(child)
		return 0, gusset.ErrClosed
	}
	id, err := s.mintLocked()
	if err != nil {
		s.mu.Unlock()
		_ = g.proc.Discard(child)
		return 0, err
	}
	s.tickets[id] = supTicket{gen: g, child: child}
	g.refs++
	s.mu.Unlock()
	return id, nil
}

func validateInput(in any) error {
	switch v := in.(type) {
	case []byte:
		if len(v) > maxInlineInput {
			return gusset.ErrInputTooLarge
		}
		return nil
	case nil:
		return nil
	case *gusset.Buffer:
		if v == nil {
			return gusset.ErrNilBuffer
		}
		return ErrBufferUnsupported
	default:
		return errors.New("gusset: input must be []byte or *Buffer")
	}
}

// Wait waits for the supervisor ticket. A result stored on a retired
// generation is still returned. A ticket with no stored result and no
// in-flight response fails with that generation's error, without the advice to
// Close (supervisedRefusal): the next call is served by a replacement. The
// cancel frame, when the deadline wins, carries the child id.
func (s *Supervisor) Wait(ctx context.Context, ticket uint64) ([]byte, error) {
	if s == nil {
		return nil, gusset.ErrNilHandle
	}
	if ctx == nil {
		return nil, gusset.ErrNilContext
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, gusset.ErrClosed
	}
	ref, ok := s.tickets[ticket]
	s.mu.Unlock()
	if !ok {
		return nil, gusset.ErrUnknownTicket
	}
	out, err := ref.gen.proc.Wait(ctx, ref.child)
	if errors.Is(err, gusset.ErrTicketBusy) {
		return out, err
	}
	s.forget(ticket)
	return out, supervisedRefusal(err)
}

// Discard gives up the supervisor ticket and the child ticket under it.
func (s *Supervisor) Discard(ticket uint64) error {
	if s == nil {
		return gusset.ErrNilHandle
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return gusset.ErrClosed
	}
	ref, ok := s.tickets[ticket]
	s.mu.Unlock()
	if !ok {
		return gusset.ErrUnknownTicket
	}
	err := ref.gen.proc.Discard(ref.child)
	if errors.Is(err, gusset.ErrTicketBusy) {
		return err
	}
	s.forget(ticket)
	return err
}

// Close latches "do not restart" before it observes whether the worker has
// died, then closes the live generation and every retired one still held.
// A Start that is already running is cancelled with the lifetime context and
// its process is not left behind. A second Close waits for the first.
func (s *Supervisor) Close() error {
	if s == nil {
		return gusset.ErrNilHandle
	}
	s.mu.Lock()
	if s.closed {
		done := s.done
		s.mu.Unlock()
		<-done
		return s.closeErr
	}
	// The latch is first. A retire that has not yet decided cannot start a
	// replacement after this, including one racing a SIGKILL.
	s.closed = true
	s.stop = true
	att := s.attempt
	s.mu.Unlock()

	s.cancel()
	if att != nil {
		<-att.done
	}

	s.mu.Lock()
	gens := s.snapshotLocked()
	s.live = nil
	s.draining = nil
	s.kept = nil
	s.tickets = nil
	s.mu.Unlock()

	var err error
	seen := map[*Proc]struct{}{}
	for _, g := range gens {
		if g == nil || g.proc == nil {
			continue
		}
		if _, ok := seen[g.proc]; ok {
			continue
		}
		seen[g.proc] = struct{}{}
		if cerr := g.proc.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	s.mu.Lock()
	s.closeErr = err
	close(s.done)
	s.mu.Unlock()
	return err
}

func (s *Supervisor) snapshotLocked() []*generation {
	seen := map[*generation]struct{}{}
	out := make([]*generation, 0, len(s.kept)+2)
	add := func(g *generation) {
		if g == nil {
			return
		}
		if _, ok := seen[g]; ok {
			return
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}
	add(s.live)
	add(s.draining)
	for g := range s.kept {
		add(g)
	}
	return out
}

// ensure returns a generation that can take a submission, waiting for a
// Start already in progress or performing one. The wait uses ctx. The Start
// uses the supervisor lifetime, on its own goroutine, so a cancelled request
// does not abandon a handshake the next caller can use.
func (s *Supervisor) ensure(ctx context.Context) (*generation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for {
		s.mu.Lock()
		if s.closed || s.stop {
			s.mu.Unlock()
			return nil, gusset.ErrClosed
		}
		if err := s.ctx.Err(); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if g := s.live; g != nil && !g.retired && g.proc.usable() {
			s.mu.Unlock()
			return g, nil
		}
		if att := s.attempt; att != nil {
			s.mu.Unlock()
			return s.waitAttempt(ctx, att)
		}
		// A generation that is dying still owns the process. Wait until its
		// retire has reserved the single replacement Start (or refused one)
		// rather than starting a second child beside it.
		if g := s.live; g != nil && !g.retired {
			settled := g.settled
			s.mu.Unlock()
			select {
			case <-settled:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		// A retired generation's process may still be running down (its group
		// is being killed and its reader drained). The replacement starts only
		// after that, so two workers never run side by side.
		if d := s.draining; d != nil {
			settled := d.settled
			s.mu.Unlock()
			select {
			case <-settled:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		now := s.now()
		if s.everAttempted && !s.prepaid && s.inWindowLocked(now) >= s.policy.MaxRestarts {
			err := budgetExhausted(s.policy)
			s.mu.Unlock()
			return nil, err
		}
		att := s.beginAttemptLocked()
		s.mu.Unlock()
		go s.finishAttempt(att)
		return s.waitAttempt(ctx, att)
	}
}

func (s *Supervisor) waitAttempt(ctx context.Context, att *startAttempt) (*generation, error) {
	select {
	case <-att.done:
		if att.err != nil {
			return nil, att.err
		}
		if att.g == nil {
			return nil, errors.New("isolate: start produced no worker")
		}
		return att.g, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Supervisor) beginAttemptLocked() *startAttempt {
	att := &startAttempt{done: make(chan struct{}), prepaid: s.prepaid}
	s.prepaid = false
	s.attempt = att
	s.everAttempted = true
	return att
}

// finishAttempt is the one Start for this attempt. It does not set Pdeathsig
// and it does not call cmd.Wait; start's handshake failure reaps a process
// that never became a Proc, and waitProc is the only Wait after a successful
// start.
func (s *Supervisor) finishAttempt(att *startAttempt) {
	if s.blocked() {
		s.finishStart(att, nil, s.currentStopErr(), false)
		return
	}
	cmd, err := s.factory()
	if err == nil && cmd == nil {
		err = errNoCommand
	}
	if err != nil {
		s.finishStart(att, nil, err, true)
		return
	}
	if s.blocked() {
		s.finishStart(att, nil, s.currentStopErr(), false)
		return
	}
	notice := make(chan struct{}, 1)
	// The timeout wraps start only. The lifetime context stays the parent, so
	// Close still aborts a handshake, and a timeout is a failed hello that
	// spends a restart where a cancelled lifetime does not.
	sctx, cancel := context.WithTimeout(s.ctx, s.policy.StartTimeout)
	p, err := start(sctx, cmd, notice, s.policy.StallTimeout)
	cancel()
	if err != nil {
		// A lifetime cancellation is Close (or the caller's ctx ending the
		// supervisor). That is not a spent restart. A handshake or factory
		// failure is.
		s.finishStart(att, nil, err, true)
		return
	}
	g := &generation{proc: p, settled: make(chan struct{})}
	s.mu.Lock()
	if s.stop || s.ctx.Err() != nil {
		if s.attempt == att {
			s.attempt = nil
		}
		s.mu.Unlock()
		_ = p.Close()
		s.finishStart(att, nil, s.currentStopErr(), false)
		return
	}
	s.live = g
	s.kept[g] = struct{}{}
	if s.attempt == att {
		s.attempt = nil
	}
	att.g = g
	s.mu.Unlock()
	close(att.done)
	go s.watch(g)
}

func (s *Supervisor) finishStart(att *startAttempt, g *generation, err error, spend bool) {
	s.mu.Lock()
	if s.attempt == att {
		s.attempt = nil
	}
	if spend && !att.prepaid && err != nil && !s.stop && s.ctx.Err() == nil {
		s.noteRestartLocked(s.now())
	}
	if g != nil {
		att.g = g
	}
	att.err = err
	s.mu.Unlock()
	close(att.done)
}

func (s *Supervisor) blocked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stop || s.ctx.Err() != nil
}

func (s *Supervisor) currentStopErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stoppedErrorLocked()
}

func (s *Supervisor) stoppedErrorLocked() error {
	if s.closed || s.stop {
		return gusset.ErrClosed
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	return gusset.ErrClosed
}

func (s *Supervisor) watch(g *generation) {
	s.retire(g, awaitRetire(g.proc))
}

func awaitRetire(p *Proc) string {
	select {
	case <-p.retire:
		return p.retireReason()
	case <-p.procExited:
		// The exit and the poison signal can both be ready. The poison is
		// the retirement; the exit that follows it is not a second one.
		select {
		case <-p.retire:
			return p.retireReason()
		default:
			return exitCause(p)
		}
	}
}

func exitCause(p *Proc) string {
	p.mu.Lock()
	err := p.waitErr
	p.mu.Unlock()
	if err == nil {
		return "child exited"
	}
	// ExitCode -1 (a signal) arrives as this error. It retires the generation
	// the same way a numeric status does.
	return "child exited: " + err.Error()
}

// retire marks the generation, counts one restart, and kills its process group
// if anything is left running. It does not start a replacement: the next
// caller that needs one does (ensure), so a worker that dies right after hello
// cannot be respawned back to back with nobody asking. The restart counted here
// pays for that replacement (prepaid) when the budget allows one. The kill runs
// here, on the watch goroutine, not inside writeLocked. Proc.Close is not
// called: the read path keeps stored results while closed is false. After the
// reader finishes, stdin is closed if poisonStream has not already closed it.
func (s *Supervisor) retire(g *generation, cause string) {
	s.mu.Lock()
	if g.retired {
		s.mu.Unlock()
		return
	}
	// Close latched "do not restart" before it observed this death. The
	// process exit is part of that Close, not a spent restart and not a warn.
	if s.stop {
		g.retired = true
		if s.live == g {
			s.live = nil
		}
		s.mu.Unlock()
		close(g.settled)
		return
	}
	g.retired = true
	if s.live == g {
		s.live = nil
	}
	now := s.now()
	projected := s.inWindowLocked(now) + 1
	s.noteRestartLocked(now)
	left := s.policy.MaxRestarts - projected
	if left < 0 {
		left = 0
	}
	pid := 0
	if g.proc.cmd != nil && g.proc.cmd.Process != nil {
		pid = g.proc.cmd.Process.Pid
	}
	s.prepaid = projected <= s.policy.MaxRestarts
	s.draining = g
	s.mu.Unlock()

	slog.Warn("isolate: worker generation retired",
		"cause", cause,
		"pid", pid,
		"restarts_left", left,
	)
	defer close(g.settled)

	// The group, not only the leader: the leader may already be gone while a
	// helper it spawned still runs.
	g.proc.killTree()
	<-g.proc.readerDone
	g.proc.closeStdin()

	s.mu.Lock()
	g.readerFinished = true
	if s.draining == g {
		s.draining = nil
	}
	s.maybeDropLocked(g)
	s.mu.Unlock()
}

// inWindowLocked counts restart events whose s.now values are still inside
// the policy window. Subtraction uses the monotonic clock both timestamps
// carry.
func (s *Supervisor) inWindowLocked(now time.Time) int {
	n := 0
	for _, ts := range s.restarts {
		if now.Sub(ts) < s.policy.Window {
			n++
		}
	}
	return n
}

func (s *Supervisor) noteRestartLocked(now time.Time) {
	fresh := make([]time.Time, 0, len(s.restarts)+1)
	for _, ts := range s.restarts {
		if now.Sub(ts) < s.policy.Window {
			fresh = append(fresh, ts)
		}
	}
	s.restarts = append(fresh, now)
}

func (s *Supervisor) mintLocked() (uint64, error) {
	if s.next == 0 || s.next >= supervisorIDCeiling {
		return 0, errTicketSpace
	}
	id := s.next
	s.next++
	return id, nil
}

func (s *Supervisor) forget(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.tickets[id]
	if !ok {
		return
	}
	delete(s.tickets, id)
	if ref.gen != nil && ref.gen.refs > 0 {
		ref.gen.refs--
	}
	if ref.gen != nil {
		s.maybeDropLocked(ref.gen)
	}
}

// maybeDropLocked drops a retired generation only once its reader has exited
// and nothing stored or in flight still names it. Supervisor.Close is the
// other way those refs are released.
func (s *Supervisor) maybeDropLocked(g *generation) {
	if g == nil || !g.retired || !g.readerFinished || g.refs > 0 {
		return
	}
	delete(s.kept, g)
}
