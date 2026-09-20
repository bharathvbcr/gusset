package pitfalls_test

// Assertions the CI matrix named but never made.
//
// AGENTS.md listed three jobs whose stated "must contain" was absent from the
// repository: the `soak` job's goroutineleak profile assertion, the `memlimit`
// job's 200 MiB allocation and 1% check, and the `cgocheck2` job's retained-pointer
// test that must fail. Each job ran and went green while checking none of it.
//
// A job that cannot fail is worse than an absent one, because it retires the rule
// it claims to enforce. These are the missing assertions.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
	"github.com/bharathvbcr/gusset/internal/ffi"
	"github.com/bharathvbcr/gusset/tests/cgoprobe"
)

// cgocheckViolationEnv makes the test binary re-enter itself as the violating
// child rather than the asserting parent.
const cgocheckViolationEnv = "GUSSET_TEST_CGOCHECK_VIOLATE"

// TestGate_CgoCheck2IsArmedWhenRequested proves the cgocheck2 job is actually
// checking something.
//
// The job's entire value is contingent on cgocheck2 being on. If the GOEXPERIMENT
// name were misspelled, or a future toolchain renamed it, the job would still run
// the suite, still go green, and enforce nothing — and R6 and R16 name this job as
// their only enforcer.
//
// So the assertion is conditional on the environment, and it is never "pass":
//   - with cgocheck2 on, the violation must be caught;
//   - with it off, the violation must go through, which confirms the test is
//     detecting the flag rather than something else.
func TestGate_CgoCheck2IsArmedWhenRequested(t *testing.T) {
	if os.Getenv(cgocheckViolationEnv) == "1" {
		// Child process: commit the violation and exit 0 if it was allowed.
		cgoprobe.StoreGoPointerIntoCMemory()
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^TestGate_CgoCheck2IsArmedWhenRequested$")
	cmd.Env = append(os.Environ(), cgocheckViolationEnv+"=1")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()

	cgocheckOn := strings.Contains(os.Getenv("GOEXPERIMENT"), "cgocheck2")

	if cgocheckOn {
		if err == nil {
			t.Fatalf("GOEXPERIMENT=cgocheck2 is set but the runtime accepted a Go pointer "+
				"to an unpinned Go pointer; R6/R16 have no enforcer.\nchild output:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "Go pointer stored into non-Go memory") {
			t.Fatalf("child failed, but not with the cgocheck violation this gate tests for.\n"+
				"error: %v\nchild output:\n%s", err, out.String())
		}
		t.Logf("cgocheck2 armed: violation caught as expected")
		return
	}

	// Without the experiment the violation must be *allowed*. If it were caught
	// anyway, this test would report success under cgocheck2 for a reason that has
	// nothing to do with cgocheck2.
	if err != nil {
		t.Fatalf("without GOEXPERIMENT=cgocheck2 the violation should pass unnoticed, "+
			"so this test cannot distinguish an armed gate from an unarmed one.\n"+
			"error: %v\nchild output:\n%s", err, out.String())
	}
	t.Logf("cgocheck2 not set: violation was allowed, as expected. " +
		"Run with GOEXPERIMENT=cgocheck2 to exercise the enforcing half.")
}

// TestSoak_GoroutineLeakProfileIsEmptyAfterDrain is the assertion the soak job
// named and did not make.
//
// Counting goroutines before and after — which is what the existing churn test
// does — cannot tell a leak from a goroutine that simply has not been scheduled
// yet, so it has to tolerate slack and therefore misses small persistent leaks.
// Go 1.27's goroutineleak profile reports goroutines the GC proved unreachable and
// permanently blocked, which is the actual property: a caller parked forever on a
// completion that will never arrive.
func TestSoak_GoroutineLeakProfileIsEmptyAfterDrain(t *testing.T) {
	prof := pprof.Lookup("goroutineleak")
	if prof == nil {
		t.Fatalf("the goroutineleak profile is unavailable on %s; the soak job's leak "+
			"assertion cannot run and must not be reported as passing", runtime.Version())
	}

	baseline := prof.Count()

	const (
		poolSize = 4
		callers  = 400
	)
	h, err := gusset.Open(gusset.WithPoolSize(poolSize), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Mix the paths that park a caller: plain calls, calls that time out
			// mid-job, and cancelled calls. Each has its own wake-up path, and a
			// leak in any one of them strands the caller forever.
			var ctx context.Context
			var cancel context.CancelFunc
			switch i % 3 {
			case 0:
				ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
			case 1:
				ctx, cancel = context.WithTimeout(context.Background(), 5*time.Millisecond)
			default:
				ctx, cancel = context.WithCancel(context.Background())
				go func() { time.Sleep(2 * time.Millisecond); cancel() }()
			}
			defer cancel()

			// Mode 5 sleeps in 10 ms units, so the short deadlines land mid-job.
			_, _ = h.Call(ctx, []byte{5, 3})
		}(i)
	}
	wg.Wait()

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// The profile is produced by a leak-detection GC cycle, so the goroutines must
	// first become unreachable. Two cycles: the first drops the last references,
	// the second observes them.
	runtime.GC()
	runtime.GC()

	leaked := prof.Count() - baseline
	if leaked > 0 {
		var buf bytes.Buffer
		_ = prof.WriteTo(&buf, 1)
		t.Fatalf("%d goroutine(s) leaked across %d calls on a %d-worker pool:\n%s",
			leaked, callers, poolSize, buf.String())
	}
	t.Logf("goroutineleak profile clean after %d calls (baseline %d)", callers, baseline)
}

// TestMemLimit_LiveStatsTrackLargeRustAllocation is the memlimit job's missing
// assertion: a large Rust allocation must be visible to Go within 1%.
//
// AdviseMemoryLimit subtracts Rust's live bytes from the process budget and hands
// the remainder to debug.SetMemoryLimit. If the accounting under-reports, Go is
// told it may use memory that Rust already holds, and the process is killed by the
// OS instead of back-pressured by the GC. Accounting accuracy is the whole
// mechanism, and nothing measured it.
func TestMemLimit_LiveStatsTrackLargeRustAllocation(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const (
		chunk     = 20 * 1024 * 1024 // 20 MiB
		chunks    = 10               // 200 MiB total
		tolerance = 0.01             // 1%
	)

	before := gusset.Stats()

	bufs := make([]*gusset.Buffer, 0, chunks)
	for i := 0; i < chunks; i++ {
		b, err := h.NewBuffer(chunk)
		if err != nil {
			t.Fatalf("NewBuffer(%d) #%d failed: %v", chunk, i, err)
		}
		bufs = append(bufs, b)
	}

	during := gusset.Stats()
	grew := int64(during.LiveBytes) - int64(before.LiveBytes)
	want := int64(chunk) * chunks

	delta := float64(grew-want) / float64(want)
	if delta < 0 {
		delta = -delta
	}
	if delta > tolerance {
		t.Fatalf("Rust live bytes grew by %d for %d bytes allocated (%.2f%% off, tolerance %.0f%%); "+
			"AdviseMemoryLimit would hand Go a budget that Rust already holds",
			grew, want, delta*100, tolerance*100)
	}
	t.Logf("live accounting within %.4f%% across a %d MiB Rust allocation", delta*100, want>>20)

	// AdviseMemoryLimit must subtract that allocation from the Go budget.
	const total = int64(512 * 1024 * 1024)
	prev := gusset.AdviseMemoryLimit(total)
	t.Cleanup(func() { gusset.AdviseMemoryLimit(prev) })

	applied := gusset.AdviseMemoryLimit(total)
	if applied > total-want {
		t.Fatalf("AdviseMemoryLimit(%d) left Go a limit of %d, which does not account for "+
			"the %d bytes Rust holds", total, applied, want)
	}

	// Peak must never retreat below live: it is a high-water mark.
	if during.PeakBytes < during.LiveBytes {
		t.Fatalf("peak %d is below live %d; the high-water mark is not monotonic",
			during.PeakBytes, during.LiveBytes)
	}

	for i, b := range bufs {
		if err := b.Free(); err != nil {
			t.Fatalf("Free #%d failed: %v", i, err)
		}
	}

	after := gusset.Stats()
	if int64(after.LiveBytes) > int64(before.LiveBytes)+int64(chunk) {
		t.Fatalf("after freeing %d MiB, live bytes are %d against a %d baseline: "+
			"the allocator is not symmetric (I1/R4)", want>>20, after.LiveBytes, before.LiveBytes)
	}

	if after.AllocCount <= before.AllocCount {
		t.Fatalf("alloc count did not advance across %d allocations", chunks)
	}
}

// TestGate_AbiCrossCheckDetectsHeaderDrift guards the guard.
//
// gusset.h is hand-maintained, so Go's init() compares Rust's self-reported layout
// against what cgo compiled. That comparison is only meaningful if the two sources
// are genuinely independent and both non-trivial — a LocalLayout that returned
// zeroes would agree with nothing and fail loudly, but one that somehow mirrored
// the Rust report would agree with everything and check nothing.
func TestGate_AbiCrossCheckDetectsHeaderDrift(t *testing.T) {
	// Reaching this line means init() already ran the cross-check without calling
	// panic, so the remaining risk is a check that is vacuous rather than wrong.
	sizes, aligns := ffi.LocalLayout()

	if len(sizes) == 0 {
		t.Fatal("LocalLayout reported no types; the ABI cross-check has nothing to compare")
	}
	for i, s := range sizes {
		if s == 0 {
			t.Fatalf("type %d compiled to size 0; the cross-check would pass vacuously", i)
		}
		if aligns[i] == 0 {
			t.Fatalf("type %d compiled to alignment 0", i)
		}
		if s%aligns[i] != 0 {
			t.Fatalf("type %d has size %d which is not a multiple of its alignment %d",
				i, s, aligns[i])
		}
	}

	expected := []uint32{
		gusset.ExpectedHeaderSize,
		gusset.ExpectedStatusSize,
		gusset.ExpectedLayoutSize,
		gusset.ExpectedStatsSize,
	}
	if len(sizes) != len(expected) {
		t.Fatalf("ABI covers %d types, constants describe %d", len(sizes), len(expected))
	}
	for i := range expected {
		if sizes[i] != expected[i] {
			t.Fatalf("type %d: cgo compiled %d bytes, the documented constant is %d",
				i, sizes[i], expected[i])
		}
	}
}
