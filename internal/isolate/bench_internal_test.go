package isolate

import (
	"bytes"
	"context"
	"log/slog"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// The isolate benchmarks run through the same worker process the tests use:
// TestMain turns this test binary into the worker when workerEnv is set, and
// supHarness.factory builds that command. A benchmark cannot call newHarness,
// which takes a *testing.T, so it builds the harness value directly. factory
// reads none of the fields a *testing.T would fill.
//
// Every figure here includes the pipe round trip to a separate process. That is
// the cost the isolate package exists to add, and it is two to three orders of
// magnitude above the in-process Call that BenchmarkCallNoopBare measures, so
// the two sets are never compared with each other, only each with its own base.

var benchEcho = []byte{0, 0x42}

// BenchmarkProcCallEcho is one Proc.Call round trip: frame out over the pipe,
// the worker's diagnostic echo (mode 0), frame back.
func BenchmarkProcCallEcho(b *testing.B) {
	h := &supHarness{}
	cmd, err := h.factory()
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := Start(ctx, cmd)
	if err != nil {
		b.Fatalf("Start: %v\nworker stderr:\n%s", err, h.stderr[0].String())
	}
	defer func() { _ = p.close(5 * time.Second) }()

	call := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := p.Call(call, benchEcho)
		if err != nil || !bytes.Equal(out, benchEcho) {
			b.Fatalf("Call = %v, %v", out, err)
		}
	}
}

// startBenchSupervisor starts a supervisor over the test worker with a restart
// budget no run can spend: one restart per iteration, and the window outlasts
// the run. The first Call has returned when it does, so a generation is live.
func startBenchSupervisor(b *testing.B, restarts int) *supHarness {
	b.Helper()
	h := &supHarness{}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Supervise(ctx, h.factory, Policy{MaxRestarts: restarts, Window: time.Hour})
	if err != nil {
		cancel()
		b.Fatal(err)
	}
	h.s = s
	b.Cleanup(func() {
		_ = s.Close()
		cancel()
	})
	call, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	if out, err := s.Call(call, benchEcho); err != nil || !bytes.Equal(out, benchEcho) {
		b.Fatalf("first Call = %v, %v", out, err)
	}
	return h
}

// BenchmarkSupervisorCallEcho is one Supervisor.Call round trip on a live
// generation: Proc.Call plus the supervisor's generation lookup and ticket
// bookkeeping.
func BenchmarkSupervisorCallEcho(b *testing.B) {
	h := startBenchSupervisor(b, 1)
	call := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := h.s.Call(call, benchEcho)
		if err != nil || !bytes.Equal(out, benchEcho) {
			b.Fatalf("Call = %v, %v", out, err)
		}
	}
}

// BenchmarkSupervisorRestart is the cost of one restart: SIGKILL the live
// worker, then Call until the replacement answers. The timed region holds the
// kill, the supervisor noticing the death, the new process starting, its
// handshake, and the Call that rode it out.
//
// A Call that lands on the dying generation before the supervisor has retired
// it returns an error and is retried at once; those are reported as
// failed-calls/op rather than hidden, and the run fails if one iteration cannot
// get an answer in 30 s or if one death started more than one process.
func BenchmarkSupervisorRestart(b *testing.B) {
	// Each retirement logs a warning through slog's default logger. Go prints
	// a benchmark's name before running it, so a log line landing in between
	// splits the result row and the recorder cannot parse it.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	defer slog.SetDefault(prev)
	h := startBenchSupervisor(b, b.N+8)
	b.ReportAllocs()
	b.ResetTimer()
	failed := 0
	for i := 0; i < b.N; i++ {
		old := pidOf(h.s)
		if old == 0 {
			b.Fatal("no live worker to kill")
		}
		before := h.starts.Load()
		if err := syscall.Kill(old, syscall.SIGKILL); err != nil {
			b.Fatal(err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			out, err := h.s.Call(ctx, benchEcho)
			cancel()
			if err == nil && bytes.Equal(out, benchEcho) && pidOf(h.s) != old {
				break
			}
			if isBudget(err) {
				b.Fatalf("restart budget spent at iteration %d: %v", i, err)
			}
			failed++
			if time.Now().After(deadline) {
				b.Fatalf("no answer after killing %d: %v", old, err)
			}
			runtime.Gosched()
		}
		if got := h.starts.Load(); got != before+1 {
			b.Fatalf("starts %d -> %d after one kill; want exactly one replacement", before, got)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(failed)/float64(b.N), "failed-calls/op")
}
