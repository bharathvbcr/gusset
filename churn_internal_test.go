package gusset

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"runtime/metrics"
	"strconv"
	"testing"
	"time"
)

// churnSample is the process-level footprint a handle can leak into.
type churnSample struct {
	goroutines int
	goThreads  int64  // /sched/threads/total:threads (Go's Ms)
	osTasks    int    // countOSThreads: Go's threads and Rust's workers; -1 unmeasured
	fds        int    // /proc/self/fd or /dev/fd; -1 unmeasured
	heapLive   uint64 // /memory/classes/heap/objects:bytes after GC
	rustLive   uint64 // Stats().LiveBytes
}

func countDir(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return -1
	}
	return len(ents)
}

// countOSThreads counts the process's OS threads: /proc/self/task on Linux,
// one `ps -M` line per thread on darwin, which has no /proc. Reading only
// /proc made every thread comparison -1 against -1 on macOS, a pass that
// measured nothing.
func countOSThreads(t *testing.T) int {
	t.Helper()
	if n := countDir(t, "/proc/self/task"); n >= 0 {
		return n
	}
	if runtime.GOOS != "darwin" {
		return -1
	}
	out, err := exec.Command("ps", "-M", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return -1
	}
	return bytes.Count(out, []byte("\n")) - 1 // minus the header
}

func countFDs(t *testing.T) int {
	t.Helper()
	if n := countDir(t, "/proc/self/fd"); n >= 0 {
		return n
	}
	return countDir(t, "/dev/fd")
}

func takeChurnSample(t *testing.T, gc bool) churnSample {
	t.Helper()
	if gc {
		runtime.GC()
		runtime.GC()
	}
	m := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(m)
	return churnSample{
		goroutines: runtime.NumGoroutine(),
		goThreads:  Threads(),
		osTasks:    countOSThreads(t),
		fds:        countFDs(t),
		heapLive:   m[0].Value.Uint64(),
		rustLive:   Stats().LiveBytes,
	}
}

// rustThreads is the OS threads that are not Go Ms: Rust's workers.
func (a churnSample) rustThreads() int64 { return int64(a.osTasks) - a.goThreads }

func (a churnSample) max(b churnSample) churnSample {
	a.goroutines = max(a.goroutines, b.goroutines)
	a.goThreads = max(a.goThreads, b.goThreads)
	a.osTasks = max(a.osTasks, b.osTasks)
	a.fds = max(a.fds, b.fds)
	a.heapLive = max(a.heapLive, b.heapLive)
	a.rustLive = max(a.rustLive, b.rustLive)
	return a
}

// churnOne opens a handle, makes a few calls of each egress shape, and either
// closes it or drops it for the AddCleanup backstop. It returns the state of a
// dropped handle (nil when closed), which does not keep the Handle reachable:
// the cleanup's argument is the state.
func churnOne(t *testing.T, i int, drop bool) *handleState {
	h, err := Open(WithPoolSize(1+i%2), WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open #%d: %v", i, err)
	}
	ctx := context.Background()
	small := []byte{0, byte(i), byte(i >> 8)} // inline completion record
	if out, err := h.Call(ctx, small); err != nil || !bytes.Equal(out, small) {
		t.Fatalf("Call #%d: %v", i, err)
	}
	mid := bytes.Repeat([]byte{byte(i)}, 300) // bare ticket, take copy
	mid[0] = 0
	if out, err := h.Call(ctx, mid); err != nil || !bytes.Equal(out, mid) {
		t.Fatalf("Call(300) #%d: %v", i, err)
	}
	in, err := h.NewBuffer(8 << 10) // take-buffer egress, zero copy
	if err != nil {
		t.Fatalf("NewBuffer #%d: %v", i, err)
	}
	in.Bytes()[0] = 0
	out, err := h.CallBuffer(ctx, in)
	if err != nil {
		t.Fatalf("CallBuffer #%d: %v", i, err)
	}
	if i%3 == 0 {
		// Leave one result buffer to its own backstop as well.
		_ = in.Free()
		out.owner = nil
	} else {
		_ = out.Free()
		_ = in.Free()
	}
	if drop {
		s := h.state
		return s
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close #%d: %v", i, err)
	}
	return nil
}

