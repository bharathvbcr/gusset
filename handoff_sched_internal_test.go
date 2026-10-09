package gusset

import (
	"context"
	"runtime"
	"runtime/metrics"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// schedTransitions is how many goroutine runnable-to-running transitions the
// scheduler has sampled since the process started.
func schedTransitions(t *testing.T) uint64 {
	t.Helper()
	s := []metrics.Sample{{Name: "/sched/latencies:seconds"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindFloat64Histogram {
		t.Skipf("/sched/latencies:seconds unsupported here (kind %v)", s[0].Value.Kind())
	}
	var n uint64
	for _, c := range s[0].Value.Float64Histogram().Counts {
		n += c
	}
	return n
}

// A serial Call, with more than one P, must reach its caller without going
// through the scheduler.
//
// The waiter used to park on its result channel for every call, so the
// reader's send readied it, and the reader yielded on every poll so a readied
// goroutine could run. Each of those is a runnable-to-running transition, and
// each one taken while a P sat idle woke a thread with a system call first:
// 200k serial Calls on an M5 Pro spent 1.5 us each in runtime.wakep at
// ~0.7 us jobs and 5.9 us at ~7 us jobs, a cost that grew with the job.
// Now a lone waiter polls for its result and the reader yields only after a
// delivery that readied someone, so a stream of serial Calls schedules
// nothing at all. The old path showed four to eight sampled transitions per
// call here (16k-30k for 4000 calls on an M5 Pro); this one shows tens.
func TestSerialCall_HandsOffWithoutScheduling(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	h, err := Open(WithPoolSize(4), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()
	in := []byte{0}
	for i := 0; i < 200; i++ { // past the first-call and refresh paths
		if _, err := h.Call(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	const calls = 4000
	before := schedTransitions(t)
	for i := 0; i < calls; i++ {
		if _, err := h.Call(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	got := schedTransitions(t) - before
	// Room for GC workers and for calls a loaded machine stalls past the poll
	// window; none for scheduling on every call.
	if limit := uint64(calls / 8); got > limit {
		t.Fatalf("%d serial Calls made %d sampled scheduler transitions, want <= %d: "+
			"the waiter parked or the reader yielded on the handoff", calls, got, limit)
	}
	t.Logf("%d serial Calls: %d sampled scheduler transitions", calls, got)
}

// A serial caller must leave a P for every other goroutine.
//
// The quiet handoff runs two loops that do not pass through the scheduler:
// the waiter polling its result channel and the reader polling the ring
// without yielding. With two Ps that is every P, and a goroutine readied
// meanwhile waited for one of them to be preempted. Measured with a goroutine
// sleeping 200 us between serial Calls at GOMAXPROCS=2 on an M5 Pro, it woke
// 0.3-3.5 ms late at the median (p99 5-21 ms), against 3-8 us with the waiter
// parking and the reader yielding on every poll, and 32 us in a process
// making no Calls at all. Under a Linux 2-CPU quota (podman --cpus=2) the
// median was ~1 ms and p99 15-19 ms, against 4 us.
func TestSerialCall_LeavesAPForOtherGoroutines(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	var stop atomic.Bool
	done := make(chan error, 1)
	go func() {
		ctx := context.Background()
		in := []byte{0}
		for !stop.Load() {
			if _, err := h.Call(ctx, in); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	const nap = 200 * time.Microsecond
	var late []time.Duration
	time.Sleep(20 * time.Millisecond) // past the first-call and refresh paths
	for deadline := time.Now().Add(2 * time.Second); len(late) < 300 && time.Now().Before(deadline); {
		t0 := time.Now()
		time.Sleep(nap)
		late = append(late, time.Since(t0)-nap)
	}
	stop.Store(true)
	if err := <-done; err != nil {
		t.Fatalf("serial Call: %v", err)
	}

	slices.Sort(late)
	median := late[len(late)/2]
	// 10x the median measured with a P to spare, and a third of the lowest
	// median measured with both Ps held.
	if limit := 100 * time.Microsecond; median > limit {
		t.Fatalf("a goroutine sleeping %v beside serial Calls at GOMAXPROCS=2 woke %v late "+
			"at the median over %d wakes (max %v), want <= %v: the waiter and the reader "+
			"held both Ps", nap, median, len(late), late[len(late)-1], limit)
	}
	t.Logf("%d wakes beside serial Calls: median %v late, max %v", len(late), median, late[len(late)-1])
}

// pollResult polls only for a lone call, and only with a P to spare.
//
// With one P the reader cannot run while the waiter polls, so a polling
// waiter only delays the result it is waiting for: a version that polled
// there took 56 us per serial Call at GOMAXPROCS=1, against 3.5 us parked.
// With two, the polling waiter and the quiet reader held both Ps
// (TestSerialCall_LeavesAPForOtherGoroutines). With several calls in flight a
// poll would hold a core per caller.
func TestPollResult_Gates(t *testing.T) {
	newState := func(procs int, inFlight int) *handleState {
		s := &handleState{sem: make(chan struct{}, 4)}
		s.quietProcs.Store(quietAt(procs))
		for i := 0; i < inFlight; i++ {
			s.sem <- struct{}{}
		}
		return s
	}
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		procs    int
		inFlight int
	}{
		{"one P", 1, 1},
		{"two Ps", 2, 1},
		{"two in flight", quietMinProcs, 2},
	} {
		ch := make(chan callResult, 1)
		start := time.Now()
		if _, ok := newState(tc.procs, tc.inFlight).pollResult(ctx, ch); ok {
			t.Fatalf("%s: pollResult took a result that was never sent", tc.name)
		}
		if d := time.Since(start); d >= waiterSpin {
			t.Fatalf("%s: pollResult polled for %v; it must not poll at all", tc.name, d)
		}
	}

	// Lone, a P to spare: a result sent meanwhile is taken without parking.
	ch := make(chan callResult, 1)
	go func() { ch <- callResult{data: []byte("x")} }()
	s := newState(quietMinProcs, 1)
	var res callResult
	var ok bool
	for deadline := time.Now().Add(5 * time.Second); !ok && time.Now().Before(deadline); {
		res, ok = s.pollResult(ctx, ch)
	}
	if !ok || string(res.data) != "x" {
		t.Fatalf("pollResult = %q, %v; want the sent result", res.data, ok)
	}

	// An ended context stops the poll at once.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	start := time.Now()
	if _, ok := newState(quietMinProcs, 1).pollResult(cctx, make(chan callResult, 1)); ok {
		t.Fatal("pollResult reported a result on a cancelled context")
	}
	if d := time.Since(start); d >= waiterSpin {
		t.Fatalf("pollResult ran %v past a cancelled context", d)
	}
}

// The quiet handoff follows GOMAXPROCS while a handle is open, both ways.
//
// The runtime changes GOMAXPROCS under a running process when a container's
// CPU limit changes, so the gate is re-read on the reader's side every
// gomaxprocsRefresh rather than fixed at Open.
func TestQuietProcs_FollowsGOMAXPROCS(t *testing.T) {
	for procs, want := range map[int]bool{1: false, 2: false, quietMinProcs: true, 8: true} {
		if got := quietAt(procs); got != want {
			t.Errorf("quietAt(%d) = %v, want %v", procs, got, want)
		}
	}

	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state
	if s.quietProcs.Load() {
		t.Fatal("handle opened at GOMAXPROCS=2 runs the quiet handoff")
	}

	ctx := context.Background()
	in := []byte{0}
	callUntil := func(want bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); s.quietProcs.Load() != want; {
			if time.Now().After(deadline) {
				t.Fatalf("quiet handoff still %v at GOMAXPROCS=%d after 5s of Calls",
					!want, runtime.GOMAXPROCS(0))
			}
			if _, err := h.Call(ctx, in); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Millisecond)
		}
	}
	runtime.GOMAXPROCS(quietMinProcs)
	callUntil(true)
	runtime.GOMAXPROCS(2)
	callUntil(false)
}
