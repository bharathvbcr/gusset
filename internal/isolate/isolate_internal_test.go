package isolate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
	"github.com/bharathvbcr/gusset/internal/hostprobe"
)

// workerEnv makes the test binary the worker: TestMain runs Serve on its own
// stdin and stdout instead of the tests, so both modes run the same Rust
// archive and the same diagnostic engine.
const workerEnv = "GUSSET_ISOLATE_WORKER"

const workerPool = 2

// Fault bytes: the worker's own process dies the way an uncatchable fault
// kills it — SIGKILL as the OOM killer or a driver reset delivers it, SIGABRT
// as Rust's abort() raises it. No diagnostic mode reaches either, and none
// may: the fuzz targets drive every mode.
const (
	faultKill  = 0xFE
	faultAbort = 0xFD
)

// modeEnv picks a misbehaving worker for the robustness tests; empty is the
// honest one.
const modeEnv = "GUSSET_ISOLATE_WORKER_MODE"

// execProbeEnv is a re-exec of this test binary that only checks whether the
// worker's protocol descriptor survived exec. It is not a worker.
const execProbeEnv = "GUSSET_ISOLATE_EXEC_PROBE"

func TestMain(m *testing.M) {
	if os.Getenv(execProbeEnv) == "1" {
		os.Exit(execProbe())
	}
	if os.Getenv(workerEnv) == "1" {
		os.Exit(workerMain())
	}
	os.Exit(m.Run())
}

// execProbeIDEnv carries the protocol pipe's identity ("dev:ino", from fstat
// in the worker) to the exec probe.
const execProbeIDEnv = "GUSSET_ISOLATE_EXEC_PROBE_ID"

// fdIdentity is the device and inode behind fd, the same for every
// descriptor that refers to one pipe.
func fdIdentity(fd int) (string, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino)), nil
}

// execProbe exits 0 when no descriptor in this process refers to the
// worker's protocol pipe and 2 when one does. It asks by identity, not by
// number: after exec closes a close-on-exec descriptor, the number is free and
// the runtime may take it. On Linux the Go runtime opens the cgroup cpu.max at
// start-up (containermaxprocs) and it lands on fd 3, so "fd 3 is open" failed
// on every Linux runner with CLOEXEC working (probe/w1-linux-evidence, run
// 40058658715). Fstat does not read the descriptor, so a leaked one cannot
// consume a protocol byte.
func execProbe() int {
	want := os.Getenv(execProbeIDEnv)
	if want == "" {
		fmt.Fprintln(os.Stderr, "probe:", execProbeIDEnv, "is not set")
		return 3
	}
	var rl syscall.Rlimit
	limit := 1024
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err == nil && rl.Cur < uint64(limit) {
		limit = int(rl.Cur)
	}
	open := 0
	for fd := 0; fd < limit; fd++ {
		id, err := fdIdentity(fd)
		if errors.Is(err, syscall.EBADF) {
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "probe: fstat fd %d: %v\n", fd, err)
			return 3
		}
		open++
		if id == want {
			fmt.Fprintf(os.Stderr, "probe: fd %d is the protocol pipe (%s) after exec\n", fd, id)
			return 2
		}
	}
	if open < 3 {
		fmt.Fprintf(os.Stderr, "probe: saw only %d open descriptors; stdio should be among them\n", open)
		return 3
	}
	return 0
}

func workerMain() int {
	out, err := ProtocolOut()
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		return 1
	}
	mode := os.Getenv(modeEnv)
	if mode == "nostdin" {
		_ = os.Stdin.Close() // before hello: the host's first write fails
	}
	if mode != "" && mode != "stray" {
		return misbehave(mode, out)
	}
	if mode == "stray" {
		fmt.Fprint(os.Stdout, "INFO engine ready\n")
	}
	h, err := gusset.Open(gusset.WithPoolSize(workerPool), gusset.WithDiagnosticEngine())
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker open:", err)
		return 1
	}
	serveErr := Serve(os.Stdin, out, faulty{h}, workerPool)
	closeErr := h.Close()
	if err := errors.Join(serveErr, closeErr); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		return 1
	}
	return 0
}

// misbehave announces itself and then fails the protocol one way, staying
// alive so only the host can end it.
func misbehave(mode string, out *os.File) int {
	if mode == "grandchild" {
		// A grandchild holding every pipe the worker has outlives it. It is
		// started before hello, so it already holds them when Start returns:
		// a kill right after Start cannot beat it to the pipe.
		gc := exec.Command("sleep", "4")
		gc.Stdout, gc.Stderr = out, os.Stderr
		if err := gc.Start(); err != nil {
			return 1
		}
	}
	if err := writeFrame(out, &response{Op: opHello, Version: protocolVersion, PoolSize: workerPool}, maxResponseFrame); err != nil {
		return 1
	}
	switch mode {
	case "corrupt":
		_, _ = out.Write([]byte{0, 0, 0, 5, 'x', 'x', 'x', 'x', 'x'})
	case "oversize":
		// A length one past what the host will ever accept; no body follows.
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], maxResponseFrame+1)
		_, _ = out.Write(hdr[:])
	case "truncated":
		// A frame that promises 1000 bytes, delivers 10, and ends the stream.
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], 1000)
		_, _ = out.Write(append(hdr[:], "0123456789"...))
		return 0
	case "stall":
		// The same promise, kept alive: the worker is up and silent mid-frame.
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], 1000)
		_, _ = out.Write(append(hdr[:], "0123456789"...))
	case "dup", "reorder", "unknown":
		return scripted(mode, out)
	case "answer-hold":
		return answerThenHold(out)
	case "cloexec":
		return execProtocolChild()
	}
	time.Sleep(time.Minute)
	return 0
}

// answerThenHold writes one result for the first submit, then keeps the
// protocol write end open from a grandchild and exits. The frame is already
// in the pipe when the worker dies; Wait must return that payload, not only
// the poison from the missing EOF.
func answerThenHold(out *os.File) int {
	br := bufio.NewReader(os.Stdin)
	var req request
	if err := readFrame(br, maxRequestFrame, &req); err != nil {
		fmt.Fprintln(os.Stderr, "answer-hold:", err)
		return 1
	}
	if err := writeFrame(out, &response{ID: req.ID, Out: append([]byte(nil), req.In...)}, maxResponseFrame); err != nil {
		fmt.Fprintln(os.Stderr, "answer-hold:", err)
		return 1
	}
	gc := exec.Command("sleep", "4")
	gc.Stdout, gc.Stderr = out, os.Stderr
	if err := gc.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "answer-hold:", err)
		return 1
	}
	os.Exit(0)
	return 0
}

// execProtocolChild replaces this worker with a probe. The protocol pipe must
// not be open there: ProtocolOut marks it close-on-exec.
func execProtocolChild() int {
	fd, err := strconv.Atoi(os.Getenv(EnvProtocolFD))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cloexec probe:", err)
		return 1
	}
	id, err := fdIdentity(fd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cloexec probe: fstat protocol fd:", err)
		return 1
	}
	env := make([]string, 0, len(os.Environ())+2)
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if k == workerEnv || k == modeEnv || k == execProbeEnv || k == execProbeIDEnv {
			continue
		}
		env = append(env, e)
	}
	env = append(env, execProbeEnv+"=1", execProbeIDEnv+"="+id)
	cmd := exec.Command(os.Args[0])
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "cloexec probe:", err)
		return 1
	}
	return 0
}

// faulty kills its own process on a fault byte and passes everything else to
// the handle.
type faulty struct{ *gusset.Handle }

func (f faulty) Submit(ctx context.Context, in any) (uint64, error) {
	if os.Getenv(modeEnv) == "stray" {
		fmt.Fprint(os.Stdout, "INFO engine ready\n")
	}
	if b, ok := in.([]byte); ok && len(b) > 0 {
		switch b[0] {
		case faultKill:
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			// The signal is asynchronous: falling through would let the job
			// answer before it lands. Nothing may produce a result.
			select {}
		case faultAbort:
			_ = syscall.Kill(os.Getpid(), syscall.SIGABRT)
			select {}
		}
	}
	return f.Handle.Submit(ctx, in)
}

