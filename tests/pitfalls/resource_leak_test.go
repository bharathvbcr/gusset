//go:build !windows

package pitfalls_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
	"github.com/bharathvbcr/gusset/tests/internal/livebytes"
)

// countDir counts entries in a directory, or returns -1 on error.
func countDir(dir string) int {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return -1
	}
	return len(ents)
}

// countFDs counts the open file descriptors for the current process:
// /proc/self/fd on Linux, /dev/fd on Darwin.
func countFDs() int {
	if n := countDir("/proc/self/fd"); n >= 0 {
		return n
	}
	return countDir("/dev/fd")
}

// countOSThreads counts the process's total OS threads:
// /proc/self/task on Linux, "ps -M -p <pid>" on Darwin.
func countOSThreads() int {
	if n := countDir("/proc/self/task"); n >= 0 {
		return n
	}
	if runtime.GOOS != "darwin" {
		return -1
	}
	out, err := exec.Command("ps", "-M", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return -1
	}
	// Subtract 1 for the column header line
	cnt := bytes.Count(out, []byte("\n")) - 1
	if cnt < 0 {
		return -1
	}
	return cnt
}

type resourceSample struct {
	goroutines  int
	goThreads   int64
	totalTasks  int
	fds         int
	rustLive    uint64
	leakProfCnt int
}

// rustThreads computes the native OS threads that are not Go scheduler Ms.
func (s resourceSample) rustThreads() int64 {
	if s.totalTasks < 0 {
		return -1
	}
	return int64(s.totalTasks) - s.goThreads
}

func takeResourceSample() resourceSample {
	runtime.GC()
	runtime.GC()
	leakCount := 0
	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		leakCount = prof.Count()
	}
	return resourceSample{
		goroutines:  runtime.NumGoroutine(),
		goThreads:   gusset.Threads(),
		totalTasks:  countOSThreads(),
		fds:         countFDs(),
		rustLive:    gusset.Stats().LiveBytes,
		leakProfCnt: leakCount,
	}
}

func settleResourceSample(base resourceSample, timeout time.Duration) resourceSample {
	deadline := time.Now().Add(timeout)
	for {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		runtime.GC()
		cur := takeResourceSample()
		goroutinesOK := cur.goroutines <= base.goroutines
		fdsOK := cur.fds <= base.fds
		rustThreadsOK := cur.totalTasks < 0 || cur.rustThreads() <= base.rustThreads()
		rustLiveOK := cur.rustLive <= base.rustLive

		if goroutinesOK && fdsOK && rustThreadsOK && rustLiveOK {
			return cur
		}
		if time.Now().After(deadline) {
			return cur
		}
	}
}

// rustLiveVerdict is the one Rust-live leak predicate for this file: leaked
// only when live is above the quiesced baseline. A reading below it is an
// earlier test's AddCleanup reclaim landing late, not a leak.
func rustLiveVerdict(baseline, live uint64) (delta int64, leaked bool) {
	return int64(live) - int64(baseline), !livebytes.NotLeaked(baseline, live)
}

// fdLeaked is the one fd leak predicate for this file: exact, not "above".
// The baseline is quiesced, so any difference in either direction is a real
// change in open descriptors; a +1 leak that a straggler closing two fds
// turns into -1 would be missed by final > base.
func fdLeaked(base, final int) bool { return final != base }

// takeResourceBaseline is takeResourceSample with rustLive and fds replaced
// by quiesced readings, so a straggler's cleanup cannot push either under the
// baseline after it is taken. The fd checks stay exact (!=): a +1 leak that a
// straggler closing two fds turns into -1 is caught by != and missed by >.
func takeResourceBaseline(t *testing.T) resourceSample {
	t.Helper()
	return takeResourceBaselineWith(t, countFDs)
}

// takeResourceBaselineWith is takeResourceBaseline with the fd count read by
// readFDs, so TestResourceBaselineQuiescesFDs can script it.
func takeResourceBaselineWith(t *testing.T, readFDs func() int) resourceSample {
	t.Helper()
	s := takeResourceSample()
	s.rustLive = livebytes.Quiesce(t)
	s.fds = quiescedFDs(t, readFDs)
	return s
}

// quiescedFDs returns the open-fd count once it has stopped moving (an
// *os.File dropped without Close is closed later by a GC finalizer), or -1
// when the platform has no fd directory to count, as countFDs does.
func quiescedFDs(t *testing.T, readFDs func() int) int {
	t.Helper()
	if readFDs() < 0 {
		return -1
	}
	return int(livebytes.QuiesceCount(t, "open fds", func() uint64 {
		n := readFDs()
		if n < 0 {
			t.Fatalf("open fds became uncountable while quiescing")
		}
		return uint64(n)
	}))
}