// Open and close handles in a loop — some dropped to the AddCleanup backstop
// instead of closed — and require goroutines, OS threads, descriptors, the Go
// heap and Rust live bytes to come back to a warm baseline.
//
// Every handle is a pipe, a drain goroutine and a pool of OS threads; a
// per-handle leak of any of them is invisible to the single-handle soak.
// GUSSET_CHURN sets the handle count (default 2000; the audit ran 20000).
func TestChurn_HandlesLeaveNothingBehind(t *testing.T) {
	if testing.Short() {
		t.Skip("churn skipped in -short mode")
	}
	total := 2000
	if v := os.Getenv("GUSSET_CHURN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("GUSSET_CHURN=%q: want a positive handle count", v)
		}
		total = n
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)

	// Warm up: Go keeps idle Ms, so a cold baseline would read their creation
	// as a leak.
	for i := 0; i < 200; i++ {
		churnOne(t, i, false)
	}
	base := takeChurnSample(t, true)
	peak := base

	// Dropped handles are collected in batches: one GC finds the whole batch
	// unreachable at once. GUSSET_CHURN_BATCH sets its size (default 64).
	churnBatch := 64
	if v := os.Getenv("GUSSET_CHURN_BATCH"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("GUSSET_CHURN_BATCH=%q: want a positive count", v)
		}
		churnBatch = n
	}
	var dropped []*handleState
	drops := 0
	settle := func() {
		runtime.GC()
		runtime.GC()
		for _, s := range dropped {
			select {
			case <-s.closeDone:
			case <-time.After(30 * time.Second):
				t.Fatal("a dropped handle was not closed by its AddCleanup backstop within 30 s")
			}
		}
		dropped = dropped[:0]
	}
	start := time.Now()
	for i := 0; i < total; i++ {
		drop := i%8 == 7
		if s := churnOne(t, 200+i, drop); s != nil {
			dropped = append(dropped, s)
			drops++
		}
		if len(dropped) >= churnBatch {
			settle()
		}
		if i%250 == 0 {
			peak = peak.max(takeChurnSample(t, false))
		}
	}
	settle()
	elapsed := time.Since(start)

	t.Logf("%d handles (%d dropped to the backstop) in %v", total, drops, elapsed.Round(time.Millisecond))
	// Go's own Ms are not a per-handle leak but a high-water mark: every
	// backstop close is its own goroutine in a cgo call joining workers, one
	// M each, and Go never retires an idle M. A batch of dropped handles
	// collected by one GC therefore raises the count by up to the batch size
	// (measured over 20000 handles: batch 1 -> +1, 8 -> +7, 64 -> +36,
	// 256 -> +48), and it must not grow with the number of handles.
	requireChurnBaseline(t, base, peak, int64(churnBatch)+8)
}