func startWorker(t *testing.T) *Proc {
	t.Helper()
	return startWorkerMode(t, "")
}

func startWorkerMode(t *testing.T, mode string) *Proc {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), workerEnv+"=1", modeEnv+"="+mode)
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := Start(ctx, cmd)
	if err != nil {
		t.Fatalf("Start: %v\nworker stderr:\n%s", err, stderr.String())
	}
	t.Cleanup(func() {
		_ = p.close(5 * time.Second)
		if t.Failed() {
			t.Logf("worker stderr:\n%s", stderr.String())
		}
	})
	return p
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// modes is the evidence the spike asks for: every parity test runs the same
// assertions against the in-process C ABI handle and the worker process.
var modes = []struct {
	name string
	open func(t *testing.T) Caller
}{
	{"inproc", func(t *testing.T) Caller {
		h, err := gusset.Open(gusset.WithPoolSize(workerPool), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Close() })
		return h
	}},
	{"proc", func(t *testing.T) Caller { return startWorker(t) }},
}

func forEachMode(t *testing.T, f func(t *testing.T, c Caller)) {
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			f(t, m.open(t))
		})
	}
}

func bg(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestParity_CallEchoes(t *testing.T) {
	forEachMode(t, func(t *testing.T, c Caller) {
		in := []byte{0, 1, 2, 3, 0xFF}
		out, err := c.Call(bg(t), in)
		if err != nil || !bytes.Equal(out, in) {
			t.Fatalf("Call = %v, %v; want %v, nil", out, err, in)
		}
		// Mode 10 returns a computed value, not the input.
		out, err = c.Call(bg(t), []byte{10, 2, 3})
		if err != nil || binary.LittleEndian.Uint64(out) != 13 {
			t.Fatalf("sum of squares = %v, %v; want 13", out, err)
		}
	})
}

func TestParity_TicketIsAwaitedOnce(t *testing.T) {
	forEachMode(t, func(t *testing.T, c Caller) {
		ticket, err := c.Submit(bg(t), []byte{0, 7})
		if err != nil {
			t.Fatal(err)
		}
		if out, err := c.Wait(bg(t), ticket); err != nil || !bytes.Equal(out, []byte{0, 7}) {
			t.Fatalf("Wait = %v, %v", out, err)
		}
		if _, err := c.Wait(bg(t), ticket); !errors.Is(err, gusset.ErrUnknownTicket) {
			t.Fatalf("second Wait = %v; want ErrUnknownTicket", err)
		}
		if _, err := c.Wait(bg(t), ticket+1000); !errors.Is(err, gusset.ErrUnknownTicket) {
			t.Fatalf("Wait on a never-issued ticket = %v; want ErrUnknownTicket", err)
		}
	})
}

func TestParity_SecondWaiterIsBusy(t *testing.T) {
	forEachMode(t, func(t *testing.T, c Caller) {
		const jobRuns = time.Second
		submitted := time.Now()
		ticket, err := c.Submit(bg(t), []byte{9, byte(jobRuns / (10 * time.Millisecond))})
		if err != nil {
			t.Fatal(err)
		}
		// Two waiters start together. Which registers first is not for the
		// test to arrange (it used to nap 50 ms and hope, and a loaded host
		// let the second one win): exactly one is refused with ErrTicketBusy
		// at once, and the other owns the ticket and gets the result.
		results := make(chan error, 2)
		gate := make(chan struct{})
		for i := 0; i < 2; i++ {
			go func() {
				<-gate
				_, err := c.Wait(bg(t), ticket)
				results <- err
			}()
		}
		close(gate)
		refused := <-results
		if !errors.Is(refused, gusset.ErrTicketBusy) {
			// The owner cannot finish before the job does, so a first return
			// before then that is not the refusal is a defect; after it, the
			// host stalled the refused waiter past the job and there is
			// nothing left to probe.
			if since := time.Since(submitted); since < jobRuns {
				t.Fatalf("first Wait to return (after %v, before the job could end) = %v; want ErrTicketBusy", since, refused)
			}
			hostprobe.Skip(t, "the first waiter returned past the job's end: nothing left to probe on this host")
		}
		if err := <-results; err != nil {
			t.Fatalf("owner Wait = %v; want the result", err)
		}
	})
}

// The detach contract (DECISIONS 2026-09-20): the caller returns at its
// deadline while a non-cooperative engine keeps running. The bound is
// relative to the engine, as that row requires.
func TestParity_DeadlineBoundsTheCaller(t *testing.T) {
	// 2.5 s of engine against a 50 ms deadline and a 1.25 s bound: a caller
	// detached at its deadline lands near 50 ms and one that waited for the
	// engine lands past 2.5 s, so a host that is slow to schedule the caller
	// still falls on the right side of the gap.
	const engineRuns = 2500 * time.Millisecond
	forEachMode(t, func(t *testing.T, c Caller) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := c.Call(ctx, []byte{9, byte(engineRuns / (10 * time.Millisecond))})
		elapsed := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Call = %v; want DeadlineExceeded", err)
		}
		if elapsed > engineRuns/2 {
			t.Fatalf("caller returned after %v; the engine runs %v", elapsed, engineRuns)
		}
		// The abandoned job holds one permit; the handle still serves.
		if out, err := c.Call(bg(t), []byte{0, 1}); err != nil || !bytes.Equal(out, []byte{0, 1}) {
			t.Fatalf("Call after a detached deadline = %v, %v", out, err)
		}
	})
}

func TestParity_PanicPoisons(t *testing.T) {
	forEachMode(t, func(t *testing.T, c Caller) {
		_, err := c.Call(bg(t), []byte{1})
		if !errors.Is(err, gusset.ErrPanic) {
			t.Fatalf("panicking Call = %v; want ErrPanic", err)
		}
		if !strings.Contains(err.Error(), "plain panic") {
			t.Fatalf("panic message lost: %v", err)
		}
		if _, err := c.Call(bg(t), []byte{0}); !errors.Is(err, gusset.ErrPoisoned) {
			t.Fatalf("Call after panic = %v; want ErrPoisoned", err)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close of a poisoned handle = %v", err)
		}
		if _, err := c.Call(bg(t), []byte{0}); !errors.Is(err, gusset.ErrClosed) {
			t.Fatalf("Call after Close of a poisoned handle = %v; want ErrClosed", err)
		}
	})
}

func TestParity_DiscardSpendsTheTicket(t *testing.T) {
	forEachMode(t, func(t *testing.T, c Caller) {
		running, err := c.Submit(bg(t), []byte{9, 10})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Discard(running); err != nil {
			t.Fatalf("Discard of a running job = %v", err)
		}
		if _, err := c.Wait(bg(t), running); !errors.Is(err, gusset.ErrUnknownTicket) {
			t.Fatalf("Wait after Discard = %v; want ErrUnknownTicket", err)
		}
		if err := c.Discard(running); !errors.Is(err, gusset.ErrUnknownTicket) {
			t.Fatalf("second Discard = %v; want ErrUnknownTicket", err)
		}

		finished, err := c.Submit(bg(t), []byte{0, 2})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond) // let the result land with no waiter
		if err := c.Discard(finished); err != nil {
			t.Fatalf("Discard of a stored result = %v", err)
		}
		// Both permits came back: two more jobs fit a pool of two.
		for range workerPool {
			if _, err := c.Call(bg(t), []byte{0}); err != nil {
				t.Fatalf("Call after Discard = %v", err)
			}
		}
	})
}