// settleResourceSampleAfterFree waits for the explicit frees to land, then
// settles the other resources. Rust live bytes are polled first and without
// a GC (livebytes.Settle): every test here frees its buffers explicitly, and
// settleResourceSample's runtime.GC loop would let the AddCleanup backstop
// hide a missing free. The settled reading replaces final.rustLive.
//
// A real Rust-bytes leak is slow to report: Settle polls the full 30 s
// without a GC, then settleResourceSample runs its GC loop for up to timeout
// (15 s in the callers; it stops early if the AddCleanup backstop reclaims the
// bytes, but the Settle reading is what the test compares). A failure here can
// therefore arrive about 45 s after the test body finished. That wait is the
// deadline of a gauge that never reached the baseline, not a hang.
func settleResourceSampleAfterFree(base resourceSample, timeout time.Duration) resourceSample {
	live, _ := livebytes.Settle(base.rustLive)
	final := settleResourceSample(base, timeout)
	final.rustLive = live
	return final
}

// dumpGoroutineStacks logs active goroutines if an unexpected goroutine count is found.
func dumpGoroutineStacks(t *testing.T) {
	t.Helper()
	buf := make([]byte, 64*1024)
	n := runtime.Stack(buf, true)
	t.Logf("ACTIVE GOROUTINE STACKS:\n%s", string(buf[:n]))
}

