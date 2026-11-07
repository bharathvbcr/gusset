package isolate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	// forkSleep, when set, is the file each worker writes its `sleep 60`
	// helper's pid to (forkSleepEnv).
	forkSleep string
	modeOf    func(n int) string

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
	if h.forkSleep != "" {
		env = append(env, forkSleepEnv+"="+h.forkSleep)
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
	// issued child id 1. (A submit that races the death is retried once on the
	// replacement, which is also a first ticket.)
	waitPID := time.Now().Add(5 * time.Second)
	for {
		// The old generation is retired once it is no longer live. The
		// replacement starts when the next caller needs one, so this waits for
		// the retirement and not for a new pid.
		if pidOf(h.s) != old {
			break
		}
		if time.Now().After(waitPID) {
			t.Fatal("the killed generation was never retired")
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

// abandonPool gives up one Call per permit, each on a short deadline, so every
// permit of the live generation is held by a ticket whose caller left.
func abandonPool(t *testing.T, h *supHarness, payload []byte) {
	t.Helper()
	for i := 0; i < workerPool; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := h.s.Call(ctx, payload)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("abandoning Call %d = %v; want DeadlineExceeded", i, err)
		}
	}
}

func liveProc(h *supHarness) *Proc {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return h.s.live.proc
}

// stallRetiredAsUnresponsive is the A4-1 flow for a worker that is alive and
// silent in one of the harness's ways: every permit stranded by a caller that
// left, one more caller parked on the permit, and then a later Call(500ms)
// that must succeed on exactly one replacement.
func stallRetiredAsUnresponsive(t *testing.T, mode string) {
	h := newHarness(t, Policy{MaxRestarts: 2, Window: time.Minute, StallTimeout: 300 * time.Millisecond}, func(n int) string {
		if n == 1 {
			return mode
		}
		return ""
	})
	// The first Call starts the worker; the worker's mode never answers.
	abandonPool(t, h, []byte{0, 1})
	old := liveProc(h)
	abandoned := time.Now()

	// A caller that arrives after the permits are stranded parks on the
	// permit. Retirement must release it, not leave it for its own deadline.
	parked := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := h.s.Call(ctx, []byte{0, 2})
		parked <- err
	}()

	deadline := time.After(5 * time.Second)
	for !old.exited() {
		select {
		case <-deadline:
			t.Fatalf("worker still alive %v after its permits were stranded (stall bound 300ms); starts=%d", time.Since(abandoned), h.starts.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := old.retireReason(); got != "unresponsive" {
		t.Fatalf("retire cause = %q; want %q", got, "unresponsive")
	}
	select {
	case err := <-parked:
		// The retirement releases it with nothing written, so it is retried
		// once on the replacement (A4-5) and succeeds. What it must not do
		// is run to its own deadline or be told to Close.
		if errors.Is(err, context.DeadlineExceeded) || (err != nil && strings.Contains(err.Error(), "Close it")) {
			t.Fatalf("parked Call = %v; want the replacement's answer or a refusal that does not advise Close, not its own deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a caller parked on a stranded permit was not released by the retirement")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := h.s.Call(ctx, []byte{0, 0x42})
	if err != nil || !bytes.Equal(out, []byte{0, 0x42}) {
		t.Fatalf("Call(500ms) after the retirement = %v, %v", out, err)
	}
	if got := h.starts.Load(); got != 2 {
		t.Fatalf("starts = %d; want exactly one restart", got)
	}
}

func TestSupervisor_HangAfterHelloRetiredUnresponsive(t *testing.T) {
	stallRetiredAsUnresponsive(t, "hang")
}

func TestSupervisor_PartialFrameStallRetiredUnresponsive(t *testing.T) {
	stallRetiredAsUnresponsive(t, "stall")
}

// A worker that is slow but answers is not wedged: the frame that lands inside
// the stall window restarts the judgement, and the permits come back.
func TestSupervisor_SlowWorkerThatAnswersIsNotRetired(t *testing.T) {
	h := newHarness(t, Policy{MaxRestarts: 2, Window: time.Minute, StallTimeout: 700 * time.Millisecond}, nil)
	pid := h.echo()
	abandonPool(t, h, []byte{9, 30}) // 300ms of work, never polling the deadline
	p := liveProc(h)
	begin := time.Now()

	// Outlast the stall window, which started when the permits were stranded.
	for time.Since(begin) < 1200*time.Millisecond {
		time.Sleep(20 * time.Millisecond)
	}
	if len(p.sem) != 0 {
		t.Fatalf("%d permits still held; the slow jobs should have answered", len(p.sem))
	}
	p.mu.Lock()
	sent := p.retireSent
	p.mu.Unlock()
	if sent || p.exited() {
		t.Fatalf("a worker that answered was retired (signal %v, exited %v)", sent, p.exited())
	}
	if h.starts.Load() != 1 {
		t.Fatalf("starts = %d; the slow worker was replaced", h.starts.Load())
	}
	if next := h.echo(); next != pid {
		t.Fatalf("pid %d -> %d", pid, next)
	}
}

func TestSupervisor_SilentHandshakeIsKilledWithinStartTimeout(t *testing.T) {
	const bound = 300 * time.Millisecond
	h := newHarness(t, Policy{MaxRestarts: 1, Window: time.Minute, StartTimeout: bound}, func(int) string { return "no-hello" })

	// Two callers attach to the one attempt; neither may park past the bound.
	errs := make(chan error, 2)
	begin := time.Now()
	for i := 0; i < 2; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := h.s.Call(ctx, []byte{0, 1})
			errs <- err
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err == nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Call %d = %v; want the handshake timeout", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Call %d still parked %v after StartTimeout %v", i, time.Since(begin), bound)
		}
	}
	if got := h.starts.Load(); got != 1 {
		t.Fatalf("starts = %d; the hung handshake started more than once", got)
	}
	pids := h.pids()
	if len(pids) != 1 || pidAlive(pids[0]) {
		t.Fatalf("worker pids %v: the silent worker was not killed and reaped", pids)
	}
	// One restart spent, MaxRestarts 1: the next Call gets the budget error.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.s.Call(ctx, []byte{0, 1}); !isBudget(err) {
		t.Fatalf("next Call = %v; the timeout must spend exactly the one restart", err)
	}
	if got := h.starts.Load(); got != 1 {
		t.Fatalf("starts = %d after the budget error", got)
	}
}

func TestSupervisor_SilentHandshakeThenFreshWorker(t *testing.T) {
	h := newHarness(t, Policy{MaxRestarts: 2, Window: time.Minute, StartTimeout: 300 * time.Millisecond}, func(n int) string {
		if n == 1 {
			return "no-hello"
		}
		return ""
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.s.Call(ctx, []byte{0, 1}); err == nil {
		t.Fatal("Call on a silent handshake = nil")
	}
	h.echo()
	if got := h.starts.Load(); got != 2 {
		t.Fatalf("starts = %d; want the silent attempt and one fresh worker", got)
	}
}

func TestSupervise_LivenessBounds(t *testing.T) {
	f := func() (*exec.Cmd, error) { return exec.Command("true"), nil }
	ctx := context.Background()
	for _, p := range []Policy{
		{MaxRestarts: 1, Window: time.Second, StartTimeout: -time.Second},
		{MaxRestarts: 1, Window: time.Second, StallTimeout: -time.Second},
	} {
		if _, err := Supervise(ctx, f, p); !errors.Is(err, errNegTimeout) {
			t.Fatalf("Supervise(%+v) = %v; want %v", p, err, errNegTimeout)
		}
	}
	s, err := Supervise(ctx, f, DefaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.policy.StartTimeout != defaultStartTimeout || s.policy.StallTimeout != defaultStallTimeout {
		t.Fatalf("zero timeouts resolved to %v, %v; want the documented defaults", s.policy.StartTimeout, s.policy.StallTimeout)
	}
}

// helperPID reads the pid a worker's forkSleepHelper published, and registers a
// cleanup that kills that pid, by number, if it is still alive when the test
// ends. A failing test therefore leaves no `sleep 60` behind.
func helperPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
			if perr != nil || pid <= 1 {
				t.Fatalf("helper pid file holds %q", b)
			}
			t.Cleanup(func() {
				if pidAlive(pid) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker never published its helper pid: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitGone polls kill(pid, 0) for ESRCH. A grandchild whose parent died is
// reparented and reaped by init, so it is not gone the instant the signal is
// sent; the deadline bounds the wait.
func waitGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: helper pid %d still exists 10s later; the group was not killed", what, pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSupervisor_RetireKillsWorkerGroup(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		kill bool // the test kills the leader alone, so only the helper is left to the retirement
	}{
		{"leader exited", "", true},
		{"leader alive", "corrupt", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Policy{MaxRestarts: 1, Window: time.Minute}, func(int) string { return tc.mode })
			h.forkSleep = filepath.Join(t.TempDir(), "helper.pid")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			g, err := h.s.ensure(ctx)
			if err != nil {
				t.Fatal(err)
			}
			helper := helperPID(t, h.forkSleep)
			if tc.kill {
				if err := syscall.Kill(g.proc.cmd.Process.Pid, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-g.settled:
			case <-time.After(10 * time.Second):
				t.Fatal("generation was never retired")
			}
			waitGone(t, helper, "after retire")
		})
	}
}

func TestProc_CloseTimeoutKillsWorkerGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "helper.pid")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), workerEnv+"=1", modeEnv+"=hang", forkSleepEnv+"="+path)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := Start(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	helper := helperPID(t, path)
	err = p.close(300 * time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("close = %v; a worker that ignores stdin EOF must be killed at the budget", err)
	}
	waitGone(t, helper, "after the Close timeout")
}

func TestStart_HandshakeFailureKillsWorkerGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "helper.pid")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), workerEnv+"=1", modeEnv+"=no-hello", forkSleepEnv+"="+path)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := Start(ctx, cmd); err == nil {
		t.Fatal("Start = nil; the worker never announces")
	}
	waitGone(t, helperPID(t, path), "after a failed handshake")
}

func TestOwnedGroup_NeverNamesAGroupItDoesNotOwn(t *testing.T) {
	own := func(pid int, attr *syscall.SysProcAttr) *exec.Cmd {
		return &exec.Cmd{Process: &os.Process{Pid: pid}, SysProcAttr: attr}
	}
	leader := &syscall.SysProcAttr{Setpgid: true}
	for _, tc := range []struct {
		name string
		cmd  *exec.Cmd
		want int
	}{
		{"own group", own(4242, leader), 4242},
		{"setsid leader", own(4242, &syscall.SysProcAttr{Setsid: true}), 4242},
		{"never started", &exec.Cmd{SysProcAttr: leader}, 0},
		{"no attributes", own(4242, nil), 0},
		{"not a leader", own(4242, &syscall.SysProcAttr{}), 0},
		{"joined another group", own(4242, &syscall.SysProcAttr{Setpgid: true, Pgid: 77}), 0},
		{"pid 1", own(1, leader), 0},
		{"pid 0", own(0, leader), 0},
		{"the host's own group", own(syscall.Getpgrp(), leader), 0},
	} {
		if got := ownedGroup(tc.cmd); got != tc.want {
			t.Errorf("%s: ownedGroup = %d; want %d", tc.name, got, tc.want)
		}
	}
	c := exec.Command("true")
	ownProcessGroup(c)
	if !c.SysProcAttr.Setpgid {
		t.Error("ownProcessGroup did not request a process group")
	}
	joined := exec.Command("true")
	joined.SysProcAttr = &syscall.SysProcAttr{Pgid: 77}
	ownProcessGroup(joined)
	if joined.SysProcAttr.Setpgid {
		t.Error("ownProcessGroup overrode a command that chose to join another group")
	}
}

