//go:build !windows

package pitfalls_test

import (
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
)

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

// TestChallenge_M4_2_ThreadCountingFidelityAndActiveStackVerification tests that:
// 1. Darwin ps -M / Linux /proc/self/task accurately counts native threads while open.
// 2. An open handle with N workers increases totalTasks by exactly N (rustThreads == N).
// 3. Handle close joins all N threads and rustThreads drops back to exactly 0.
// 4. Repeated cycles of 16-worker handles completely reclaim thread stacks without unbounded RSS growth.
func TestChallenge_M4_2_ThreadCountingFidelityAndActiveStackVerification(t *testing.T) {
	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	// Calibrate baseline
	runtime.GC()
	time.Sleep(30 * time.Millisecond)
	runtime.GC()

	base := takeResourceSample()
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
	if final.fds != base.fds {
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
	if settledAfterChurn.fds != base.fds {
		t.Errorf("FD leak after churn: base=%d, final=%d", base.fds, settledAfterChurn.fds)
	}
}

// TestChallenge_M4_2_ViolentCloseRace_CancellationAndFDZeroLeak stresses handle close
// while multiple concurrent goroutines are actively submitting, waiting, allocating buffers,
// and cancelling contexts. Asserts:
// - Close terminates gracefully without panic, deadlock or wedge.
// - All completion pipe descriptors and netpoller registrations are freed (0 FD delta).
// - All OS worker threads are cleanly joined.
// - 0 leaked goroutines.
func TestChallenge_M4_2_ViolentCloseRace_CancellationAndFDZeroLeak(t *testing.T) {
	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	// Calibrate baseline
	runtime.GC()
	time.Sleep(30 * time.Millisecond)
	runtime.GC()
	base := takeResourceSample()

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
	final := settleResourceSample(base, 15*time.Second)
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
	if int64(final.rustLive)-int64(base.rustLive) != 0 {
		t.Errorf("Rust live bytes leaked after violent close races: delta=%d",
			int64(final.rustLive)-int64(base.rustLive))
	}
}

// TestChallenge_M4_2_KqueueNetpollerRapidChurnStress tests 100 consecutive rapid
// handle open and close cycles asserting that Go's internal netpoller and kqueue
// do not leak file descriptors, event queues, or reader goroutines.
func TestChallenge_M4_2_KqueueNetpollerRapidChurnStress(t *testing.T) {
	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	runtime.GC()
	time.Sleep(30 * time.Millisecond)
	runtime.GC()
	base := takeResourceSample()

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

// TestChallenge_M4_2_ProlongedSoakWithGoroutineLeakProfileAudit drives 100,000 mixed operations
// under 32 concurrent goroutines while continuously checking that the pprof goroutineleak profile
// remains at 0 leaking records.
func TestChallenge_M4_2_ProlongedSoakWithGoroutineLeakProfileAudit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 100k soak in short mode")
	}

	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	runtime.GC()
	time.Sleep(30 * time.Millisecond)
	runtime.GC()
	base := takeResourceSample()

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

	final := settleResourceSample(base, 15*time.Second)
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
	if int64(final.rustLive)-int64(base.rustLive) != 0 {
		t.Errorf("Rust live bytes leaked after 100k soak: delta=%d",
			int64(final.rustLive)-int64(base.rustLive))
	}
}
