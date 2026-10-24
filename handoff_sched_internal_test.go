package gusset

import (
	"context"
	"runtime"
	"runtime/metrics"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset/internal/hostprobe"
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
	// One sampled transition per call. The code this guards made four to eight
	// per call (16k-30k for 4000), so one per call sits well below it; the old
	// quiet path made tens, but a loaded host or the race detector adds GC
	// workers and stalled calls (510-1385 were seen with a limit of calls/8, on
	// a build that was correct), so a tighter bound measured the host.
	if limit := uint64(calls); got > limit {
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
//
// The check sees the starvation only where it shows. With the process pinned
// to two Linux CPUs (taskset 0,1) the code before the fix passed it 55 times
// out of 55, on a darwin host at load 17.5 it passed 13 of 16, and on GitHub's
// macos-latest runners it woke 25-45 us late, inside the bound. On GitHub's
// Linux runners it fails every run (6.3-14 ms). No tail
// statistic set beside the median separated the two builds on Linux either
// (bench/results/handoff-gomaxprocs/wake-tail/). TestQuietProcs_FollowsGOMAXPROCS
// and TestPollResult_Gates check the gate itself rather than a wake time, and
// failed on that code in every run, idle or at load 13-16.
func TestSerialCall_LeavesAPForOtherGoroutines(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	const nap = 200 * time.Microsecond
	// wakes times a goroutine's sleeps and returns how late each one ended.
	wakes := func() []time.Duration {
		var late []time.Duration
		for deadline := time.Now().Add(2 * time.Second); len(late) < 300 && time.Now().Before(deadline); {
			t0 := time.Now()
			time.Sleep(nap)
			late = append(late, time.Since(t0)-nap)
		}
		slices.Sort(late)
		return late
	}

	// The same sleeps with no Call running, but with the scheduler cycling as
	// it does beside correct Calls (the waiter parks and the reader yields):
	// what this host does to a 200 us nap on its own. A fixed limit measured
	// the host (a median of 127-170 us was seen on a build with nothing wrong,
	// under load), so the claim is relative to this.
	//
	// The baseline must not be an idle process. With every P idle, Go's Linux
	// netpoller rounds a sub-millisecond timer wait up to epoll_wait(1)
	// (runtime/netpoll_epoll.go), so an idle 200 us nap wakes ~860 us late on
	// every Linux host. That baseline skipped this test on every Linux runner,
	// and GUSSET_REQUIRE_PROBES made the skip a failure (Matrix 39270656716).
	// A pure spinner sits on the same floor; a goroutine cycling through the
	// scheduler checks timers on each pass. Measured on GitHub runners
	// (probe/w1-linux-evidence, run 40058658715, 10 runs each): this baseline
	// 2-5 us on ubuntu-latest and ubuntu-24.04-arm and 0.5-17 us on
	// macos-latest; the fixed build beside Calls 1.5-5 us on Linux; the code
	// before the fix 6.3-14 ms on Linux, every run.
	var cycling atomic.Bool
	cycled := make(chan struct{})
	go func() {
		defer close(cycled)
		for !cycling.Load() {
			runtime.Gosched()
		}
	}()
	baseline := wakes()
	cycling.Store(true)
	<-cycled
	base := baseline[len(baseline)/2]
	if base > 100*time.Microsecond {
		hostprobe.Skip(t, "this host wakes a 200 us nap %v late at the median with the scheduler "+
			"cycling and no Calls running: too noisy to show the cost of a handoff "+
			"(TestPollResult_Gates and TestQuietProcs_FollowsGOMAXPROCS check the gate itself)", base)
	}

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

	time.Sleep(20 * time.Millisecond) // past the first-call and refresh paths
	late := wakes()
	stop.Store(true)
	if err := <-done; err != nil {
		t.Fatalf("serial Call: %v", err)
	}

	median := late[len(late)/2]
	// 100 us over the baseline: 10x the median measured with a P to spare, and
	// a third of the lowest median measured with both Ps held.
	if limit := base + 100*time.Microsecond; median > limit {
		t.Fatalf("a goroutine sleeping %v beside serial Calls at GOMAXPROCS=2 woke %v late "+
			"at the median over %d wakes (max %v), want <= %v (%v with no Calls, plus 100 us): "+
			"the waiter and the reader held both Ps", nap, median, len(late), late[len(late)-1], limit, base)
	}
	t.Logf("%d wakes beside serial Calls: median %v late (%v with none), max %v", len(late), median, base, late[len(late)-1])
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
		s := newState(tc.procs, tc.inFlight)
		d := minElapsed(5, func() {
			if _, ok := s.pollResult(ctx, ch); ok {
				t.Fatalf("%s: pollResult took a result that was never sent", tc.name)
			}
		})
		if d >= waiterSpin {
			t.Fatalf("%s: pollResult polled for %v at best of 5; it must not poll at all", tc.name, d)
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
	ended := newState(quietMinProcs, 1)
	d := minElapsed(5, func() {
		if _, ok := ended.pollResult(cctx, make(chan callResult, 1)); ok {
			t.Fatal("pollResult reported a result on a cancelled context")
		}
	})
	if d >= waiterSpin {
		t.Fatalf("pollResult ran %v past a cancelled context at best of 5", d)
	}
}

// minElapsed is the fastest of n runs of f. A poll that does not stop when it
// should takes the whole spin window on every run, so its fastest run still
// shows it; a correct one is only slowed by the host, and one run in n that
// the scheduler leaves alone is enough to show that. A single timed run
// failed under cgocheck2 on a loaded host (258 us past a cancelled context).
func minElapsed(n int, f func()) time.Duration {
	best := time.Duration(1<<63 - 1)
	for i := 0; i < n; i++ {
		start := time.Now()
		f()
		if d := time.Since(start); d < best {
			best = d
		}
	}
	return best
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