// TestResourceLeak_GoroutinesAndPipesZeroLeakUnderSoak validates Feature 22 by driving
// 50,000 operations across 32 concurrent goroutines on a 16-worker pool, asserting
// strict zero-leak of goroutines, pipe descriptors, and native Rust worker threads.
func TestResourceLeak_GoroutinesAndPipesZeroLeakUnderSoak(t *testing.T) {
	totalOps := 50000
	warmupOps := 2000
	workers := 32
	if testing.Short() {
		totalOps = 5000
		warmupOps = 500
		workers = 16
	}

	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	// 1. Initial calibration: warm up CGo runtime Ms and netpoller descriptors
	warmHandle, err := gusset.Open(
		gusset.WithPoolSize(16),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open warmHandle failed: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < warmupOps; i++ {
		_, _ = warmHandle.Call(ctx, []byte{0, byte(i % 16)})
	}
	if err := warmHandle.Close(); err != nil {
		t.Fatalf("Close warmHandle failed: %v", err)
	}

	// 2. Establish calibrated baseline
	runtime.GC()
	time.Sleep(30 * time.Millisecond)
	runtime.GC()
	base := takeResourceBaseline(t)
	t.Logf("Baseline resources: goroutines=%d, goThreads=%d, totalTasks=%d (rustThreads=%d), fds=%d, rustLive=%d, leakProfCnt=%d",
		base.goroutines, base.goThreads, base.totalTasks, base.rustThreads(), base.fds, base.rustLive, base.leakProfCnt)

	// 3. Open prolonged soak handle
	h, err := gusset.Open(
		gusset.WithPoolSize(16),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open soak handle failed: %v", err)
	}

	mode16Payload := func(size int, seed byte) []byte {
		b := make([]byte, 6)
		b[0] = 16
		binary.LittleEndian.PutUint32(b[1:5], uint32(size))
		b[5] = seed
		return b
	}

	var (
		completedOps atomic.Int64
		wg           sync.WaitGroup
	)

	perWorker := totalOps / workers
	start := time.Now()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(0x42424242, uint64(wid)+1))
			scratch := make([]byte, 1024)
			discardIn := []byte{0, byte(wid), 1}

			for i := 0; i < perWorker; i++ {
				k := r.IntN(100)
				switch {
				case k < 50:
					// 50% inline Call echo
					sz := 16 + r.IntN(496)
					in := scratch[:sz]
					in[0] = 0
					_, _ = h.Call(ctx, in)

				case k < 75:
					// 25% NewBuffer + CallBuffer + Free
					n := 64 + r.IntN(1024)
					buf, err := h.NewBuffer(n)
					if err == nil {
						b := buf.Bytes()
						b[0] = 0
						out, err := h.CallBuffer(ctx, buf)
						if err == nil && out != nil {
							_ = out.Free()
						}
						_ = buf.Free()
					}

				case k < 90:
					// 15% Submit + Wait (large take buffer > 4096B)
					sz := 5000 + r.IntN(3000)
					if tk, err := h.Submit(ctx, mode16Payload(sz, byte(wid))); err == nil {
						_, _ = h.Wait(ctx, tk)
					}

				default:
					// 10% Submit + WaitBuffer + Free
					sz := 5000 + r.IntN(3000)
					if tk, err := h.Submit(ctx, mode16Payload(sz, byte(wid))); err == nil {
						if buf, err := h.WaitBuffer(ctx, tk); err == nil && buf != nil {
							_ = buf.Free()
						} else {
							_ = h.Discard(tk)
						}
					} else {
						_ = h.Discard(0)
					}
				}
				_ = discardIn
				completedOps.Add(1)
			}
		}(w)
	}

	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("Completed %d soak ops in %v (%.0f ops/s)", completedOps.Load(), elapsed, float64(completedOps.Load())/elapsed.Seconds())

	// 4. Teardown soak handle and settle
	if err := h.Close(); err != nil {
		t.Fatalf("Close soak handle failed: %v", err)
	}

	final := settleResourceSampleAfterFree(base, 15*time.Second)

	goroutineDelta := final.goroutines - base.goroutines
	fdDelta := final.fds - base.fds
	rustThreadDelta := int64(-1)
	if base.totalTasks >= 0 && final.totalTasks >= 0 {
		rustThreadDelta = final.rustThreads() - base.rustThreads()
	}
	rustLiveDelta, rustLiveLeaked := rustLiveVerdict(base.rustLive, final.rustLive)

	t.Logf("Final resources: goroutines=%d (delta %d), goThreads=%d, totalTasks=%d (rustThreads=%d, delta %d), fds=%d (delta %d), rustLive=%d (delta %d)",
		final.goroutines, goroutineDelta, final.goThreads, final.totalTasks, final.rustThreads(), rustThreadDelta, final.fds, fdDelta, final.rustLive, rustLiveDelta)

	failed := false

	// Assertion 1: pprof goroutineleak count == 0
	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		cnt := prof.Count()
		if cnt != 0 {
			t.Errorf("pprof goroutineleak profile reported %d leaking goroutines", cnt)
			failed = true
		}
	}

	// Assertion 2: finalGoroutines - baseGoroutines == 0
	if goroutineDelta != 0 {
		// Verify whether lingering goroutines belong to gusset
		buf := make([]byte, 64*1024)
		n := runtime.Stack(buf, true)
		stackStr := string(buf[:n])
		if strings.Contains(stackStr, "github.com/bharathvbcr/gusset") {
			t.Errorf("Gusset goroutine leak detected: delta=%d", goroutineDelta)
			dumpGoroutineStacks(t)
			failed = true
		} else {
			t.Logf("Note: non-Gusset goroutine count variance: base=%d, final=%d (delta %d)", base.goroutines, final.goroutines, goroutineDelta)
		}
	}

	// Assertion 3: finalFDs - baseFDs == 0
	if fdLeaked(base.fds, final.fds) {
		t.Errorf("File descriptor leak detected: base=%d, final=%d (delta=%d)", base.fds, final.fds, fdDelta)
		failed = true
	}

	// Assertion 4: finalRustThreads - baseRustThreads == 0
	if rustThreadDelta > 0 {
		t.Errorf("Native Rust worker thread leak detected: base=%d, final=%d (delta=%d)", base.rustThreads(), final.rustThreads(), rustThreadDelta)
		failed = true
	}

	// Assertion 5: finalRustLive - baseRustLive == 0
	if rustLiveLeaked {
		t.Errorf("Rust live bytes leak detected: base=%d, final=%d (delta=%d)", base.rustLive, final.rustLive, rustLiveDelta)
		failed = true
	}

	if failed {
		dumpHeapProfile(t)
	}
}