func TestSupervisor_CrashPlusFailingReplacementSpendsOneRestart(t *testing.T) {
	// Generation 2 is the replacement and fails its hello. The crash that
	// caused it and that failure are one restart: with MaxRestarts 2 a third
	// start must still be allowed.
	var n atomic.Int32
	factory := func() (*exec.Cmd, error) {
		if n.Add(1) == 2 {
			return exec.Command("true"), nil
		}
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), workerEnv+"=1", modeEnv+"=")
		cmd.Stderr = &lockedBuffer{}
		return cmd, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Supervise(ctx, factory, Policy{MaxRestarts: 2, Window: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer ccancel()
	if _, err := s.Call(cctx, []byte{0, 0x42}); err != nil {
		t.Fatal(err)
	}
	g := func() *generation {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.live
	}()
	if err := syscall.Kill(g.proc.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-g.settled
	if got := s.restartsInWindow(); got != 1 {
		t.Fatalf("restarts charged after the crash = %d; want 1", got)
	}
	// The replacement fails its hello: that is the restart the crash paid for.
	if _, err := s.Call(cctx, []byte{0, 0x42}); err == nil {
		t.Fatal("Call on the failing replacement = nil")
	}
	if got := s.restartsInWindow(); got != 1 {
		t.Fatalf("restarts charged after crash + failing replacement = %d; want 1, not 2", got)
	}
	out, err := s.Call(cctx, []byte{0, 0x42})
	if err != nil || !bytes.Equal(out, []byte{0, 0x42}) {
		t.Fatalf("Call after crash + failing replacement = %v, %v; one restart is left and must be usable", out, err)
	}
	if got := n.Load(); got != 3 {
		t.Fatalf("starts = %d; want the first worker, the failed replacement and one more", got)
	}
	// The failing replacement did not widen the budget either: the failure of
	// an attempt nobody prepaid for charges, so the third failure is the end.
	if got := s.restartsInWindow(); got != 1 {
		t.Fatalf("restarts charged after a clean start = %d; want 1", got)
	}
}

func (s *Supervisor) restartsInWindow() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inWindowLocked(time.Now())
}

func TestSupervisor_DieAfterHelloIsNotRespawnedUntilCalled(t *testing.T) {
	const max = 3
	h := newHarness(t, Policy{MaxRestarts: max, Window: time.Hour}, func(int) string { return "die-on-first-job" })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	submit := func() *generation {
		id, err := h.s.Submit(ctx, []byte{0, 1})
		if err != nil {
			t.Fatalf("Submit = %v", err)
		}
		h.s.mu.Lock()
		g := h.s.tickets[id].gen
		h.s.mu.Unlock()
		if _, err := h.s.Wait(ctx, id); err == nil {
			t.Fatal("Wait = nil; the worker exits without answering")
		}
		select {
		case <-g.settled:
		case <-time.After(10 * time.Second):
			t.Fatal("generation never settled")
		}
		return g
	}

	submit()
	// The generation has died and been retired. Nothing has asked for another
	// worker, so none is running: no eager respawn, and exactly one charge.
	if got := h.starts.Load(); got != 1 {
		t.Fatalf("starts = %d after the only generation died with no caller waiting; want 1", got)
	}
	if got := h.s.restartsInWindow(); got != 1 {
		t.Fatalf("restarts charged = %d; want 1", got)
	}
	// Each call starts one replacement, and the budget ends the sequence:
	// the first process plus MaxRestarts replacements, no more.
	for want := 2; want <= max+1; want++ {
		submit()
		if got := int(h.starts.Load()); got != want {
			t.Fatalf("starts = %d; want %d", got, want)
		}
	}
	_, err := h.s.Submit(ctx, []byte{0, 1})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("Submit past the budget = %v; want ErrBudgetExhausted", err)
	}
	if got := h.starts.Load(); got != max+1 {
		t.Fatalf("starts = %d after the budget was spent; want %d", got, max+1)
	}
}