func TestParity_InputLimitAndTypes(t *testing.T) {
	forEachMode(t, func(t *testing.T, c Caller) {
		if _, err := c.Call(bg(t), make([]byte, maxInlineInput+1)); !errors.Is(err, gusset.ErrInputTooLarge) {
			t.Fatalf("Call with %d bytes = %v; want ErrInputTooLarge", maxInlineInput+1, err)
		}
		if _, err := c.Submit(bg(t), "text"); err == nil {
			t.Fatal("Submit of a string succeeded")
		}
		//lint:ignore SA1012 the nil context is the input under test
		if _, err := c.Call(nil, []byte{0}); !errors.Is(err, gusset.ErrNilContext) {
			t.Fatalf("Call(nil ctx) = %v; want ErrNilContext", err)
		}
	})
}

type carrier struct{}

func (carrier) TraceID() [16]byte {
	return [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
}
func (carrier) SpanID() [8]byte { return [8]byte{0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8} }

func TestParity_TraceCrossesTheBoundary(t *testing.T) {
	forEachMode(t, func(t *testing.T, c Caller) {
		ctx := context.WithValue(bg(t), gusset.SpanContextKey, carrier{})
		out, err := c.Call(ctx, []byte{7})
		if err != nil {
			t.Fatal(err)
		}
		tid, sid := carrier{}.TraceID(), carrier{}.SpanID()
		if want := append(tid[:], sid[:]...); !bytes.Equal(out, want) {
			t.Fatalf("mode 7 echoed %x; want %x", out, want)
		}
	})
}

func TestParity_CloseReleasesAndRefuses(t *testing.T) {
	forEachMode(t, func(t *testing.T, c Caller) {
		ticket, err := c.Submit(bg(t), []byte{5, 100}) // cooperative, 1 s
		if err != nil {
			t.Fatal(err)
		}
		waited := make(chan error, 1)
		go func() { _, err := c.Wait(bg(t), ticket); waited <- err }()
		time.Sleep(20 * time.Millisecond)
		if err := c.Close(); err != nil {
			t.Fatalf("Close = %v", err)
		}
		if err := <-waited; !errors.Is(err, gusset.ErrClosed) {
			t.Fatalf("waiter released by Close = %v; want ErrClosed", err)
		}
		if _, err := c.Call(bg(t), []byte{0}); !errors.Is(err, gusset.ErrClosed) {
			t.Fatalf("Call after Close = %v; want ErrClosed", err)
		}
		if err := c.Discard(ticket); !errors.Is(err, gusset.ErrClosed) {
			t.Fatalf("Discard after Close = %v; want ErrClosed", err)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("second Close = %v", err)
		}
	})
}

// The point of the mode: a fault no in-process firewall survives ends the
// worker, fails its in-flight work as poisoned, and leaves this process — the
// test binary is the Go service here — running and able to start another.
func TestIsolation_UncatchableFaultEndsOnlyTheWorker(t *testing.T) {
	for _, fault := range []struct {
		name   string
		b      byte
		signal []string // any one names the exit
	}{
		{"SIGKILL", faultKill, []string{"signal: killed"}},
		// Go's own handler takes SIGABRT, prints the crash dump and exits 2,
		// so an abort() from Rust code reaches the host as this status too.
		// A process-directed signal can land on one of the engine's own
		// threads instead, where cgo forwards it to the default action: Linux
		// then reports the signal.
		{"SIGABRT", faultAbort, []string{"exit status 2", "signal: aborted"}},
	} {
		t.Run(fault.name, func(t *testing.T) {
			t.Parallel()
			p := startWorker(t)
			inflight, err := p.Submit(bg(t), []byte{9, 200}) // 2 s
			if err != nil {
				t.Fatal(err)
			}
			waited := make(chan error, 1)
			go func() { _, err := p.Wait(bg(t), inflight); waited <- err }()

			_, err = p.Call(bg(t), []byte{fault.b})
			if !errors.Is(err, gusset.ErrPoisoned) {
				t.Fatalf("faulting Call = %v; want ErrPoisoned", err)
			}
			named := false
			for _, want := range fault.signal {
				named = named || strings.Contains(err.Error(), want)
			}
			if !named {
				t.Fatalf("worker exit not named (want one of %q): %v", fault.signal, err)
			}
			select {
			case err := <-waited:
				if !errors.Is(err, gusset.ErrPoisoned) {
					t.Fatalf("in-flight Wait = %v; want ErrPoisoned", err)
				}
			case <-time.After(time.Second):
				t.Fatal("in-flight waiter still parked a second after the worker died")
			}
			if _, err := p.Submit(bg(t), []byte{0}); !errors.Is(err, gusset.ErrPoisoned) {
				t.Fatalf("Submit after worker death = %v; want ErrPoisoned", err)
			}
			if err := p.Close(); err != nil {
				t.Fatalf("Close after worker death = %v", err)
			}

			again := startWorker(t)
			if out, err := again.Call(bg(t), []byte{0, 9}); err != nil || !bytes.Equal(out, []byte{0, 9}) {
				t.Fatalf("replacement worker Call = %v, %v", out, err)
			}
		})
	}
}

// Out of process, Close can end a worker whose engine never returns: its
// memory is its own. In process that is the 2026-09-30 close-budget error and
// the pool stays allocated.
func TestIsolation_CloseKillsAWedgedWorker(t *testing.T) {
	// The job has to be inside the engine when Close lands: closing first
	// cancels it before it starts, and the worker then exits inside the budget
	// with nothing to kill. There is no signal for "the engine has it" across
	// the process boundary, so a nap stands in for one, and a host too loaded
	// for the first nap to cover the job's start gets a longer one on a fresh
	// worker. A Close that does not kill a wedged worker fails every attempt,
	// because the engine outlasts all the naps (2.55 s against 1.5 s).
	var err error
	var elapsed time.Duration
	for _, nap := range []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 1500 * time.Millisecond} {
		p := startWorker(t)
		if _, serr := p.Submit(bg(t), []byte{9, 255}); serr != nil { // 2.55 s, non-cooperative
			t.Fatal(serr)
		}
		time.Sleep(nap)
		start := time.Now()
		err = p.close(100 * time.Millisecond)
		elapsed = time.Since(start)
		if err != nil && strings.Contains(err.Error(), "killed") {
			break
		}
	}
	if err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("close past budget = %v; want the kill reported", err)
	}
	if elapsed > time.Second {
		t.Fatalf("close took %v with a 100ms budget", elapsed)
	}
}

func TestStart_RefusesAWorkerThatNeverAnnounces(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Start(ctx, cmd); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start = %v; want the handshake deadline", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("the silent worker was not reaped")
	}
}

func TestReadFrame_RefusesOversizeBeforeAllocating(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], maxRequestFrame+1)
	var req request
	err := readFrame(bufio.NewReader(bytes.NewReader(hdr[:])), maxRequestFrame, &req)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("readFrame = %v; want the size refusal", err)
	}
	// A frame cut short is unexpected, not a clean end.
	binary.BigEndian.PutUint32(hdr[:], 10)
	err = readFrame(bufio.NewReader(bytes.NewReader(append(hdr[:], '{'))), maxRequestFrame, &req)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated frame = %v; want ErrUnexpectedEOF", err)
	}
}

func TestServe_FailsLoudOnProtocolErrors(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	for _, tc := range []struct {
		name string
		reqs []request
		want string
	}{
		{"unknown op", []request{{Op: "exec"}}, "unknown op"},
		{"duplicate id", []request{{Op: opSubmit, ID: 1, In: []byte{9, 50}}, {Op: opSubmit, ID: 1}}, "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var in bytes.Buffer
			for _, r := range tc.reqs {
				if err := writeFrame(&in, &r, maxRequestFrame); err != nil {
					t.Fatal(err)
				}
			}
			err := Serve(&in, io.Discard, h, 1)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Serve = %v; want %q", err, tc.want)
			}
		})
	}
	if err := Serve(&bytes.Buffer{}, io.Discard, h, 0); err == nil {
		t.Fatal("Serve accepted pool size 0")
	}
}