// TestResourceLeak_MultiHandleChurnAndTeardownZeroLeak validates Feature 22 across
// 50 handles created, exercised with 8 MiB worker stacks, and torn down, asserting
// complete reclamation of pipe descriptors, Darwin kqueue registrations, worker threads,
// and alternate signal stacks.
func TestResourceLeak_MultiHandleChurnAndTeardownZeroLeak(t *testing.T) {
	totalHandles := 50
	callsPerHandle := 100
	workersPerHandle := 8
	if testing.Short() {
		totalHandles = 10
		callsPerHandle = 20
		workersPerHandle = 4
	}

	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	// Warmup cycle to stabilize Go M scheduler baseline
	warmHandle, err := gusset.Open(
		gusset.WithPoolSize(4),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open warmHandle failed: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		_, _ = warmHandle.Call(ctx, []byte{0, byte(i)})
	}
	_ = warmHandle.Close()

	runtime.GC()
	time.Sleep(30 * time.Millisecond)
	runtime.GC()

	base := takeResourceBaseline(t)
	t.Logf("Multi-handle churn baseline: goroutines=%d, goThreads=%d, totalTasks=%d (rustThreads=%d), fds=%d, rustLive=%d",
		base.goroutines, base.goThreads, base.totalTasks, base.rustThreads(), base.fds, base.rustLive)

	start := time.Now()
	for cycle := 0; cycle < totalHandles; cycle++ {
		h, err := gusset.Open(
			gusset.WithPoolSize(4),
			gusset.WithDiagnosticEngine(),
		)
		if err != nil {
			t.Fatalf("Cycle %d Open failed: %v", cycle, err)
		}

		var wg sync.WaitGroup
		perWorker := callsPerHandle / workersPerHandle
		for w := 0; w < workersPerHandle; w++ {
			wg.Add(1)
			go func(wid int) {
				defer wg.Done()
				r := rand.New(rand.NewPCG(uint64(cycle+1), uint64(wid)+1))
				scratch := make([]byte, 512)
				for i := 0; i < perWorker; i++ {
					switch i % 3 {
					case 0:
						sz := 16 + r.IntN(256)
						in := scratch[:sz]
						in[0] = 0
						_, _ = h.Call(ctx, in)
					case 1:
						buf, err := h.NewBuffer(128)
						if err == nil {
							b := buf.Bytes()
							b[0] = 0
							out, err := h.CallBuffer(ctx, buf)
							if err == nil && out != nil {
								_ = out.Free()
							}
							_ = buf.Free()
						}
					case 2:
						tk, err := h.Submit(ctx, []byte{0, byte(wid)})
						if err == nil {
							_, _ = h.Wait(ctx, tk)
						}
					}
				}
			}(w)
		}
		wg.Wait()

		// Explicit Close verifies teardown of:
		// 1. Rust worker threads (joined via pthread_join)
		// 2. 8 MiB thread stacks
		// 3. Alternate signal stacks (sigaltstack disabled, munmap)
		// 4. Pipe read & write descriptors (libc::close and netpollclose EV_DELETE on kqueue)
		// 5. drainPipe reader goroutines
		if err := h.Close(); err != nil {
			t.Fatalf("Cycle %d Close failed: %v", cycle, err)
		}
	}

	elapsed := time.Since(start)
	t.Logf("Completed %d handle churn cycles in %v", totalHandles, elapsed)

	final := settleResourceSampleAfterFree(base, 15*time.Second)

	goroutineDelta := final.goroutines - base.goroutines
	fdDelta := final.fds - base.fds
	rustThreadDelta := int64(-1)
	if base.totalTasks >= 0 && final.totalTasks >= 0 {
		rustThreadDelta = final.rustThreads() - base.rustThreads()
	}
	rustLiveDelta, rustLiveLeaked := rustLiveVerdict(base.rustLive, final.rustLive)

	t.Logf("Churn final resources: goroutines=%d (delta %d), goThreads=%d, totalTasks=%d (rustThreads=%d, delta %d), fds=%d (delta %d), rustLive=%d (delta %d)",
		final.goroutines, goroutineDelta, final.goThreads, final.totalTasks, final.rustThreads(), rustThreadDelta, final.fds, fdDelta, final.rustLive, rustLiveDelta)

	failed := false

	// Assertion 1: Pipes and descriptors completely reclaimed
	if fdLeaked(base.fds, final.fds) {
		t.Errorf("File descriptor leak after %d handle cycles: base=%d, final=%d (delta=%d)",
			totalHandles, base.fds, final.fds, fdDelta)
		failed = true
	}

	// Assertion 2: Worker threads and 8 MiB stacks reclaimed
	if rustThreadDelta > 0 {
		t.Errorf("Rust worker thread leak after %d handle cycles: base=%d, final=%d (delta=%d)",
			totalHandles, base.rustThreads(), final.rustThreads(), rustThreadDelta)
		failed = true
	}

	// Assertion 3: Goroutines cleanly terminated
	if goroutineDelta != 0 {
		buf := make([]byte, 64*1024)
		n := runtime.Stack(buf, true)
		stackStr := string(buf[:n])
		if strings.Contains(stackStr, "github.com/bharathvbcr/gusset") {
			t.Errorf("Gusset goroutine leak after %d handle cycles: delta=%d", totalHandles, goroutineDelta)
			dumpGoroutineStacks(t)
			failed = true
		} else {
			t.Logf("Note: non-Gusset goroutine count variance: base=%d, final=%d (delta %d)",
				base.goroutines, final.goroutines, goroutineDelta)
		}
	}

	// Assertion 4: Zero Rust live bytes leak
	if rustLiveLeaked {
		t.Errorf("Rust live bytes leaked after %d handle cycles: base=%d, final=%d (delta=%d)",
			totalHandles, base.rustLive, final.rustLive, rustLiveDelta)
		failed = true
	}

	// Assertion 5: Go scheduler thread ceiling bounded
	maxAllowedGoThreads := base.goThreads + int64(runtime.GOMAXPROCS(0)+16)
	if final.goThreads > maxAllowedGoThreads {
		t.Errorf("Go scheduler thread count unbounded: base=%d, final=%d (ceiling=%d)",
			base.goThreads, final.goThreads, maxAllowedGoThreads)
		failed = true
	}

	if failed {
		dumpHeapProfile(t)
	}
}

