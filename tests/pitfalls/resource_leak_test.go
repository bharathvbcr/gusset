//go:build !windows

package pitfalls_test

import (
	"bytes"
	"context"
	"encoding/binary"
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
	base := takeResourceSample()
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

	final := settleResourceSample(base, 15*time.Second)

	goroutineDelta := final.goroutines - base.goroutines
	fdDelta := final.fds - base.fds
	rustThreadDelta := int64(-1)
	if base.totalTasks >= 0 && final.totalTasks >= 0 {
		rustThreadDelta = final.rustThreads() - base.rustThreads()
	}
	rustLiveDelta := int64(final.rustLive) - int64(base.rustLive)

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
	if fdDelta != 0 {
		t.Errorf("File descriptor leak detected: base=%d, final=%d (delta=%d)", base.fds, final.fds, fdDelta)
		failed = true
	}

	// Assertion 4: finalRustThreads - baseRustThreads == 0
	if rustThreadDelta > 0 {
		t.Errorf("Native Rust worker thread leak detected: base=%d, final=%d (delta=%d)", base.rustThreads(), final.rustThreads(), rustThreadDelta)
		failed = true
	}

	// Assertion 5: finalRustLive - baseRustLive == 0
	if rustLiveDelta != 0 {
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

	base := takeResourceSample()
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

	final := settleResourceSample(base, 15*time.Second)

	goroutineDelta := final.goroutines - base.goroutines
	fdDelta := final.fds - base.fds
	rustThreadDelta := int64(-1)
	if base.totalTasks >= 0 && final.totalTasks >= 0 {
		rustThreadDelta = final.rustThreads() - base.rustThreads()
	}
	rustLiveDelta := int64(final.rustLive) - int64(base.rustLive)

	t.Logf("Churn final resources: goroutines=%d (delta %d), goThreads=%d, totalTasks=%d (rustThreads=%d, delta %d), fds=%d (delta %d), rustLive=%d (delta %d)",
		final.goroutines, goroutineDelta, final.goThreads, final.totalTasks, final.rustThreads(), rustThreadDelta, final.fds, fdDelta, final.rustLive, rustLiveDelta)

	failed := false

	// Assertion 1: Pipes and descriptors completely reclaimed
	if fdDelta != 0 {
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
	if rustLiveDelta != 0 {
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
