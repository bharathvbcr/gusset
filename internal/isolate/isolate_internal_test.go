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
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
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

func TestMain(m *testing.M) {
	if os.Getenv(workerEnv) == "1" {
		os.Exit(workerMain())
	}
	os.Exit(m.Run())
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
	case "grandchild":
		// A grandchild holding every pipe the worker has outlives it.
		gc := exec.Command("sleep", "4")
		gc.Stdout, gc.Stderr = out, os.Stderr
		if err := gc.Start(); err != nil {
			return 1
		}
	}
	time.Sleep(time.Minute)
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
			t.Skip("the first waiter returned past the job's end: nothing left to probe on this host")
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
			if err := p.send(&request{Op: opSubmit, ID: id, In: in}); err != nil {
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
