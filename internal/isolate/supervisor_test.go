package isolate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// supHarness starts workers from this test binary. modeOf is called with the
// 1-based generation number and may be nil.
type supHarness struct {
	t      *testing.T
	s      *Supervisor
	starts atomic.Int32
	count  string
	modeOf func(n int) string

	mu     sync.Mutex
	cmds   []*exec.Cmd
	stderr []*lockedBuffer
}

func newHarness(t *testing.T, policy Policy, modeOf func(n int) string) *supHarness {
	t.Helper()
	h := &supHarness{t: t, modeOf: modeOf}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Supervise(ctx, h.factory, policy)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	h.s = s
	t.Cleanup(func() {
		if t.Failed() {
			h.mu.Lock()
			for i, b := range h.stderr {
				if text := b.String(); text != "" {
					t.Logf("worker %d stderr:\n%s", i+1, text)
				}
			}
			h.mu.Unlock()
		}
		_ = s.Close()
		cancel()
	})
	return h
}

func (h *supHarness) factory() (*exec.Cmd, error) {
	n := int(h.starts.Add(1))
	mode := ""
	if h.modeOf != nil {
		mode = h.modeOf(n)
	}
	cmd := exec.Command(os.Args[0])
	env := append(os.Environ(), workerEnv+"=1", modeEnv+"="+mode)
	if h.count != "" {
		env = append(env, countFileEnv+"="+h.count)
	}
	cmd.Env = env
	buf := &lockedBuffer{}
	cmd.Stderr = buf
	h.mu.Lock()
	h.cmds = append(h.cmds, cmd)
	h.stderr = append(h.stderr, buf)
	h.mu.Unlock()
	return cmd, nil
}

func (h *supHarness) echo() int {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := h.s.Call(ctx, []byte{0, 0x42})
	if err != nil || !bytes.Equal(out, []byte{0, 0x42}) {
		h.t.Fatalf("echo = %v, %v", out, err)
	}
	pid := pidOf(h.s)
	if pid == 0 {
		h.t.Fatal("echo returned with no live pid")
	}
	return pid
}

// killAndEcho kills pid and returns the replacement's pid. One death starts
// one process: a second Start in that window fails the test.
func (h *supHarness) killAndEcho(old int) int {
	h.t.Helper()
	before := h.starts.Load()
	if err := syscall.Kill(old, syscall.SIGKILL); err != nil {
		h.t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		out, err := h.s.Call(ctx, []byte{0, 0x42})
		cancel()
		if err == nil && bytes.Equal(out, []byte{0, 0x42}) {
			pid := pidOf(h.s)
			if pid != 0 && pid != old {
				if got := h.starts.Load(); got != before+1 {
					h.t.Fatalf("starts %d -> %d after killing %d; want exactly one replacement", before, got, old)
				}
				return pid
			}
		}
		last = err
		if isBudget(err) {
			h.t.Fatalf("budget while replacing pid %d: %v", old, err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	h.t.Fatalf("echo after killing %d: %v (starts %d -> %d)", old, last, before, h.starts.Load())
	return 0
}

func (h *supHarness) pids() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]int, 0, len(h.cmds))
	for _, c := range h.cmds {
		if c.Process != nil {
			out = append(out, c.Process.Pid)
		}
	}
	return out
}

func pidOf(s *Supervisor) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live == nil || s.live.proc == nil || s.live.proc.cmd == nil || s.live.proc.cmd.Process == nil {
		return 0
	}
	return s.live.proc.cmd.Process.Pid
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func isBudget(err error) bool {
	return err != nil && errors.Is(err, gusset.ErrPoisoned) && strings.Contains(err.Error(), "restart budget exhausted")
}

func countPayload(t *testing.T, path string, payload []byte) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	want := ""
	if payload != nil {
		want = strings.TrimSpace(strings.ToLower(hexBytes(payload)))
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if line == want {
			n++
		}
	}
	return n
}

func hexBytes(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = digits[c>>4]
		out[i*2+1] = digits[c&0x0f]
	}
	return string(out)
}

