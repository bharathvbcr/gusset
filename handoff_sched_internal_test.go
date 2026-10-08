package gusset

import (
	"context"
	"runtime"
	"runtime/metrics"
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

// pollResult polls only for a lone call, and only with more than one P.
//
// With one P the reader cannot run while the waiter polls, so a polling
// waiter only delays the result it is waiting for: a version that polled
// there took 56 us per serial Call at GOMAXPROCS=1, against 3.5 us parked.
// With several calls in flight a poll would hold a core per caller.
func TestPollResult_Gates(t *testing.T) {
	newState := func(multiP bool, inFlight int) *handleState {
		s := &handleState{sem: make(chan struct{}, 4)}
		s.multiP.Store(multiP)
		for i := 0; i < inFlight; i++ {
			s.sem <- struct{}{}
		}
		return s
	}
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		multiP   bool
		inFlight int
	}{
		{"one P", false, 1},
		{"two in flight", true, 2},
	} {
		ch := make(chan callResult, 1)
		start := time.Now()
		if _, ok := newState(tc.multiP, tc.inFlight).pollResult(ctx, ch); ok {
			t.Fatalf("%s: pollResult took a result that was never sent", tc.name)
		}
		if d := time.Since(start); d >= waiterSpin {
			t.Fatalf("%s: pollResult polled for %v; it must not poll at all", tc.name, d)
		}
	}

	// Lone, several Ps: a result sent meanwhile is taken without parking.
	ch := make(chan callResult, 1)
	go func() { ch <- callResult{data: []byte("x")} }()
	s := newState(true, 1)
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
	if _, ok := newState(true, 1).pollResult(cctx, make(chan callResult, 1)); ok {
		t.Fatal("pollResult reported a result on a cancelled context")
	}
	if d := time.Since(start); d >= waiterSpin {
		t.Fatalf("pollResult ran %v past a cancelled context", d)
	}
}