func TestWireError_KeepsErrorsIs(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want []error
	}{
		{&gusset.Error{Code: gusset.ErrGeneric.Code, Msg: "cancelled: DeadlineExceeded"}, []error{gusset.ErrGeneric, context.DeadlineExceeded}},
		{&gusset.Error{Code: gusset.ErrPanic.Code, Msg: "boom", File: "x.rs", Line: 3}, []error{gusset.ErrPanic}},
		{fmt.Errorf("wrapped: %w", gusset.ErrClosed), []error{gusset.ErrClosed}},
		{fmt.Errorf("wrapped: %w", context.Canceled), []error{context.Canceled}},
		{gusset.ErrShutdown, []error{gusset.ErrShutdown}},
	} {
		got := encodeError(tc.err).decode()
		if got.Error() != tc.err.Error() {
			t.Errorf("text %q became %q", tc.err, got)
		}
		for _, w := range tc.want {
			if !errors.Is(got, w) {
				t.Errorf("%q no longer matches %v", got, w)
			}
		}
	}
}

// The one place the modes are meant to differ (DECISIONS 2026-10-08): a
// *gusset.Buffer is Rust memory in the calling process, so the worker cannot
// be handed one until a shared-memory transport exists. Proc refuses it
// without consuming it; the handle that made it still takes it. A change
// that makes Buffers cross must change this test, not slip past it.
func TestParity_BufferInputDiverges(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	buf, err := h.NewBuffer(8192)
	if err != nil {
		t.Fatal(err)
	}
	buf.Bytes()[0] = 0 // echo mode

	p := startWorker(t)
	if _, err := p.Submit(bg(t), buf); !errors.Is(err, ErrBufferUnsupported) {
		t.Fatalf("Proc.Submit(*Buffer) = %v; want ErrBufferUnsupported", err)
	}
	if _, err := p.Submit(bg(t), (*gusset.Buffer)(nil)); !errors.Is(err, gusset.ErrNilBuffer) {
		t.Fatalf("Proc.Submit(nil *Buffer) = %v; want ErrNilBuffer", err)
	}
	// The refusal took no permit: the pool still runs work.
	for range workerPool + 1 {
		if _, err := p.Call(bg(t), []byte{0}); err != nil {
			t.Fatalf("Proc.Call after the refusal = %v", err)
		}
	}

	out, err := h.CallBuffer(bg(t), buf)
	if err != nil {
		t.Fatalf("in-process CallBuffer of the refused buffer = %v", err)
	}
	if len(out.Bytes()) != 8192 {
		t.Fatalf("in-process echo returned %d bytes; want 8192", len(out.Bytes()))
	}
	if err := out.Free(); err != nil {
		t.Fatal(err)
	}
	if err := buf.Free(); err != nil {
		t.Fatalf("Free after the refusal = %v; the refusal must not consume the buffer", err)
	}
}

// within fails the test unless f returns inside d, without waiting on f past
// it.
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
	}
}

// A live worker that emits a corrupt frame is dead to the host: every waiter
// gets ErrPoisoned at once, not at its own deadline.
func TestProc_CorruptFrameLatchesTheWorkerDead(t *testing.T) {
	p := startWorkerMode(t, "corrupt")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	var err error
	within(t, 3*time.Second, "Call", func() { _, err = p.Call(ctx, []byte{0}) })
	if !errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("Call = %v; want ErrPoisoned", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waiter released after %v, near its 5s deadline", elapsed)
	}
}

// An engine's own stdout writes are not protocol.
func TestProc_StrayStdoutCannotCorruptTheProtocol(t *testing.T) {
	p := startWorkerMode(t, "stray")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		out, err := p.Call(ctx, []byte{0, byte(i)})
		if err != nil || !bytes.Equal(out, []byte{0, byte(i)}) {
			t.Fatalf("Call %d = %v, %v", i, out, err)
		}
	}
}

func TestReadFrame_AllocatesOnlyWhatItRead(t *testing.T) {
	const limit = 1 << 30
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 1<<29)
	r := bufio.NewReader(bytes.NewReader(append(hdr[:], "{\"op\":"...)))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var resp response
	err := readFrame(r, limit, &resp)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("readFrame = %v; want ErrUnexpectedEOF", err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 8<<20 {
		t.Fatalf("readFrame allocated %d bytes for a 7-byte body", got)
	}
}

func TestClose_BoundedWithAGrandchildHoldingTheWorkersPipes(t *testing.T) {
	p := startWorkerMode(t, "grandchild")
	start := time.Now()
	var err error
	within(t, 3*time.Second, "close", func() { err = p.close(100 * time.Millisecond) })
	if err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("close = %v; want the kill reported", err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("close took %v with a 100ms budget; the 4s grandchild held it", elapsed)
	}
}

func TestClose_BoundedWhileAWriterIsBlockedOnAFullStdin(t *testing.T) {
	p := startWorkerMode(t, "deaf") // never reads stdin
	writer := make(chan error, 1)
	go func() {
		in := make([]byte, maxInlineInput)
		for id := uint64(1); ; id++ {
			if err := p.send(context.Background(), &request{Op: opSubmit, ID: id, In: in}); err != nil {
				writer <- err
				return
			}
		}
	}()
	select {
	case err := <-writer:
		t.Fatalf("writer ended before the pipe filled: %v", err)
	case <-time.After(300 * time.Millisecond): // blocked, holding wmu
	}
	start := time.Now()
	within(t, 3*time.Second, "close", func() { _ = p.close(100 * time.Millisecond) })
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("close took %v with a 100ms budget", elapsed)
	}
	select {
	case <-writer:
	case <-time.After(2 * time.Second):
		t.Fatal("the blocked writer was not released by Close")
	}
}

func TestSubmit_ReturnsCtxErrWhileTheWorkerHasNotYetBeenLatchedDead(t *testing.T) {
	p := startWorkerMode(t, "nostdin") // alive, but every write to it fails
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var err error
	within(t, 3*time.Second, "Submit", func() { _, err = p.Submit(ctx, []byte{0}) })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Submit = %v; want ctx.Err()", err)
	}
}

// blockOnFullStdin fills the worker's stdin and parks one goroutine inside
// send. The caller must not return that goroutine; Close does.
func blockOnFullStdin(t *testing.T, p *Proc) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		in := make([]byte, maxInlineInput)
		for id := uint64(1); ; id++ {
			if err := p.send(context.Background(), &request{Op: opSubmit, ID: id, In: in}); err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
		t.Fatal("writer ended before the pipe filled")
	case <-time.After(300 * time.Millisecond):
	}
	return done
}

// A deaf worker never reads stdin. Once the pipe is full, a Submit whose
// context ends in 100ms must return DeadlineExceeded instead of waiting out
// the writer that holds the pipe, and the permit comes back unless the
// partial write poisoned the Proc.
func TestProc_FullStdinSubmitHonorsDeadline(t *testing.T) {
	p := startWorkerMode(t, "deaf")
	blocked := blockOnFullStdin(t, p)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := p.Submit(ctx, []byte{0})
		got <- err
	}()
	var err error
	select {
	case err = <-got:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Submit did not return within 500ms; a full stdin pipe held it past the deadline")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Submit returned after %v", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Submit = %v; want DeadlineExceeded", err)
	}
	p.mu.Lock()
	poisoned := p.poisoned
	p.mu.Unlock()
	if !poisoned && len(p.sem) != 0 {
		t.Fatalf("permit leaked: sem len %d and the Proc is not poisoned", len(p.sem))
	}
	select {
	case <-blocked:
		if !poisoned {
			t.Fatal("the blocked send finished without the Proc being poisoned")
		}
	default:
	}
}

