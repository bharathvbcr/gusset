// Package livebytes is the one place the test suites compare the process-wide
// Stats().LiveBytes gauge.
//
// LiveBytes counts every Rust-owned buffer in the process, so a test that
// snapshots it races any earlier test whose runtime.AddCleanup has not run
// yet. Two failure modes follow, and the helpers here close both:
//
//   - snapshot taken too early: a straggler's cleanup frees bytes after the
//     snapshot, the gauge falls under it, and an equality check reports a
//     leak that is really somebody else's reclaim. Quiesce takes the baseline
//     only once the gauge has stopped moving.
//   - verdict taken too early: the buffers the test freed are still being
//     returned (a worker finishing, a drain reclaiming a result). Settle polls
//     until the gauge reaches the baseline. SettleGC also forces a GC on every
//     poll, for a test whose buffers are abandoned to runtime.AddCleanup on
//     purpose; a test about an explicit free must use Settle, because a GC
//     would let the AddCleanup backstop hide a missing free.
//
// Quiescence is a judgement about a gauge that other goroutines move, so it has
// a limit: Quiesce accepts quiesceStable equal readings in a row, each taken a
// GC and 10 ms after the last, which is about 20 ms of stillness. A straggler
// whose cleanup is delayed longer than that (a finalizer queue backed up behind
// a slow one, a worker parked for longer than the window) can still land after
// the baseline is taken and show up as a drop. A drop is never a failure
// (NotLeaked accepts it), so this can only make a leak check more lenient by
// that straggler's bytes, never report a false leak. Raise quiesceStable to
// widen the window at 10 ms plus a GC per step, on every Quiesce.
//
// "Not leaked" is live <= baseline. A reading under the quiesced baseline is
// a reclaim, not a leak; a reading that stays above it after settling is.
//
// QuiesceCount takes the same quiesced baseline for another process-wide
// count a leak check compares, such as open file descriptors: an *os.File an
// earlier test dropped without Close is closed by a GC finalizer, so that
// count can also fall after a snapshot.
package livebytes

import (
	"runtime"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

const (
	pollEvery     = 10 * time.Millisecond
	settleTimeout = 30 * time.Second
	// quiesceStable is how many equal readings in a row end a quiesce. Two
	// matched across a single 10 ms gap, which a straggler's burst of cleanups
	// can sit still through.
	quiesceStable = 3
)

// gauge is the process-wide counter plus the clock that polls it. Production
// wiring is process; the unit test substitutes a scripted one.
type gauge struct {
	read    func() uint64
	collect func()
	every   time.Duration
	timeout time.Duration
}

var process = gauge{
	read:    func() uint64 { return gusset.Stats().LiveBytes },
	collect: runtime.GC,
	every:   pollEvery,
	timeout: settleTimeout,
}

// NotLeaked is the leak predicate: a drop under the baseline is an earlier
// cleanup, only a reading above it is a leak.
func NotLeaked(baseline, live uint64) bool {
	return live <= baseline
}

// Quiesce GCs until quiesceStable consecutive LiveBytes readings match and
// returns that reading, to be used as the baseline for NotLeaked and Settle. It fails the
// test if the gauge has not stopped moving after 30 seconds.
func Quiesce(tb testing.TB) uint64 {
	tb.Helper()
	live, ok := process.quiesce()
	if !ok {
		tb.Fatalf("LiveBytes did not quiesce: last=%d", live)
	}
	return live
}

// QuiesceCount is Quiesce for another process-wide count, read by read: it
// GCs until quiesceStable consecutive readings match and returns that reading. It fails
// the test, naming the count, if it has not stopped moving after 30 seconds.
func QuiesceCount(tb testing.TB, name string, read func() uint64) uint64 {
	tb.Helper()
	g := process
	g.read = read
	n, ok := g.quiesce()
	if !ok {
		tb.Fatalf("%s did not quiesce: last=%d", name, n)
	}
	return n
}

// Settle polls, without forcing a GC, until LiveBytes is at or under baseline
// or 30 seconds pass. It returns the last reading and whether NotLeaked
// accepted it; false is a leak above the baseline.
func Settle(baseline uint64) (uint64, bool) {
	return process.settle(baseline, false)
}

// SettleGC is Settle with a runtime.GC on every poll, so a buffer abandoned to
// its runtime.AddCleanup backstop is reclaimed while the test waits.
func SettleGC(baseline uint64) (uint64, bool) {
	return process.settle(baseline, true)
}

// WaitUntil polls LiveBytes until ok accepts a reading or 30 seconds pass,
// and returns the last reading with whether ok accepted it. It does not force
// a GC; it is for tests that wait on a specific gauge level (a held result
// counted, a discarded one released) rather than on the leak predicate.
func WaitUntil(ok func(live uint64) bool) (uint64, bool) {
	return process.waitUntil(ok, false)
}

func (g gauge) quiesce() (uint64, bool) {
	deadline := time.Now().Add(g.timeout)
	prev := g.read()
	stable := 1
	for {
		g.collect()
		time.Sleep(g.every)
		live := g.read()
		if live == prev {
			stable++
			if stable >= quiesceStable {
				return live, true
			}
			continue
		}
		if time.Now().After(deadline) {
			return live, false
		}
		prev, stable = live, 1
	}
}

func (g gauge) settle(baseline uint64, collect bool) (uint64, bool) {
	return g.waitUntil(func(live uint64) bool { return NotLeaked(baseline, live) }, collect)
}

func (g gauge) waitUntil(ok func(uint64) bool, collect bool) (uint64, bool) {
	deadline := time.Now().Add(g.timeout)
	for {
		live := g.read()
		if ok(live) {
			return live, true
		}
		if time.Now().After(deadline) {
			return live, false
		}
		if collect {
			g.collect()
		}
		time.Sleep(g.every)
	}
}