func waitCount(t *testing.T, path string, payload []byte, want int) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		n := countPayload(t, path, payload)
		if n >= want {
			return n
		}
		if time.Now().After(deadline) {
			t.Fatalf("payload recorded %d times; want at least %d", n, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSupervisor_NotInParityModes(t *testing.T) {
	if len(modes) != 2 || modes[0].name != "inproc" || modes[1].name != "proc" {
		names := make([]string, len(modes))
		for i, m := range modes {
			names[i] = m.name
		}
		t.Fatalf("parity modes = %v; want [inproc proc]", names)
	}
}

func TestSupervise_RefusesBadPolicy(t *testing.T) {
	f := func() (*exec.Cmd, error) { return nil, errors.New("unused") }
	ctx := context.Background()
	_, err := Supervise(ctx, f, Policy{})
	if !errors.Is(err, errZeroPolicy) {
		t.Fatalf("zero Policy = %v; want %v", err, errZeroPolicy)
	}
	_, err = Supervise(ctx, f, Policy{MaxRestarts: -1, Window: time.Second})
	if !errors.Is(err, errNegRestarts) {
		t.Fatalf("negative MaxRestarts = %v; want %v", err, errNegRestarts)
	}
	_, err = Supervise(ctx, f, Policy{MaxRestarts: 1, Window: 0})
	if !errors.Is(err, errZeroWindow) {
		t.Fatalf("zero Window = %v; want %v", err, errZeroWindow)
	}
	_, err = Supervise(ctx, f, Policy{MaxRestarts: 1, Window: -time.Second})
	if !errors.Is(err, errZeroWindow) {
		t.Fatalf("negative Window = %v; want %v", err, errZeroWindow)
	}
	if DefaultPolicy.MaxRestarts != 3 || DefaultPolicy.Window != time.Minute {
		t.Fatalf("DefaultPolicy = %+v; want 3 restarts in one minute", DefaultPolicy)
	}
}

func TestStart_ReplacesStaleProtocolFD(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		workerEnv+"=1",
		modeEnv+"=",
		EnvProtocolFD+"=9",
		"GUSSET_ISOLATE_UNRELATED=1",
		EnvProtocolFD+"=8",
	)
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := Start(ctx, cmd)
	if err != nil {
		t.Fatalf("Start = %v\nstderr:\n%s", err, stderr.String())
	}
	t.Cleanup(func() { _ = p.Close() })

	n := 0
	got := ""
	for _, e := range cmd.Env {
		k, v, ok := strings.Cut(e, "=")
		if !ok || k != EnvProtocolFD {
			continue
		}
		n++
		got = v
	}
	if n != 1 || got == "9" || got == "8" || got == "" {
		t.Fatalf("EnvProtocolFD entries = %d, value %q; want one replaced descriptor", n, got)
	}
	out, err := p.Call(ctx, []byte{0, 0x42})
	if err != nil || !bytes.Equal(out, []byte{0, 0x42}) {
		t.Fatalf("Call = %v, %v; stderr:\n%s", out, err, stderr.String())
	}
}

func TestSupervisor_HelloFailureSpendsOneRestart(t *testing.T) {
	var n atomic.Int32
	factory := func() (*exec.Cmd, error) {
		n.Add(1)
		return exec.Command("true"), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Supervise(ctx, factory, Policy{MaxRestarts: 1, Window: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer callCancel()
	if _, err := s.Call(callCtx, []byte{0}); err == nil {
		t.Fatal("Call = nil; a hello that never arrives must fail")
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("factory calls during the failing Call = %d; the call started more than once", got)
	}
	_, err = s.Call(callCtx, []byte{0})
	if !isBudget(err) {
		t.Fatalf("second Call = %v; the failed hello must spend the one restart", err)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("factory calls = %d; a spent restart started another process", got)
	}
}

func TestSupervisor_SigkillDoesNotReplay(t *testing.T) {
	h := newHarness(t, Policy{MaxRestarts: 3, Window: time.Minute}, nil)
	h.count = filepath.Join(t.TempDir(), "submits")
	pid := h.echo()
	if h.starts.Load() != 1 {
		t.Fatalf("starts = %d after warmup; want 1", h.starts.Load())
	}
	// 2.55s asleep in the engine, counted when the worker accepts the job.
	payload := []byte{9, 255, 0, 0xAB}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := h.s.Submit(ctx, payload)
	if err != nil {
		t.Fatal(err)
	}
	before := waitCount(t, h.count, payload, 1)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	out, err := h.s.Wait(wctx, id)
	if err == nil || bytes.Equal(out, payload) {
		t.Fatalf("in-flight Wait = %v, %v; SIGKILL must fail the call", out, err)
	}
	if !errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("in-flight Wait = %v; want ErrPoisoned", err)
	}
	next := h.echo()
	if next == pid {
		t.Fatalf("echo still on pid %d", pid)
	}
	if got := countPayload(t, h.count, payload); got != before {
		t.Fatalf("payload recorded %d times after the replacement; was %d", got, before)
	}
	if h.starts.Load() != 2 {
		t.Fatalf("starts = %d; the death should start one replacement", h.starts.Load())
	}
}

func TestSupervisor_PanicSpendsOneRestart(t *testing.T) {
	h := newHarness(t, Policy{MaxRestarts: 3, Window: time.Minute}, nil)
	pid := h.echo()
	// Both jobs sit in the engine long enough for both submits to land on
	// this process, then panic. Two observations, one retirement.
	payload := []byte{9, 80, 1}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id1, err := h.s.Submit(ctx, payload)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := h.s.Submit(ctx, payload)
	if err != nil {
		t.Fatal(err)
	}
	_, err1 := h.s.Wait(ctx, id1)
	_, err2 := h.s.Wait(ctx, id2)
	sawPanic := false
	for _, err := range []error{err1, err2} {
		switch {
		case errors.Is(err, gusset.ErrPanic):
			sawPanic = true
			if !strings.Contains(err.Error(), "plain panic") {
				t.Fatalf("panic text = %v", err)
			}
		case errors.Is(err, gusset.ErrPoisoned):
		default:
			t.Fatalf("in-flight error = %v; want ErrPanic or ErrPoisoned", err)
		}
	}
	if !sawPanic {
		t.Fatalf("errors %v and %v; want a diagnostic panic", err1, err2)
	}
	next := h.echo()
	if next == pid {
		t.Fatal("echo reused the panicked process")
	}
	if got := h.starts.Load(); got != 2 {
		t.Fatalf("starts = %d; two poison observations spent more than one restart", got)
	}
}

func TestSupervisor_CutWriteRetiresOneRestart(t *testing.T) {
	h := newHarness(t, Policy{MaxRestarts: 1, Window: time.Minute}, func(n int) string {
		if n == 1 {
			return "deaf"
		}
		return ""
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.s.Submit(ctx, []byte{0}); err != nil {
		t.Fatal(err)
	}
	h.s.mu.Lock()
	p := h.s.live.proc
	h.s.mu.Unlock()
	fillStdin(t, p)

	wctx, wcancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer wcancel()
	var err error
	within(t, time.Second, "cut Submit", func() {
		_, err = h.s.Submit(wctx, []byte{0x5A})
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cut Submit = %v; want the request's deadline", err)
	}
	pid := h.echo()
	if h.starts.Load() != 2 {
		t.Fatalf("starts = %d after the cut write; the generation was not replaced once", h.starts.Load())
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h.waitBudget(h.starts.Load())
	if got := h.starts.Load(); got != 2 {
		t.Fatalf("starts = %d; the cut write did not consume the one restart", got)
	}
}

func (h *supHarness) waitBudget(startsAt int32) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := h.s.Call(ctx, []byte{0, 1})
		cancel()
		last = err
		if isBudget(err) {
			return
		}
		if err == nil && h.starts.Load() != startsAt {
			h.t.Fatalf("call succeeded on a new process (starts %d -> %d) after the budget was spent", startsAt, h.starts.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("budget error did not arrive; last %v; starts %d", last, h.starts.Load())
}

func TestSupervisor_BudgetStopsAtMaxRestarts(t *testing.T) {
	h := newHarness(t, Policy{MaxRestarts: 2, Window: time.Minute}, nil)
	pid := h.echo()
	pid = h.killAndEcho(pid)
	pid = h.killAndEcho(pid)
	if h.starts.Load() != 3 {
		t.Fatalf("starts = %d; want the initial process and two restarts", h.starts.Load())
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h.waitBudget(3)
	if got := h.starts.Load(); got != 3 {
		t.Fatalf("starts = %d; the third death left another process", got)
	}
	h.waitDead()
}

func TestSupervisor_DefaultPolicyFourthDeathFails(t *testing.T) {
	if DefaultPolicy.MaxRestarts != 3 || DefaultPolicy.Window != time.Minute {
		t.Fatalf("DefaultPolicy = %+v", DefaultPolicy)
	}
	h := newHarness(t, DefaultPolicy, nil)
	pid := h.echo()
	for range 3 {
		pid = h.killAndEcho(pid)
	}
	if h.starts.Load() != 4 {
		t.Fatalf("starts = %d; want four processes under DefaultPolicy", h.starts.Load())
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := h.s.Call(ctx, []byte{0, 1})
		cancel()
		last = err
		if isBudget(err) {
			const want = "isolate: restart budget exhausted (3 restarts in 1m0s)"
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("budget error = %v; want it to contain %q", err, want)
			}
			if h.starts.Load() != 4 {
				t.Fatalf("starts = %d after the fourth death", h.starts.Load())
			}
			h.waitDead()
			return
		}
		if err == nil && h.starts.Load() != 4 {
			t.Fatalf("fifth process started (starts %d)", h.starts.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fourth death did not exhaust DefaultPolicy; last %v", last)
}

func (h *supHarness) waitDead() {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		alive := false
		var stuck int
		for _, pid := range h.pids() {
			if pidAlive(pid) {
				alive = true
				stuck = pid
			}
		}
		if !alive {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("pid %d still alive", stuck)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSupervisor_Mode9DoesNotRestart(t *testing.T) {
	h := newHarness(t, DefaultPolicy, nil)
	pid := h.echo()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := h.s.Call(ctx, []byte{9, 200}) // 2s, never polls the deadline
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call = %v; want DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("caller returned after %v; the engine runs 2s", elapsed)
	}
	if got := pidOf(h.s); got != pid {
		t.Fatalf("pid %d -> %d; a mode-9 sleep restarted the worker", pid, got)
	}
	if h.starts.Load() != 1 {
		t.Fatalf("starts = %d; mode 9 retired the generation", h.starts.Load())
	}
	if next := h.echo(); next != pid {
		t.Fatalf("echo pid %d; want the same process %d", next, pid)
	}
}

func TestSupervisor_StoredResultDoesNotAliasChildID(t *testing.T) {
	h := newHarness(t, DefaultPolicy, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first := []byte{0, 0x11}
	id1, err := h.s.Submit(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	h.s.mu.Lock()
	ref1 := h.s.tickets[id1]
	h.s.mu.Unlock()
	waitStored(t, ref1.gen.proc, ref1.child)
	if ref1.child != 1 {
		t.Fatalf("first child id = %d; want 1", ref1.child)
	}
	old := pidOf(h.s)
	if err := syscall.Kill(old, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// The next submission has to be the first ticket of the replacement.
	// A submit that lands before the death is visible is another job on the
	// process that is going away, and that process's counter has already
	// issued child id 1.
	waitPID := time.Now().Add(5 * time.Second)
	for {
		pid := pidOf(h.s)
		if pid != 0 && pid != old {
			break
		}
		if time.Now().After(waitPID) {
			t.Fatal("replacement did not become live")
		}
		time.Sleep(5 * time.Millisecond)
	}
	second := []byte{0, 0x22}
	id2, err := h.s.Submit(ctx, second)
	if err != nil {
		t.Fatalf("Submit on the replacement = %v", err)
	}
	h.s.mu.Lock()
	ref2 := h.s.tickets[id2]
	h.s.mu.Unlock()
	if id1 == id2 {
		t.Fatalf("supervisor issued %d for both generations", id1)
	}
	if ref2.child != 1 {
		t.Fatalf("replacement child id = %d; want 1", ref2.child)
	}
	out1, err := h.s.Wait(ctx, id1)
	if err != nil || !bytes.Equal(out1, first) {
		t.Fatalf("Wait stored = %v, %v; want %v", out1, err, first)
	}
	out2, err := h.s.Wait(ctx, id2)
	if err != nil || !bytes.Equal(out2, second) {
		t.Fatalf("Wait replacement = %v, %v; want %v", out2, err, second)
	}
}

func TestSupervisor_CloseRacingKillDoesNotRestart(t *testing.T) {
	before := fdIdentities(t)
	h := newHarness(t, Policy{MaxRestarts: 5, Window: time.Minute}, nil)
	pid := h.echo()
	errc := make(chan error, 1)
	go func() { errc <- h.s.Close() }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.s.mu.Lock()
		latched := h.s.stop
		h.s.mu.Unlock()
		if latched {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not latch do-not-restart")
		}
		time.Sleep(time.Millisecond)
	}
	// Close may already have reaped the worker by closing its stdin. The
	// signal still has to be a death the latched supervisor does not replace.
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err != nil && !strings.Contains(err.Error(), "killed") {
			t.Fatalf("Close = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	if got := h.starts.Load(); got != 1 {
		t.Fatalf("starts = %d; a death observed after Close latched still restarted", got)
	}
	h.waitDead()
	settle := time.Now().Add(2 * time.Second)
	var extra []string
	for {
		extra = extraFDs(before, fdIdentities(t))
		if len(extra) == 0 {
			return
		}
		if time.Now().After(settle) {
			t.Fatalf("descriptors still open after Close: %d new", len(extra))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func fdIdentities(t *testing.T) map[string]struct{} {
	t.Helper()
	limit := 4096
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err == nil && rl.Cur > 0 && int(rl.Cur) < limit {
		limit = int(rl.Cur)
	}
	out := make(map[string]struct{})
	for fd := 0; fd < limit; fd++ {
		id, err := fdIdentity(fd)
		if err != nil {
			continue
		}
		out[id] = struct{}{}
	}
	return out
}

func extraFDs(before, after map[string]struct{}) []string {
	var extra []string
	for id := range after {
		if _, ok := before[id]; !ok {
			extra = append(extra, id)
		}
	}
	return extra
}

func TestSupervisor_CancelDuringHelloKeepsTheProcess(t *testing.T) {
	h := newHarness(t, DefaultPolicy, func(int) string { return "delay-hello" })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := h.s.Call(ctx, []byte{0, 1})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call during hello = %v; want the request deadline", err)
	}
	if h.starts.Load() != 1 {
		t.Fatalf("starts = %d; cancelling the waiter started another handshake", h.starts.Load())
	}
	pid := h.echo()
	if pid == 0 {
		t.Fatal("the continued hello did not yield a process")
	}
	if h.starts.Load() != 1 {
		t.Fatalf("starts = %d; the next caller did not reuse the handshake", h.starts.Load())
	}
}

func TestSupervisor_GrandchildDrainBeforeNextHello(t *testing.T) {
	var gen1 atomic.Pointer[Proc]
	var early atomic.Bool
	var starts atomic.Int32
	factory := func() (*exec.Cmd, error) {
		n := starts.Add(1)
		mode := ""
		if n == 1 {
			mode = "grandchild"
		} else if p := gen1.Load(); p != nil {
			select {
			case <-p.readerDone:
			default:
				early.Store(true)
			}
		}
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), workerEnv+"=1", modeEnv+"="+mode)
		cmd.Stderr = &lockedBuffer{}
		return cmd, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Supervise(ctx, factory, DefaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	if _, err := s.Submit(sctx, []byte{0, 1}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	gen1.Store(s.live.proc)
	pid := s.live.proc.cmd.Process.Pid
	s.mu.Unlock()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// A call that already bound the dying generation fails with that
	// generation and is not replayed. The following call must reach hello
	// on the replacement without waiting out the grandchild's sleep.
	echoDeadline := time.Now().Add(3 * time.Second)
	var last error
	for {
		cctx, ccancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		out, err := s.Call(cctx, []byte{0, 0x42})
		ccancel()
		if err == nil && bytes.Equal(out, []byte{0, 0x42}) {
			last = nil
			break
		}
		last = err
		if time.Now().After(echoDeadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if early.Load() {
		t.Fatal("replacement factory ran before the previous reader finished")
	}
	if last != nil {
		t.Fatalf("echo after the grandchild drain = %v", last)
	}
	if starts.Load() != 2 {
		t.Fatalf("starts = %d; want the drained generation and one replacement", starts.Load())
	}
}

func TestSupervisor_WindowSlides(t *testing.T) {
	const window = 200 * time.Millisecond
	h := newHarness(t, Policy{MaxRestarts: 1, Window: window}, nil)
	pid := h.echo()
	pid = h.killAndEcho(pid)
	if h.starts.Load() != 2 {
		t.Fatalf("starts = %d; want the initial process and one restart", h.starts.Load())
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h.waitBudget(2)
	// Both deaths are inside the window. After it slides, time.Now subtraction
	// drops them and one more process is allowed.
	time.Sleep(window + 200*time.Millisecond)
	if next := h.echo(); next == 0 || next == pid {
		t.Fatalf("echo after the window slid did not start a new process (pid %d)", next)
	}
	if h.starts.Load() != 3 {
		t.Fatalf("starts = %d; the slid window should allow exactly one more process", h.starts.Load())
	}
}

func TestSupervisor_TicketIDsStopAtTheCeiling(t *testing.T) {
	h := newHarness(t, DefaultPolicy, nil)
	h.echo()
	h.s.mu.Lock()
	h.s.next = supervisorIDCeiling
	h.s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := h.s.Submit(ctx, []byte{0, 1})
	if !errors.Is(err, errTicketSpace) {
		t.Fatalf("Submit at the ceiling = %v; want %v", err, errTicketSpace)
	}
	h.s.mu.Lock()
	next := h.s.next
	h.s.next = 2
	h.s.mu.Unlock()
	if next != supervisorIDCeiling {
		t.Fatalf("next = %d; a refused id advanced the counter", next)
	}
	if _, err := h.s.Call(ctx, []byte{0, 2}); err != nil {
		t.Fatalf("Call after restoring the counter = %v", err)
	}
}