// Wait's best-effort cancel must not run on the caller's goroutine. A send
// already blocked on a full stdin pipe holds the write lock; Wait used to
// take that lock before returning ctx.Err().
func TestProc_WaitReturnsWhileSendBlocked(t *testing.T) {
	p := startWorkerMode(t, "deaf")
	ticket, err := p.Submit(context.Background(), []byte{0})
	if err != nil {
		t.Fatal(err)
	}
	blocked := blockOnFullStdin(t, p)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := p.Wait(ctx, ticket)
		got <- err
	}()
	select {
	case err = <-got:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Wait did not return within 500ms while another send held the pipe")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Wait returned after %v", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v; want DeadlineExceeded", err)
	}
	select {
	case <-blocked:
		t.Fatal("Wait returned only after the blocked send finished")
	default:
	}
}

// The Submit above times out waiting for the write lock. This one is the
// writer: the pipe is already full and no other goroutine holds the lock, so
// the deadline has to interrupt the write itself. That write may be partial,
// so the Proc is poisoned and the permit is still restored.
// fillStdin packs the worker's stdin until a write hits its deadline. The
// pipe stays full and writeMu stays free: nothing is left blocked in send.
func fillStdin(t *testing.T, p *Proc) {
	t.Helper()
	buf := bytes.Repeat([]byte{'x'}, 256<<10)
	if err := p.stdin.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	filled := false
	for range 8 {
		if _, err := p.stdin.Write(buf); err != nil {
			filled = true
			break
		}
	}
	if err := p.stdin.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if !filled {
		t.Fatal("stdin accepted 2 MiB; the pipe never filled")
	}
}

func TestProc_SubmitWriteOnFullPipeHonorsDeadline(t *testing.T) {
	p := startWorkerMode(t, "deaf")
	fillStdin(t, p)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got := make(chan error, 1)
	go func() {
		_, err := p.Submit(ctx, []byte{0})
		got <- err
	}()
	var err error
	select {
	case err = <-got:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Submit did not return within 500ms of a full-pipe write")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Submit = %v; want DeadlineExceeded", err)
	}
	p.mu.Lock()
	poisoned := p.poisoned
	p.mu.Unlock()
	if !poisoned {
		t.Fatal("a deadline during the write did not poison the Proc")
	}
	if len(p.sem) != 0 {
		t.Fatalf("permit leaked: sem len %d", len(p.sem))
	}
}