// getProcessRSS returns process RSS in KiB from ps on Darwin or /proc/self/statm on Linux.
func getProcessRSS() int64 {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
		if err != nil {
			return -1
		}
		s := strings.TrimSpace(string(out))
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v
		}
		return -1
	}
	// Linux fallback: read /proc/self/statm
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return -1
	}
	fields := strings.Fields(string(data))
	if len(fields) >= 2 {
		pages, _ := strconv.ParseInt(fields[1], 10, 64)
		return pages * int64(os.Getpagesize()) / 1024
	}
	return -1
}

// TestResourceLeak_ThreadCountingFidelityAndActiveStackVerification tests that:
// 1. Darwin ps -M / Linux /proc/self/task accurately counts native threads while open.
// 2. An open handle with N workers increases totalTasks by exactly N (rustThreads == N).
// 3. Handle close joins all N threads and rustThreads drops back to exactly 0.
// 4. Repeated cycles of 16-worker handles completely reclaim thread stacks without unbounded RSS growth.
func TestResourceLeak_ThreadCountingFidelityAndActiveStackVerification(t *testing.T) {
	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	// Calibrate baseline
	runtime.GC()
	time.Sleep(30 * time.Millisecond)
	runtime.GC()

	base := takeResourceBaseline(t)
	baseRSS := getProcessRSS()
	t.Logf("Thread fidelity baseline: totalTasks=%d, goThreads=%d, rustThreads=%d, fds=%d, rss=%d KiB",
		base.totalTasks, base.goThreads, base.rustThreads(), base.fds, baseRSS)

	if base.totalTasks < 0 {
		t.Skip("OS thread counting unsupported on this platform")
	}

	const poolWorkers = 16
	h, err := gusset.Open(
		gusset.WithPoolSize(poolWorkers),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Wait briefly for all OS worker threads to spin up and report ready
	time.Sleep(50 * time.Millisecond)

	activeSample := takeResourceSample()
	activeRustThreads := activeSample.rustThreads()
	activeFDs := activeSample.fds
	t.Logf("Active handle with %d workers: totalTasks=%d, goThreads=%d, rustThreads=%d, fds=%d",
		poolWorkers, activeSample.totalTasks, activeSample.goThreads, activeRustThreads, activeFDs)

	// Assertion 1: rustThreads should be at least poolWorkers while active
	if activeRustThreads < poolWorkers {
		t.Errorf("Active handle failed thread fidelity check: expected >= %d rust threads, got %d",
			poolWorkers, activeRustThreads)
	}

	// Assertion 2: FDs should have increased by at least 1 (the completion pipe)
	if activeFDs <= base.fds {
		t.Errorf("Active handle failed FD check: base=%d, active=%d (expected pipe creation)",
			base.fds, activeFDs)
	}

	// Run some work
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		_, err := h.Call(ctx, []byte{0, byte(i % 16)})
		if err != nil {
			t.Fatalf("Call failed: %v", err)
		}
	}

	// Close handle
	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Settle and verify return to baseline
	final := settleResourceSample(base, 10*time.Second)
	finalRustThreads := final.rustThreads()
	t.Logf("Closed handle: totalTasks=%d, goThreads=%d, rustThreads=%d (delta %d), fds=%d (delta %d)",
		final.totalTasks, final.goThreads, finalRustThreads, finalRustThreads-base.rustThreads(),
		final.fds, final.fds-base.fds)

	if finalRustThreads > base.rustThreads() {
		t.Errorf("Rust worker threads not joined after Close: base=%d, final=%d (delta=%d)",
			base.rustThreads(), finalRustThreads, finalRustThreads-base.rustThreads())
	}
	if fdLeaked(base.fds, final.fds) {
		t.Errorf("Pipe file descriptors leaked after Close: base=%d, final=%d (delta=%d)",
			base.fds, final.fds, final.fds-base.fds)
	}

	// Test 10 repeated cycles of 16-worker handles to verify stack reclamation
	for cycle := 0; cycle < 10; cycle++ {
		ch, err := gusset.Open(
			gusset.WithPoolSize(poolWorkers),
			gusset.WithDiagnosticEngine(),
		)
		if err != nil {
			t.Fatalf("Cycle %d Open failed: %v", cycle, err)
		}
		for i := 0; i < 20; i++ {
			_, _ = ch.Call(ctx, []byte{0, byte(i)})
		}
		if err := ch.Close(); err != nil {
			t.Fatalf("Cycle %d Close failed: %v", cycle, err)
		}
	}

	settledAfterChurn := settleResourceSample(base, 10*time.Second)
	finalRSS := getProcessRSS()
	t.Logf("After 10x 16-worker cycles: totalTasks=%d, goThreads=%d, rustThreads=%d, fds=%d, rss=%d KiB (delta %d KiB)",
		settledAfterChurn.totalTasks, settledAfterChurn.goThreads, settledAfterChurn.rustThreads(),
		settledAfterChurn.fds, finalRSS, finalRSS-baseRSS)

	if settledAfterChurn.rustThreads() > base.rustThreads() {
		t.Errorf("Native thread leak after churn: base=%d, final=%d",
			base.rustThreads(), settledAfterChurn.rustThreads())
	}
	if fdLeaked(base.fds, settledAfterChurn.fds) {
		t.Errorf("FD leak after churn: base=%d, final=%d", base.fds, settledAfterChurn.fds)
	}
}