func TestErrBudgetExhausted_DistinguishesFromPoison(t *testing.T) {
	budget := budgetExhausted(Policy{MaxRestarts: 3, Window: time.Minute})
	if !errors.Is(budget, ErrBudgetExhausted) {
		t.Fatal("the budget error does not match ErrBudgetExhausted")
	}
	if !errors.Is(budget, gusset.ErrPoisoned) {
		t.Fatal("the budget error stopped matching ErrPoisoned; callers that only test for poison would miss it")
	}
	if !strings.Contains(budget.Error(), "isolate: restart budget exhausted (3 restarts in 1m0s)") {
		t.Fatalf("budget error text = %q", budget)
	}
	poisoned := workerExitError(nil, nil)
	if !errors.Is(poisoned, gusset.ErrPoisoned) {
		t.Fatal("a dead worker's error lost ErrPoisoned")
	}
	if errors.Is(poisoned, ErrBudgetExhausted) {
		t.Fatal("a plain poisoned worker matches ErrBudgetExhausted")
	}
}

func TestSupervisor_SubmitOnDyingGeneration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  Policy
		healthy bool // the replacement hello succeeds
		budget  bool // no restart is left for it
	}{
		{"replacement succeeds", Policy{MaxRestarts: 2, Window: time.Minute}, true, false},
		{"replacement fails", Policy{MaxRestarts: 2, Window: time.Minute}, false, false},
		{"budget spent", Policy{MaxRestarts: 0, Window: time.Minute}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n atomic.Int32
			factory := func() (*exec.Cmd, error) {
				if n.Add(1) > 1 && !tc.healthy {
					return exec.Command("true"), nil
				}
				cmd := exec.Command(os.Args[0])
				cmd.Env = append(os.Environ(), workerEnv+"=1", modeEnv+"=")
				cmd.Stderr = &lockedBuffer{}
				return cmd, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s, err := Supervise(ctx, factory, tc.policy)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			g, err := s.ensure(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(g.proc.cmd.Process.Pid, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			<-g.proc.procExited // the generation has died; the caller still holds it

			id, err := s.submitVia(ctx, []byte{0, 0x42}, g)
			switch {
			case tc.healthy && !tc.budget:
				if err != nil {
					t.Fatalf("Submit on a dying generation = %v; want it to succeed on the replacement", err)
				}
				out, werr := s.Wait(ctx, id)
				if werr != nil || !bytes.Equal(out, []byte{0, 0x42}) {
					t.Fatalf("Wait = %v, %v", out, werr)
				}
				if got := n.Load(); got != 2 {
					t.Fatalf("starts = %d; the retry must start exactly one replacement", got)
				}
			default:
				if err == nil {
					t.Fatal("Submit = nil; there is no usable replacement")
				}
				if strings.Contains(err.Error(), "Close it") {
					t.Fatalf("Submit = %q; the supervisor owns Close and must not advise the caller to", err)
				}
				if tc.budget && !errors.Is(err, ErrBudgetExhausted) {
					t.Fatalf("Submit = %v; want ErrBudgetExhausted", err)
				}
			}
			if got := n.Load(); got > 2 {
				t.Fatalf("starts = %d; the retry is bounded to one", got)
			}
		})
	}
}

func TestSupervisedRefusal_DropsTheCloseAdvice(t *testing.T) {
	for _, in := range []error{
		workerExitError(nil, nil),
		(&Proc{poisoned: true}).refusalLocked(),
	} {
		if !strings.Contains(in.Error(), "Close it") {
			t.Fatalf("fixture error %q has no Close advice to remove", in)
		}
		out := supervisedRefusal(in)
		if strings.Contains(out.Error(), "Close it") {
			t.Errorf("supervisedRefusal(%q) = %q", in, out)
		}
		if !errors.Is(out, gusset.ErrPoisoned) {
			t.Errorf("supervisedRefusal(%q) stopped matching ErrPoisoned", in)
		}
	}
	if !errors.Is(supervisedRefusal(gusset.ErrClosed), gusset.ErrClosed) {
		t.Error("ErrClosed was rewritten")
	}
	if supervisedRefusal(nil) != nil {
		t.Error("nil became an error")
	}
}

// A job submitted while the live worker is being SIGKILLed either runs on the
// replacement or fails with an error that does not tell the caller to Close.
// Run with -race.
func TestSupervisor_SubmitRacingSigkill(t *testing.T) {
	h := newHarness(t, Policy{MaxRestarts: 1000, Window: time.Hour}, nil)
	h.echo()
	const rounds = 12
	var advised atomic.Int32
	var succeeded atomic.Int32
	for round := 0; round < rounds; round++ {
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				id, err := h.s.Submit(ctx, []byte{0, 0x42})
				if err != nil {
					if strings.Contains(err.Error(), "Close it") || errors.Is(err, gusset.ErrClosed) {
						advised.Add(1)
						t.Errorf("Submit raced a SIGKILL and returned %q", err)
					}
					return
				}
				succeeded.Add(1)
				_, _ = h.s.Wait(ctx, id)
			}()
		}
		close(start)
		if pid := pidOf(h.s); pid != 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		wg.Wait()
	}
	if succeeded.Load() == 0 {
		t.Fatal("no Submit succeeded across the rounds; the race never reached a replacement")
	}
	if advised.Load() != 0 {
		t.Fatalf("%d Submits were told to Close", advised.Load())
	}
}