// SIGKILL while a grandchild holds the protocol write end used to leave read
// blocked until that fd closed, so cmd.Wait never ran and a Submit with no
// deadline never returned. The reaper must poison the Proc without Close.
func TestProc_ExternalKillWithGrandchildPoisonsQuickly(t *testing.T) {
	p := startWorkerMode(t, "grandchild")
	ticket, err := p.Submit(context.Background(), []byte{0})
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() {
		_, err := p.Wait(context.Background(), ticket)
		waited <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(p.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	killed := time.Now()
	select {
	case err := <-waited:
		if !errors.Is(err, gusset.ErrPoisoned) {
			t.Fatalf("parked Wait = %v; want ErrPoisoned", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked waiter not released within 2s of SIGKILL; read is still waiting for EOF")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := p.Submit(ctx, []byte{1}); !errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("Submit after SIGKILL = %v; want ErrPoisoned", err)
	}
	if elapsed := time.Since(killed); elapsed > 2*time.Second {
		t.Fatalf("Submit still not poisoned %v after SIGKILL", elapsed)
	}
	start := time.Now()
	if err := p.Close(); err != nil {
		t.Fatalf("Close = %v; the reader should already have finished", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %v; want well under the close budget", elapsed)
	}
}

func TestProc_ProtocolFDIsCloseOnExec(t *testing.T) {
	p := startWorkerMode(t, "cloexec")
	deadline := time.Now().Add(3 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, last = p.Submit(ctx, []byte{0})
		cancel()
		if errors.Is(last, gusset.ErrPoisoned) {
			p.mu.Lock()
			waitErr := p.waitErr
			p.mu.Unlock()
			if waitErr != nil {
				t.Fatalf("worker exit %v; the protocol fd was still open in the child, or the probe failed", waitErr)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("worker did not exit after the exec probe; last Submit: %v", last)
}

func TestProc_TypedNilTraceCarrierReturnsError(t *testing.T) {
	p := startWorker(t)
	var c *nilReceiverCarrier
	ctx := context.WithValue(context.Background(), gusset.SpanContextKey, c)
	_, err := p.Call(ctx, []byte{0})
	if err == nil {
		t.Fatal("Call with a typed-nil trace carrier returned a nil error")
	}
	out, err := p.Call(bg(t), []byte{0, 1})
	if err != nil || !bytes.Equal(out, []byte{0, 1}) {
		t.Fatalf("Call after the typed-nil carrier = %v, %v", out, err)
	}
}

// nilReceiverCarrier panics on a nil pointer, which is what a typed nil
// stored in a TraceCarrier interface does. A value with nil-safe methods
// must still be callable; this one is not.
type nilReceiverCarrier struct{ id byte }

func (c *nilReceiverCarrier) TraceID() [16]byte { return [16]byte{c.id} }
func (c *nilReceiverCarrier) SpanID() [8]byte   { return [8]byte{c.id} }

// Close marks the ticket abandoned and closed under one lock, then waits out
// its budget. A Wait that enters the lookup in that window must match
// ErrClosed. The already-registered waiter is a different path: it is sent
// errProcClosed before it looks the ticket up again.
func TestWait_DuringCloseIsErrClosed(t *testing.T) {
	p := startWorkerMode(t, "deaf")
	ticket, err := p.Submit(context.Background(), []byte{0})
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- p.close(2 * time.Second) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		closed := p.closed
		abandoned := false
		if tkt := p.tickets[ticket]; tkt != nil {
			abandoned = tkt.abandoned
		}
		p.mu.Unlock()
		if closed && abandoned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not mark the in-flight ticket abandoned")
		}
		time.Sleep(time.Millisecond)
	}
	_, err = p.Wait(context.Background(), ticket)
	if !errors.Is(err, gusset.ErrClosed) || errors.Is(err, gusset.ErrUnknownTicket) {
		t.Fatalf("Wait during Close = %v; want ErrClosed, not ErrUnknownTicket", err)
	}
	select {
	case cerr := <-errCh:
		if cerr == nil || !strings.Contains(cerr.Error(), "killed") {
			t.Fatalf("close = %v; want the budget kill", cerr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}
}

// Wait's cancel used to call send(context.Background()). On a full pipe that
// goroutine acquired writeMu and blocked in the write forever, and every
// later Wait queued another goroutine behind it. The cancel's own deadline
// has to poison the Proc or give the lock back.
func TestProc_CancelSendDoesNotPinTheWriteLock(t *testing.T) {
	p := startWorkerMode(t, "deaf")
	ticket, err := p.Submit(context.Background(), []byte{0x11})
	if err != nil {
		t.Fatal(err)
	}
	fillStdin(t, p)

	p.mu.Lock()
	ids := make([]uint64, 0, 20)
	ids = append(ids, ticket)
	for n := 0; len(ids) < 20; n++ {
		id := uint64(10_000 + n)
		p.tickets[id] = &ticketState{}
		ids = append(ids, id)
	}
	p.mu.Unlock()

	before := runtime.NumGoroutine()
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		start := time.Now()
		_, werr := p.Wait(ctx, id)
		cancel()
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("Wait %d took %v", id, elapsed)
		}
		if !errors.Is(werr, context.DeadlineExceeded) {
			t.Fatalf("Wait %d = %v; want DeadlineExceeded", id, werr)
		}
	}

	// cancelSendBudget is 1s and the waits above already overlapped it. A
	// following Submit must return inside 2s, not for the life of the Proc.
	subCtx, subCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	subDone := make(chan error, 1)
	go func() {
		_, err := p.Submit(subCtx, []byte{0})
		subDone <- err
	}()
	var subErr error
	select {
	case subErr = <-subDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit blocked; a cancel send is holding writeMu with no deadline")
	}
	subCancel()
	p.mu.Lock()
	poisoned := p.poisoned
	p.mu.Unlock()
	if !poisoned {
		select {
		case p.writeMu <- struct{}{}:
			<-p.writeMu
		default:
			t.Fatalf("Submit = %v; writeMu still held and the Proc is not poisoned", subErr)
		}
	}

	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before+8 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines %d -> %d; cancel sends still parked", before, runtime.NumGoroutine())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The deaf worker ignores the closed stdin, so any budget ends in a kill;
	// the public Close's 30s would only be waited out.
	if err := p.close(100 * time.Millisecond); err != nil && !strings.Contains(err.Error(), "killed") {
		t.Fatalf("close = %v", err)
	}
}

func TestProc_QueuedResultSurvivesGrandchild(t *testing.T) {
	p := startWorkerMode(t, "answer-hold")
	in := []byte{0xAB, 0xCD}
	ticket, err := p.Submit(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := p.Wait(ctx, ticket)
	if err != nil {
		t.Fatalf("Wait = %v; want the queued payload %v", err, in)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("Wait = %v; want %v", out, in)
	}
}

// waitStored polls until the reader has stored ticket's result: delivered,
// with nobody waiting on it yet.
func waitStored(t *testing.T, p *Proc, ticket uint64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		p.mu.Lock()
		stored := false
		if tk := p.tickets[ticket]; tk != nil {
			stored = tk.res != nil
		}
		p.mu.Unlock()
		if stored {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ticket %d's result was never stored", ticket)
		}
		time.Sleep(time.Millisecond)
	}
}

// A result the reader stored before the worker died is finished work. Wait
// returns it, as the in-process handle delivers a result that exists when it
// is poisoned (DECISIONS 2026-09-22); only new work is refused. The reader
// used to delete every ticket on exit, so this Wait returned the poison.
func TestProc_StoredResultSurvivesWorkerDeath(t *testing.T) {
	p := startWorker(t)
	in := []byte{0, 0xB2}
	ticket, err := p.Submit(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := p.Submit(context.Background(), []byte{0, 0xB3})
	if err != nil {
		t.Fatal(err)
	}
	waitStored(t, p, ticket)
	waitStored(t, p, kept)
	if err := syscall.Kill(p.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.readerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the reader did not latch the worker dead within 3s of SIGKILL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := p.Wait(ctx, ticket)
	if err != nil || !bytes.Equal(out, in) {
		t.Fatalf("Wait after the worker died = %x, %v; want the stored %x", out, err, in)
	}
	if _, err := p.Wait(ctx, ticket); !errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("second Wait = %v; want ErrPoisoned: the ticket is spent and the worker is dead", err)
	}
	if _, err := p.Submit(ctx, []byte{0}); !errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("Submit after the worker died = %v; want ErrPoisoned", err)
	}
	if n := len(p.sem); n != 0 {
		t.Fatalf("%d permits held after both results were stored", n)
	}
	// Close drops the result nobody collected.
	if err := p.close(time.Second); err != nil {
		t.Fatalf("close = %v", err)
	}
	p.mu.Lock()
	left := len(p.tickets)
	p.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d tickets still held after Close", left)
	}
}

// Once the worker has exited, a new submission is refused at once: no ticket
// is registered that only a descendant still holding the pipes could answer.
// It used to be written to the dead worker's stdin and to wait for the reader
// to latch, which the drain window delays.
func TestProc_SubmitAfterExitIsRefusedBeforeTheDrainEnds(t *testing.T) {
	p := startWorkerMode(t, "grandchild") // the grandchild holds the protocol write end
	if err := syscall.Kill(p.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.procExited:
	case <-time.After(3 * time.Second):
		t.Fatal("the worker was not reaped within 3s of SIGKILL")
	}
	_, err := p.Submit(context.Background(), []byte{0})
	select {
	case <-p.readerDone:
		t.Fatalf("Submit = %v returned only after the reader latched; want an immediate refusal", err)
	default:
	}
	if !errors.Is(err, gusset.ErrPoisoned) {
		t.Fatalf("Submit after the worker exited = %v; want ErrPoisoned", err)
	}
}

// A past read deadline sticks in bufio and the next readFrame never calls the
// file. Bytes already in the pipe must still be delivered. pw stays open so
// the pipe has no EOF, the same shape as a grandchild holding the write end.
func TestRead_StickyDeadlineKeepsAQueuedFrame(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})
	payload := []byte{0xAB, 0xCD}
	if err := writeFrame(pw, &response{ID: 4, Out: payload}, maxResponseFrame); err != nil {
		t.Fatal(err)
	}
	if err := pr.SetReadDeadline(time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(pr)
	var resp response
	err = readFrame(br, maxResponseFrame, &resp)
	if err == nil {
		if resp.ID != 4 || !bytes.Equal(resp.Out, payload) {
			t.Fatalf("frame = %+v", resp)
		}
		return
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("first read = %v; want a deadline with the frame still queued", err)
	}
	src := &streamSrc{r: pr}
	br = detachBufio(br, src)
	if err := pr.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := readFrame(br, maxResponseFrame, &resp); err != nil {
		t.Fatalf("read after the sticky deadline = %v; the queued frame was dropped", err)
	}
	if resp.ID != 4 || !bytes.Equal(resp.Out, payload) {
		t.Fatalf("frame = id %d out %v", resp.ID, resp.Out)
	}
}

// A deadline already in the past makes poll return before the syscall, so
// ReadFull copies nothing and the error is a plain deadline, not a frameError.
// The frame sitting in the pipe still has to be delivered by the probe.
func TestRead_PastDeadlineWithNothingConsumedStillProbes(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})
	payload := []byte{0xAB, 0xCD}
	if err := writeFrame(pw, &response{ID: 7, Out: payload}, maxResponseFrame); err != nil {
		t.Fatal(err)
	}
	if err := pr.SetReadDeadline(time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	p := &Proc{
		rd:         pr,
		sem:        make(chan struct{}, 1),
		tickets:    make(map[uint64]*ticketState, 1),
		readerDone: make(chan struct{}),
		procExited: make(chan struct{}),
	}
	close(p.procExited)
	waiter := make(chan result, 1)
	p.tickets[7] = &ticketState{waiter: waiter}
	p.sem <- struct{}{}
	go p.read(bufio.NewReader(pr))

	select {
	case <-p.readerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("read did not finish")
	}
	select {
	case got := <-waiter:
		if got.err != nil || !bytes.Equal(got.out, payload) {
			t.Fatalf("waiter = out %x err %v; want %x", got.out, got.err, payload)
		}
	default:
		t.Fatal("waiter received nothing")
	}
}

// writeFramesSteadily writes one echo frame per payload, ids 1..n, gap apart,
// then closes pw. A write error ends it: the reader closed its end, which the
// test reports through the frames it did not receive.
func writeFramesSteadily(pw *os.File, payloads [][]byte, gap time.Duration) {
	defer func() { _ = pw.Close() }()
	for i, payload := range payloads {
		time.Sleep(gap)
		if err := writeFrame(pw, &response{ID: uint64(i + 1), Out: payload}, maxResponseFrame); err != nil {
			return
		}
	}
}

// The reaper's wake has already put the reader into its probe, and frames then
// keep arriving 15ms apart: each inside queuedFrameProbe, all of them together
// well past it. The probe used to be armed once, so every frame after its
// first 50ms got the poison instead.
func TestRead_SteadyFramesAfterTheProbeAreAllDelivered(t *testing.T) {
	const gap = 15 * time.Millisecond
	payloads := make([][]byte, 8)
	for i := range payloads {
		payloads[i] = []byte{0x40, byte(i)}
	}
	if time.Duration(len(payloads))*gap < 2*queuedFrameProbe {
		t.Fatal("the frames must outlast the probe window, or this test proves nothing")
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})
	if err := pr.SetReadDeadline(time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	go writeFramesSteadily(pw, payloads, gap)
	got, dead := readQueuedFrames(t, pr, bufio.NewReader(pr), payloads)
	assertQueuedPayloads(t, payloads, dead, got)
}

// A worker that has exited while a descendant still writes its answers, one
// every 100ms: each inside protocolDrain, all of them together past
// protocolDrain plus the probe. The reaper's window used to be wall-clock from
// the exit, so the later frames were lost.
func TestDrain_SteadyFramesAfterTheWorkerExitsAreAllDelivered(t *testing.T) {
	const gap = 100 * time.Millisecond
	payloads := make([][]byte, 5)
	for i := range payloads {
		payloads[i] = []byte{0x50, byte(i)}
	}
	if time.Duration(len(payloads))*gap < 2*(protocolDrain+queuedFrameProbe) {
		t.Fatal("the frames must outlast both drain windows, or this test proves nothing")
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})
	p := &Proc{
		cmd:        cmd,
		rd:         pr,
		sem:        make(chan struct{}, len(payloads)),
		tickets:    make(map[uint64]*ticketState, len(payloads)),
		readerDone: make(chan struct{}),
		procExited: make(chan struct{}),
	}
	waiters := make([]chan result, len(payloads))
	for i := range payloads {
		ch := make(chan result, 1)
		waiters[i] = ch
		p.tickets[uint64(i+1)] = &ticketState{waiter: ch}
		p.sem <- struct{}{}
	}
	go p.waitProc()
	go p.read(bufio.NewReader(pr))
	select {
	case <-p.procExited:
	case <-time.After(3 * time.Second):
		t.Fatal("the worker was not reaped")
	}
	go writeFramesSteadily(pw, payloads, gap)
	select {
	case <-p.readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("read did not finish")
	}
	p.mu.Lock()
	dead := p.dead
	p.mu.Unlock()
	got := make([]result, len(payloads))
	for i, ch := range waiters {
		select {
		case got[i] = <-ch:
		default:
			t.Fatalf("ticket %d waiter received nothing; reader dead=%v", i+1, dead)
		}
	}
	assertQueuedPayloads(t, payloads, dead, got)
}

// Two complete frames are queued. The first two header bytes have already
// been pulled into bufio — the shape of a reader blocked mid-header — and a
// past deadline is set before read runs, the same wake waitProc uses. The
// second frame must still be delivered. probed is set by that first deadline,
// so a one-shot probe that resumes after the consumed prefix drops it.
//
// Cleanup deletes every ticket before it closes readerDone, and deliver
// deletes a ticket it hands to a waiter. The map is empty either way; the
// waiter is the observation that survives.
func TestRead_TwoQueuedFramesSurviveAMidHeaderDeadline(t *testing.T) {
	payloads := [][]byte{{0x11}, {0x22, 0x33}}
	pr, br := queueFramesPastDeadline(t, payloads, 2)
	got, dead := readQueuedFrames(t, pr, br, payloads)
	assertQueuedPayloads(t, payloads, dead, got)
}

// The whole 4-byte header of frame 1 is already in bufio. The rest of that
// frame and all of frame 2 sit in the pipe behind a past deadline, and the
// write end stays open. A ReadFull that returns the deadline after taking
// the header must not drop the body that follows.
func TestRead_TwoQueuedFramesSurviveAMidBodyDeadline(t *testing.T) {
	payloads := [][]byte{{0x11}, {0x22, 0x33}}
	pr, br := queueFramesPastDeadline(t, payloads, 4)
	got, dead := readQueuedFrames(t, pr, br, payloads)
	assertQueuedPayloads(t, payloads, dead, got)
}

// queueFramesPastDeadline writes two frames, leaves the first prefix bytes
// buffered in a bufio.Reader (via Peek), writes the remainder, and sets a
// deadline in the past. The write end stays open.
func queueFramesPastDeadline(t *testing.T, payloads [][]byte, prefix int) (*os.File, *bufio.Reader) {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})
	var framed bytes.Buffer
	for i, payload := range payloads {
		if err := writeFrame(&framed, &response{ID: uint64(i + 1), Out: payload}, maxResponseFrame); err != nil {
			t.Fatal(err)
		}
	}
	raw := framed.Bytes()
	if prefix <= 0 || prefix >= len(raw) {
		t.Fatalf("prefix %d out of range for %d framed bytes", prefix, len(raw))
	}
	if _, err := pw.Write(raw[:prefix]); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(pr)
	if _, err := br.Peek(prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write(raw[prefix:]); err != nil {
		t.Fatal(err)
	}
	if err := pr.SetReadDeadline(time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	return pr, br
}

// readQueuedFrames runs Proc.read with one buffered waiter per ticket.
// deliver sends the payload and drops the ticket; cleanup sends p.dead only
// while the ticket is still in the map. One slot cannot take both.
func readQueuedFrames(t *testing.T, pr *os.File, br *bufio.Reader, payloads [][]byte) ([]result, error) {
	t.Helper()
	p := &Proc{
		rd:         pr,
		sem:        make(chan struct{}, len(payloads)),
		tickets:    make(map[uint64]*ticketState, len(payloads)),
		readerDone: make(chan struct{}),
		procExited: make(chan struct{}),
	}
	close(p.procExited)
	waiters := make([]chan result, len(payloads))
	for i := range payloads {
		ch := make(chan result, 1)
		waiters[i] = ch
		p.tickets[uint64(i+1)] = &ticketState{waiter: ch}
		p.sem <- struct{}{}
	}
	go p.read(br)

	select {
	case <-p.readerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("read did not finish")
	}
	p.mu.Lock()
	dead := p.dead
	p.mu.Unlock()
	got := make([]result, len(payloads))
	for i, ch := range waiters {
		select {
		case got[i] = <-ch:
		default:
			t.Fatalf("ticket %d waiter received nothing; reader dead=%v", i+1, dead)
		}
	}
	return got, dead
}

func assertQueuedPayloads(t *testing.T, payloads [][]byte, dead error, got []result) {
	t.Helper()
	var mismatch []string
	t.Logf("reader dead=%v", dead)
	for i, payload := range payloads {
		t.Logf("ticket %d out=%x err=%v", i+1, got[i].out, got[i].err)
		if got[i].err != nil || !bytes.Equal(got[i].out, payload) {
			mismatch = append(mismatch, fmt.Sprintf("ticket %d out=%x err=%v; want %x", i+1, got[i].out, got[i].err, payload))
		}
	}
	if len(mismatch) > 0 {
		t.Fatalf("reader dead=%v\n%s", dead, strings.Join(mismatch, "\n"))
	}
}

// Header splits at the first and third byte. The existing mid-header case
// peeks two bytes; a one-byte or three-byte prefix is a different consumed
// slice for the probe to put back. Both frames are already queued behind it.
func TestRead_HeaderSplitAtByte1DeliversBothFrames(t *testing.T) {
	payloads := [][]byte{{0x11}, {0x22, 0x33}}
	pr, br := queueFramesPastDeadline(t, payloads, 1)
	got, dead := readQueuedFrames(t, pr, br, payloads)
	assertQueuedPayloads(t, payloads, dead, got)
}

func TestRead_HeaderSplitAtByte3DeliversBothFrames(t *testing.T) {
	payloads := [][]byte{{0x11}, {0x22, 0x33}}
	pr, br := queueFramesPastDeadline(t, payloads, 3)
	got, dead := readQueuedFrames(t, pr, br, payloads)
	assertQueuedPayloads(t, payloads, dead, got)
}

// Four header bytes plus the first body byte sit in bufio. The rest of that
// body and the second frame are still in the pipe, and the write end stays
// open. The probe has to put the partial body back, not just the header.
func TestRead_BodySplitAfterFirstByteDeliversBothFrames(t *testing.T) {
	payloads := [][]byte{{0x11}, {0x22, 0x33}}
	var one bytes.Buffer
	if err := writeFrame(&one, &response{ID: 1, Out: payloads[0]}, maxResponseFrame); err != nil {
		t.Fatal(err)
	}
	bodyLen := int(binary.BigEndian.Uint32(one.Bytes()[:4]))
	if bodyLen < 2 {
		t.Fatalf("first body is %d bytes; the split has to fall inside it", bodyLen)
	}
	pr, br := queueFramesPastDeadline(t, payloads, 4+1)
	got, dead := readQueuedFrames(t, pr, br, payloads)
	assertQueuedPayloads(t, payloads, dead, got)
}

// Three complete frames are already in the pipe. Nothing has been peeked, and
// the deadline is already past, so the first read is a zero-byte deadline.
// The one probe has to deliver every queued frame, not stop after the first.
func TestRead_ThreeQueuedFramesPastDeadlineNothingPeeked(t *testing.T) {
	payloads := [][]byte{{0x01}, {0x02}, {0x03}}
	var raw bytes.Buffer
	for i, payload := range payloads {
		if err := writeFrame(&raw, &response{ID: uint64(i + 1), Out: payload}, maxResponseFrame); err != nil {
			t.Fatal(err)
		}
	}
	pr, br := queueRawPastDeadline(t, raw.Bytes(), 0)
	got, dead := readQueuedFrames(t, pr, br, payloads)
	assertQueuedPayloads(t, payloads, dead, got)
}

// A declared length over the limit is a plain error. The probe must not retry
// it, and the valid frame queued behind that header must not be delivered.
func TestRead_OverLimitLengthDoesNotDeliverFollowingFrame(t *testing.T) {
	payload := []byte{0xAB}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 0xFFFFFFFF)
	if uint64(0xFFFFFFFF) <= uint64(maxResponseFrame) {
		t.Fatal("declared length is inside the limit")
	}
	var raw bytes.Buffer
	raw.Write(hdr[:])
	if err := writeFrame(&raw, &response{ID: 1, Out: payload}, maxResponseFrame); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []int{0, 1} {
		t.Run(fmt.Sprintf("prefix%d", prefix), func(t *testing.T) {
			pr, br := queueRawPastDeadline(t, raw.Bytes(), prefix)
			got, dead := readQueuedFrames(t, pr, br, [][]byte{payload})
			assertFollowingFrameRefused(t, payload, dead, got[0], "exceeds")
		})
	}
}

// A complete frame of invalid JSON latches the reader dead. The valid frame
// behind it is not a resync point.
func TestRead_InvalidJSONLatchesDeadBeforeTheNextFrame(t *testing.T) {
	payload := []byte{0x5A}
	bad := []byte("[]")
	var raw bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(bad)))
	raw.Write(hdr[:])
	raw.Write(bad)
	if err := writeFrame(&raw, &response{ID: 1, Out: payload}, maxResponseFrame); err != nil {
		t.Fatal(err)
	}
	pr, br := queueRawPastDeadline(t, raw.Bytes(), 0)
	got, dead := readQueuedFrames(t, pr, br, [][]byte{payload})
	assertFollowingFrameRefused(t, payload, dead, got[0], "json")
}