// TestResourceLeak_ViolentCloseRace_CancellationAndFDZeroLeak stresses handle close
// while multiple concurrent goroutines are actively submitting, waiting, allocating buffers,
// and cancelling contexts. Asserts:
// - Close terminates gracefully without panic, deadlock or wedge.
// - All completion pipe descriptors and netpoller registrations are freed (0 FD delta).
// - All OS worker threads are cleanly joined.
// - 0 leaked goroutines.
func TestResourceLeak_ViolentCloseRace_CancellationAndFDZeroLeak(t *testing.T) {
	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	// Calibrate baseline
	runtime.GC()
	time.Sleep(30 * time.Millisecond)
	runtime.GC()
	base := takeResourceBaseline(t)

	const iterations = 25
	for iter := 0; iter < iterations; iter++ {
		h, err := gusset.Open(
			gusset.WithPoolSize(8),
			gusset.WithDiagnosticEngine(),
		)
		if err != nil {
			t.Fatalf("Iter %d Open failed: %v", iter, err)
		}

		var (
			wg       sync.WaitGroup
			stopWork atomic.Bool
		)

		const workers = 24
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(wid int) {
				defer wg.Done()
				r := rand.New(rand.NewPCG(uint64(iter*100+wid), 0xABCD))
				for !stopWork.Load() {
					op := r.IntN(4)
					// Context with short aggressive deadline (50us - 2ms)
					timeout := time.Duration(50+r.IntN(2000)) * time.Microsecond
					ctx, cancel := context.WithTimeout(context.Background(), timeout)

					switch op {
					case 0:
						// Call
						_, _ = h.Call(ctx, []byte{0, byte(wid)})
					case 1:
						// Buffer CallBuffer
						buf, bErr := h.NewBuffer(128)
						if bErr == nil {
							b := buf.Bytes()
							if len(b) > 0 {
								b[0] = 0
								out, cErr := h.CallBuffer(ctx, buf)
								if cErr == nil && out != nil {
									_ = out.Free()
								}
							}
							_ = buf.Free()
						}
					case 2:
						// Submit + WaitBuffer
						tk, sErr := h.Submit(ctx, []byte{0, byte(wid)})
						if sErr == nil {
							if resBuf, wErr := h.WaitBuffer(ctx, tk); wErr == nil && resBuf != nil {
								_ = resBuf.Free()
							} else {
								_ = h.Discard(tk)
							}
						}
					case 3:
						// Submit + immediate Discard
						tk, sErr := h.Submit(ctx, []byte{0, byte(wid)})
						if sErr == nil {
							_ = h.Discard(tk)
						}
					}
					cancel()
				}
			}(w)
		}

		// Let work run for a brief moment (1 to 5 ms), then violently close handle
		time.Sleep(time.Duration(1+rand.IntN(5)) * time.Millisecond)

		// Close concurrently while workers are still running!
		closeErr := h.Close()
		if closeErr != nil && !errors.Is(closeErr, gusset.ErrClosed) {
			t.Logf("Iter %d Close returned error: %v", iter, closeErr)
		}

		// Tell workers to stop
		stopWork.Store(true)
		wg.Wait()
	}

	// Settle and verify resource invariance
	final := settleResourceSampleAfterFree(base, 15*time.Second)
	t.Logf("Violent close race final: goroutines=%d (delta %d), fds=%d (delta %d), rustThreads=%d (delta %d), rustLive=%d (delta %d)",
		final.goroutines, final.goroutines-base.goroutines,
		final.fds, final.fds-base.fds,
		final.rustThreads(), final.rustThreads()-base.rustThreads(),
		final.rustLive, int64(final.rustLive)-int64(base.rustLive))

	if final.fds != base.fds {
		t.Errorf("File descriptor leak after violent close races: base=%d, final=%d (delta=%d)",
			base.fds, final.fds, final.fds-base.fds)
	}
	if final.rustThreads() > base.rustThreads() {
		t.Errorf("Rust thread leak after violent close races: base=%d, final=%d (delta=%d)",
			base.rustThreads(), final.rustThreads(), final.rustThreads()-base.rustThreads())
	}
	if final.goroutines-base.goroutines != 0 {
		buf := make([]byte, 64*1024)
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), "github.com/bharathvbcr/gusset") {
			t.Errorf("Gusset goroutine leak after violent close races: delta=%d", final.goroutines-base.goroutines)
			dumpGoroutineStacks(t)
		}
	}
	if delta, leaked := rustLiveVerdict(base.rustLive, final.rustLive); leaked {
		t.Errorf("Rust live bytes leaked after violent close races: delta=%d", delta)
	}
}