// requireChurnBaseline waits up to 30 s for the process to come back to base,
// then fails on every resource still above it. goThreadSlack bounds Go's own
// Ms, which are a high-water mark rather than a per-handle leak. A resource
// the platform cannot count skips the test after the other checks have run,
// rather than passing without having measured it.
func requireChurnBaseline(t *testing.T, base, peak churnSample, goThreadSlack int64) {
	t.Helper()
	// Converge: cleanups for dropped result buffers and the last drain
	// goroutines finish asynchronously.
	var after churnSample
	deadline := time.Now().Add(30 * time.Second)
	for {
		after = takeChurnSample(t, true)
		ok := after.goroutines <= base.goroutines && after.fds <= base.fds &&
			after.rustLive <= base.rustLive && after.rustThreads() <= base.rustThreads()
		if ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("baseline:   %+v", base)
	t.Logf("peak:       %+v", peak)
	t.Logf("after:      %+v", after)

	if after.goroutines > base.goroutines {
		t.Errorf("goroutines %d -> %d", base.goroutines, after.goroutines)
	}
	if after.fds > base.fds {
		t.Errorf("open descriptors %d -> %d", base.fds, after.fds)
	}
	if after.rustLive > base.rustLive {
		t.Errorf("Rust live bytes %d -> %d", base.rustLive, after.rustLive)
	}
	// Threads that are not Go's are Rust workers, and Close joins every one.
	if after.rustThreads() > base.rustThreads() {
		t.Errorf("non-Go OS threads (Rust workers) %d -> %d", base.rustThreads(), after.rustThreads())
	}
	if after.goThreads > base.goThreads+goThreadSlack {
		t.Errorf("Go threads %d -> %d, past the bound +%d", base.goThreads, after.goThreads, goThreadSlack)
	}
	if after.heapLive > base.heapLive+4<<20 {
		t.Errorf("Go heap %d -> %d bytes", base.heapLive, after.heapLive)
	}
	if base.osTasks < 0 || after.osTasks < 0 || base.fds < 0 || after.fds < 0 {
		t.Skipf("OS threads or descriptors cannot be counted on %s; those checks did not run", runtime.GOOS)
	}
}

// poisonOne runs the documented recovery path once (DECISIONS.md, 2026-09-30:
// poison is per handle; recovery is Close and Open): a fresh handle answers, a
// slow job is in flight when a sibling's panic poisons the handle, new work is
// refused, and Close drains the in-flight job — collected by Wait on even
// cycles, left for Close on odd ones. The panicking worker, the in-flight
// result (a Rust take buffer when large) and the handle's pipe and drain
// goroutine are what a recovery that leaks would leave behind.
func poisonOne(t *testing.T, i int) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open #%d: %v", i, err)
	}
	ctx := context.Background()
	if out, err := h.Call(ctx, []byte{0, byte(i)}); err != nil || !bytes.Equal(out, []byte{0, byte(i)}) {
		t.Fatalf("Call on a fresh handle #%d: %v", i, err)
	}
	// Mode 9: sleep 20 ms, then echo slow[2:]. Over 4 KiB on alternate pairs
	// of cycles, so both the inline and the take-buffer result are in flight.
	n := 300
	if i%4 >= 2 {
		n = 5000
	}
	slow := bytes.Repeat([]byte{byte(i)}, n)
	slow[0], slow[1], slow[2] = 9, 2, 0
	var in any = slow
	if n > 4096 {
		// A []byte input is capped at 4 KiB; the large one goes as a Buffer.
		buf, err := h.NewBuffer(n)
		if err != nil {
			t.Fatalf("NewBuffer #%d: %v", i, err)
		}
		copy(buf.Bytes(), slow)
		defer func() { _ = buf.Free() }()
		in = buf
	}
	inflight, err := h.Submit(ctx, in)
	if err != nil {
		t.Fatalf("Submit #%d: %v", i, err)
	}
	if _, err := h.Call(ctx, []byte{1}); !errors.Is(err, ErrPanic) {
		t.Fatalf("mode 1 #%d: want ErrPanic, got %v", i, err)
	}
	if _, err := h.Call(ctx, []byte{0}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("Call after the panic #%d: want ErrPoisoned, got %v", i, err)
	}
	if _, err := h.NewBuffer(64); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("NewBuffer after the panic #%d: want ErrPoisoned, got %v", i, err)
	}
	if i%2 == 0 {
		// Poison refuses new work, not finished work (DECISIONS.md, 2026-09-22).
		out, err := h.Wait(ctx, inflight)
		if err != nil || !bytes.Equal(out, slow[2:]) {
			t.Fatalf("Wait on the in-flight job #%d: %d bytes, %v", i, len(out), err)
		}
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close of the poisoned handle #%d: %v", i, err)
	}
}

// A poisoned handle closed and replaced by a fresh one, over and over, must
// bring goroutines, OS threads, descriptors, the Go heap and Rust live bytes
// back to a warm baseline: recovery from ErrPoisoned leaks nothing. The engine
// here is the diagnostic one, which needs no registration; an adopter whose
// panic tore its engine's captured state also runs clear_engine_handlers and
// registers again in Rust before the Open, which no Go entry point does.
// GUSSET_POISON_CHURN sets the cycle count (default 200).
func TestChurn_PoisonedHandlesRecreateLeavingNothingBehind(t *testing.T) {
	if testing.Short() {
		t.Skip("poison churn skipped in -short mode")
	}
	total := 200
	if v := os.Getenv("GUSSET_POISON_CHURN"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("GUSSET_POISON_CHURN=%q: want a positive cycle count", v)
		}
		total = n
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)

	// Warm up: Go keeps idle Ms, so a cold baseline would read their creation
	// as a leak.
	for i := 0; i < 20; i++ {
		poisonOne(t, i)
	}
	base := takeChurnSample(t, true)
	peak := base
	start := time.Now()
	for i := 0; i < total; i++ {
		poisonOne(t, 20+i)
		if i%25 == 0 {
			peak = peak.max(takeChurnSample(t, false))
		}
	}
	t.Logf("%d poisoned handles closed and reopened in %v", total, time.Since(start).Round(time.Millisecond))

	// Recovery closes on the caller's goroutine, so there is no backstop
	// burst: Go's Ms may only grow by the runtime's own slack.
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open after recovery: %v", err)
	}
	if out, err := h.Call(context.Background(), []byte{0, 7}); err != nil || !bytes.Equal(out, []byte{0, 7}) {
		t.Fatalf("Call on the recovered handle: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close of the recovered handle: %v", err)
	}
	requireChurnBaseline(t, base, peak, 8)
}