// `{}` is a valid response (id 0, empty out). It must be delivered, including
// when the whole frame is already buffered, not dropped as a short prefix.
// The second frame distinguishes "dropped the prefix" from "parsed it".
func TestRead_EmptyObjectIsDeliveredNotDropped(t *testing.T) {
	second := []byte{0x7E}
	var raw bytes.Buffer
	if err := writeFrame(&raw, &response{}, maxResponseFrame); err != nil {
		t.Fatal(err)
	}
	first := 4 + int(binary.BigEndian.Uint32(raw.Bytes()[:4]))
	if string(raw.Bytes()[4:first]) != "{}" {
		t.Fatalf("empty response frame is %q", raw.Bytes()[4:first])
	}
	if err := writeFrame(&raw, &response{ID: 1, Out: second}, maxResponseFrame); err != nil {
		t.Fatal(err)
	}
	for name, prefix := range map[string]int{"unpeeked": 0, "firstFramePeeked": first} {
		t.Run(name, func(t *testing.T) {
			pr, br := queueRawPastDeadline(t, raw.Bytes(), prefix)
			got, dead := readQueuedIDs(t, pr, br, []uint64{0, 1})
			t.Logf("reader dead=%v", dead)
			if got[0].err != nil || len(got[0].out) != 0 {
				t.Fatalf("empty object out=%x err=%v; want an empty delivery", got[0].out, got[0].err)
			}
			if got[1].err != nil || !bytes.Equal(got[1].out, second) {
				t.Fatalf("following frame out=%x err=%v; want %x", got[1].out, got[1].err, second)
			}
		})
	}
}