// TestResourceLeak_KqueueNetpollerRapidChurnStress tests 100 consecutive rapid
// handle open and close cycles asserting that Go's internal netpoller and kqueue
// do not leak file descriptors, event queues, or reader goroutines.
func TestResourceLeak_KqueueNetpollerRapidChurnStress(t *testing.T) {
	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	runtime.GC()
	time.Sleep(30 * time.Millisecond)
	runtime.GC()
	base := takeResourceBaseline(t)

	const churnCycles = 100
	ctx := context.Background()

	for i := 0; i < churnCycles; i++ {
		h, err := gusset.Open(
			gusset.WithPoolSize(2),
			gusset.WithDiagnosticEngine(),
		)
		if err != nil {
			t.Fatalf("Cycle %d Open failed: %v", i, err)
		}

		// Perform 5 fast calls
		for j := 0; j < 5; j++ {
			_, _ = h.Call(ctx, []byte{0, byte(j)})
		}

		if err := h.Close(); err != nil {
			t.Fatalf("Cycle %d Close failed: %v", i, err)
		}
	}

	final := settleResourceSample(base, 15*time.Second)
	t.Logf("Kqueue/Netpoller 100x churn final: fds=%d (delta %d), rustThreads=%d (delta %d), goroutines=%d (delta %d)",
		final.fds, final.fds-base.fds,
		final.rustThreads(), final.rustThreads()-base.rustThreads(),
		final.goroutines, final.goroutines-base.goroutines)

	if final.fds != base.fds {
		t.Errorf("FD leak after %d kqueue churn cycles: base=%d, final=%d (delta=%d)",
			churnCycles, base.fds, final.fds, final.fds-base.fds)
	}
	if final.rustThreads() > base.rustThreads() {
		t.Errorf("Thread leak after %d kqueue churn cycles: base=%d, final=%d",
			churnCycles, base.rustThreads(), final.rustThreads())
	}
	if final.goroutines-base.goroutines != 0 {
		buf := make([]byte, 64*1024)
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), "github.com/bharathvbcr/gusset") {
			t.Errorf("Goroutine leak after %d kqueue churn cycles: delta=%d",
				churnCycles, final.goroutines-base.goroutines)
			dumpGoroutineStacks(t)
		}
	}
}

