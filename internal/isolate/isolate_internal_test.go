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

func TestMain(m *testing.M) {
	if os.Getenv(workerEnv) == "1" {
		os.Exit(workerMain())
	}
	os.Exit(m.Run())
}

func workerMain() int {
	h, err := gusset.Open(gusset.WithPoolSize(workerPool), gusset.WithDiagnosticEngine())
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker open:", err)
		return 1
	}
	serveErr := Serve(os.Stdin, os.Stdout, faulty{h}, workerPool)
	closeErr := h.Close()
	if err := errors.Join(serveErr, closeErr); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		return 1
	}
	return 0
}

// faulty kills its own process on a fault byte and passes everything else to
// the handle.
type faulty struct{ *gusset.Handle }

func (f faulty) Submit(ctx context.Context, in any) (uint64, error) {
	if b, ok := in.([]byte); ok && len(b) > 0 {
		switch b[0] {
		case faultKill:
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		case faultAbort:
			_ = syscall.Kill(os.Getpid(), syscall.SIGABRT)
		}
	}
	return f.Handle.Submit(ctx, in)
}

func startWorker(t *testing.T) *Proc {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), workerEnv+"=1")
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := Start(ctx, cmd)
	if err != nil {
		t.Fatalf("Start: %v\nworker stderr:\n%s", err, stderr.String())
	}
	t.Cleanup(func() {
		_ = p.Close()
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
		ticket, err := c.Submit(bg(t), []byte{9, 30}) // 300 ms
		if err != nil {
			t.Fatal(err)
		}
		first := make(chan error, 1)
		go func() { _, err := c.Wait(bg(t), ticket); first <- err }()
		time.Sleep(50 * time.Millisecond) // let the first waiter register
		if _, err := c.Wait(bg(t), ticket); !errors.Is(err, gusset.ErrTicketBusy) {
			t.Fatalf("second Wait = %v; want ErrTicketBusy", err)
		}
		if err := <-first; err != nil {
			t.Fatalf("first Wait = %v; want the result", err)
		}
	})
}

// The detach contract (DECISIONS 2026-09-20): the caller returns at its
// deadline while a non-cooperative engine keeps running. The bound is
// relative to the engine, as that row requires.
func TestParity_DeadlineBoundsTheCaller(t *testing.T) {
	const engineRuns = time.Second
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
		signal string
	}{
		{"SIGKILL", faultKill, "signal: killed"},
		// Go's own handler takes SIGABRT, prints the crash dump and exits 2,
		// so an abort() from Rust code reaches the host as this status too.
		{"SIGABRT", faultAbort, "exit status 2"},
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
			if !strings.Contains(err.Error(), fault.signal) {
				t.Fatalf("worker exit not named: %v", err)
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
	p := startWorker(t)
	if _, err := p.Submit(bg(t), []byte{9, 255}); err != nil { // 2.55 s, non-cooperative
		t.Fatal(err)
	}
	// Let the job reach the engine. Closing first cancels it before it
	// starts, and the worker then exits inside the budget.
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	err := p.close(100 * time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("close past budget = %v; want the kill reported", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
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