// A declared length of zero is not a JSON value and not a dropped prefix.
// The reader latches that error and does not deliver the frame behind it.
func TestRead_ZeroLengthBodyIsFatal(t *testing.T) {
	payload := []byte{0x7E}
	var raw bytes.Buffer
	var hdr [4]byte
	raw.Write(hdr[:])
	if err := writeFrame(&raw, &response{ID: 1, Out: payload}, maxResponseFrame); err != nil {
		t.Fatal(err)
	}
	pr, br := queueRawPastDeadline(t, raw.Bytes(), 0)
	got, dead := readQueuedFrames(t, pr, br, [][]byte{payload})
	assertFollowingFrameRefused(t, payload, dead, got[0], "unexpected end of JSON")
}

// queueRawPastDeadline writes raw, optionally leaving the first prefix bytes
// buffered via Peek, then sets a deadline in the past. The write end stays
// open. prefix 0 buffers nothing.
func queueRawPastDeadline(t *testing.T, raw []byte, prefix int) (*os.File, *bufio.Reader) {
	t.Helper()
	if prefix < 0 || prefix >= len(raw) {
		t.Fatalf("prefix %d out of range for %d bytes", prefix, len(raw))
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})
	if prefix == 0 {
		if _, err := pw.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := pr.SetReadDeadline(time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
		return pr, bufio.NewReader(pr)
	}
	if _, err := pw.Write(raw[:prefix]); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(pr)
	if _, err := br.Peek(prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write(raw[prefix:]); err != nil {
		t.Fatal(err)
	}
	if err := pr.SetReadDeadline(time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	return pr, br
}

// readQueuedIDs is readQueuedFrames for tickets that are not 1..n.
// deliver removes a ticket before readerDone is closed, so the waiter is
// the only record of a payload.
func readQueuedIDs(t *testing.T, pr *os.File, br *bufio.Reader, ids []uint64) ([]result, error) {
	t.Helper()
	p := &Proc{
		rd:         pr,
		sem:        make(chan struct{}, len(ids)),
		tickets:    make(map[uint64]*ticketState, len(ids)),
		readerDone: make(chan struct{}),
		procExited: make(chan struct{}),
	}
	close(p.procExited)
	waiters := make([]chan result, len(ids))
	for i, id := range ids {
		ch := make(chan result, 1)
		waiters[i] = ch
		p.tickets[id] = &ticketState{waiter: ch}
		p.sem <- struct{}{}
	}
	go p.read(br)

	select {
	case <-p.readerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("read did not finish")
	}
	p.mu.Lock()
	dead := p.dead
	p.mu.Unlock()
	got := make([]result, len(ids))
	for i, ch := range waiters {
		select {
		case got[i] = <-ch:
		default:
			t.Fatalf("ticket %d waiter received nothing; reader dead=%v", ids[i], dead)
		}
	}
	return got, dead
}

func assertFollowingFrameRefused(t *testing.T, payload []byte, dead error, got result, want string) {
	t.Helper()
	t.Logf("reader dead=%v waiter out=%x err=%v", dead, got.out, got.err)
	if got.err == nil || bytes.Equal(got.out, payload) {
		t.Fatalf("following frame delivered out=%x err=%v dead=%v", got.out, got.err, dead)
	}
	if dead == nil || !strings.Contains(dead.Error(), want) {
		t.Fatalf("dead = %v; want it to mention %q", dead, want)
	}
}