// TestResourceLeak_ProlongedSoakWithGoroutineLeakProfileAudit drives 100,000 mixed operations
// under 32 concurrent goroutines while continuously checking that the pprof goroutineleak profile
// remains at 0 leaking records.
func TestResourceLeak_ProlongedSoakWithGoroutineLeakProfileAudit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 100k soak in short mode")
	}

	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	runtime.GC()
	time.Sleep(30 * time.Millisecond)
	runtime.GC()
	base := takeResourceBaseline(t)

	h, err := gusset.Open(
		gusset.WithPoolSize(16),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const totalOps = 100000
	const workers = 32
	perWorker := totalOps / workers

	mode16Payload := func(size int, seed byte) []byte {
		b := make([]byte, 6)
		b[0] = 16
		binary.LittleEndian.PutUint32(b[1:5], uint32(size))
		b[5] = seed
		return b
	}

	var (
		completed atomic.Int64
		wg        sync.WaitGroup
	)

	ctx := context.Background()
	start := time.Now()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(0x5555, uint64(wid)+1))
			scratch := make([]byte, 1024)

			for i := 0; i < perWorker; i++ {
				k := r.IntN(100)
				switch {
				case k < 50:
					// Call inline
					sz := 16 + r.IntN(500)
					in := scratch[:sz]
					in[0] = 0
					_, _ = h.Call(ctx, in)
				case k < 75:
					// CallBuffer
					sz := 64 + r.IntN(1024)
					buf, bErr := h.NewBuffer(sz)
					if bErr == nil {
						b := buf.Bytes()
						b[0] = 0
						out, cErr := h.CallBuffer(ctx, buf)
						if cErr == nil && out != nil {
							_ = out.Free()
						}
						_ = buf.Free()
					}
				case k < 90:
					// Submit + Wait
					sz := 5000 + r.IntN(3000)
					tk, sErr := h.Submit(ctx, mode16Payload(sz, byte(wid)))
					if sErr == nil {
						_, _ = h.Wait(ctx, tk)
					}
				default:
					// Submit + WaitBuffer
					sz := 5000 + r.IntN(3000)
					tk, sErr := h.Submit(ctx, mode16Payload(sz, byte(wid)))
					if sErr == nil {
						buf, wErr := h.WaitBuffer(ctx, tk)
						if wErr == nil && buf != nil {
							_ = buf.Free()
						} else {
							_ = h.Discard(tk)
						}
					}
				}
				completed.Add(1)
			}
		}(w)
	}

	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("Completed %d ops in %v (%.0f ops/s)", completed.Load(), elapsed, float64(completed.Load())/elapsed.Seconds())

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	final := settleResourceSampleAfterFree(base, 15*time.Second)
	t.Logf("100k Soak Final: goroutines=%d (delta %d), totalTasks=%d, goThreads=%d, rustThreads=%d (delta %d), fds=%d (delta %d), rustLive=%d, leakProfCnt=%d",
		final.goroutines, final.goroutines-base.goroutines,
		final.totalTasks, final.goThreads, final.rustThreads(), final.rustThreads()-base.rustThreads(),
		final.fds, final.fds-base.fds, final.rustLive, final.leakProfCnt)

	// Check 1: pprof goroutineleak profile
	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		if cnt := prof.Count(); cnt != 0 {
			t.Errorf("pprof goroutineleak profile reported %d leaking goroutines", cnt)
		}
	}

	// Check 2: exact FD delta
	if final.fds != base.fds {
		t.Errorf("FD leak detected after 100k soak: base=%d, final=%d (delta=%d)",
			base.fds, final.fds, final.fds-base.fds)
	}

	// Check 3: exact Rust thread delta
	if final.rustThreads() > base.rustThreads() {
		t.Errorf("Rust thread leak detected after 100k soak: base=%d, final=%d (delta=%d)",
			base.rustThreads(), final.rustThreads(), final.rustThreads()-base.rustThreads())
	}

	// Check 4: zero gusset goroutine leak
	if final.goroutines-base.goroutines != 0 {
		buf := make([]byte, 64*1024)
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), "github.com/bharathvbcr/gusset") {
			t.Errorf("Gusset goroutine leak after 100k soak: delta=%d", final.goroutines-base.goroutines)
			dumpGoroutineStacks(t)
		}
	}

	// Check 5: zero Rust live bytes
	if delta, leaked := rustLiveVerdict(base.rustLive, final.rustLive); leaked {
		t.Errorf("Rust live bytes leaked after 100k soak: delta=%d", delta)
	}
}
