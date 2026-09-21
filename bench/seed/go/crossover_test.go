package ffibench

// Raw blocking cgo versus Gusset, on byte-identical work.
//
// Every other benchmark here measures a noop, which is where any coordination
// layer looks worst and which is not why anyone embeds Rust in Go. The decision
// an adopter actually faces is: *at what work duration does Gusset's
// coordination stop mattering?* Answering that needs a dial, and it needs both
// transports turning the identical dial.
//
// `rs_spin` in bench/seed/rs and diagnostic mode 11 in gusset::pool run the same
// integer loop, so the difference between these measurements is transport alone:
//
//   - RawCgo: one blocking cgo call. The calling goroutine's M is parked inside
//     Rust for the whole of the work and the Go scheduler cannot reuse it.
//   - Gusset: semaphore, submit, worker dispatch, pipe write, netpoller wake.
//     The M is released immediately; the goroutine parks on a Go channel.
//
// The serial sweep shows the fixed overhead and how fast it stops being visible.
// The parallel sweep is the deployment view, and it is the one that inverts:
// that is where blocking cgo has to grow the thread pool to keep making
// progress. Both report Δthreads so the cost is visible and not just asserted.
//
// This lives in the seed module because Go forbids cgo in _test.go files, and
// because neither libffibench nor a benchmark-only Gusset dependency belongs in
// the main module's build graph.
//
// Run:
//
//	cd bench/seed/go && go test -run '^$' -bench Crossover -benchtime=1s

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// spinPayload encodes an iteration count for Gusset diagnostic mode 11.
func spinPayload(iters uint32) []byte {
	p := make([]byte, 5)
	p[0] = 11
	binary.LittleEndian.PutUint32(p[1:], iters)
	return p
}

// workSizes are iteration counts spanning a noop to roughly a millisecond.
var workSizes = []uint32{0, 1_000, 10_000, 100_000, 1_000_000}

// calibrateBatch is how long one timed batch must last before its per-call
// figure is trustworthy.
//
// Timing a single Spin(1000) means timing about 700 ns with two time.Now()
// calls and a cgo transition inside the measurement. One scheduler event moves
// it by a third, and taking the minimum of five such samples does not help
// when all five are that fragile: a real recording labelled the identical loop
// 708ns in three arms and 958ns in the fourth. Those names are compared across
// processes by tools/benchplot to detect a contaminated arm, so a name that
// wobbles by 35% on its own is a false alarm generator.
//
// A millisecond is ~1400 repetitions of the smallest work size, which puts the
// timer and a stray preemption several orders of magnitude below the signal.
const calibrateBatch = time.Millisecond

// calibrateMaxReps bounds the doubling, so a pathologically fast machine cannot
// turn calibration into an unbounded loop.
const calibrateMaxReps = 1 << 22

// Burn-in bounds. The burn-in waits for this process's CPU clock to ramp, which
// is a settling time rather than a number of loops, so it probes until the
// measured rate stops improving instead of guessing a duration. Fixed durations
// were tried first and neither was enough on its own: 100 ms took the first
// arm's 1000-iteration calibration from ~1000 ns to 828 ns against a settled
// 666–703 ns, still 24% out.
const (
	burnInProbe  = 20 * time.Millisecond // one probe
	burnInMax    = 3 * time.Second       // give up and proceed
	burnInSettle = 0.02                  // two probes within 2% means ramped
)

// burnIn brings this process's CPU clock up before anything is calibrated.
//
// `bench/record.sh` warms the machine before the first arm, which removed most
// of a frequency ramp that had the four arms calibrating the same deterministic
// loop at 1048/751/737/696 us — monotonic in recording order, at every work
// size. That fix works at the machine level and took the spread at 1e6
// iterations from 51% to 9.5%.
//
// It cannot close the gap entirely, because each arm is its own process and the
// warm-up ends before that process starts: the first arm still calibrates on a
// core that has just been handed a freshly exec'd binary. So the burn-in also
// happens here, inside every arm's own process, which is the only place that is
// symmetric across arms. Together they take the 1000-iteration spread to ~3%.
//
// It returns as soon as the machine stops getting faster, so an already-warm
// process pays two probes and a cold one pays however long its clock takes.
var burnIn = sync.OnceFunc(func() {
	deadline := time.Now().Add(burnInMax)
	prev := 0.0
	for time.Now().Before(deadline) {
		reps := 0
		start := time.Now()
		var elapsed time.Duration
		for {
			runtime.KeepAlive(Spin(100_000))
			reps++
			if elapsed = time.Since(start); elapsed >= burnInProbe {
				break
			}
		}
		rate := float64(elapsed) / float64(reps)
		if prev > 0 {
			delta := rate - prev
			if delta < 0 {
				delta = -delta
			}
			if delta/prev <= burnInSettle {
				return
			}
		}
		prev = rate
	}
})

// calibrate measures what an iteration count actually costs on this machine, so
// the benchmark names carry real durations rather than an assumed clock rate.
//
// A fixed-cost loop's minimum is its least noisy estimate — but the minimum of
// five measurements is only as good as one measurement, so each sample here is
// a batch long enough to be worth taking a minimum of.
func calibrate(iters uint32) time.Duration {
	if iters == 0 {
		return 0
	}
	burnIn()
	best := time.Duration(1<<63 - 1)
	for i := 0; i < 5; i++ {
		reps := 1
		var elapsed time.Duration
		for {
			start := time.Now()
			for r := 0; r < reps; r++ {
				Spin(uint64(iters))
			}
			elapsed = time.Since(start)
			if elapsed >= calibrateBatch || reps >= calibrateMaxReps {
				break
			}
			// Scale straight to the target rather than doubling blindly: one
			// extra batch instead of ten for the smallest size.
			next := reps * 2
			if elapsed > 0 {
				if want := int(float64(reps) * float64(calibrateBatch) / float64(elapsed)); want > next {
					next = want
				}
			}
			if next > calibrateMaxReps {
				next = calibrateMaxReps
			}
			reps = next
		}
		if per := elapsed / time.Duration(reps); per < best {
			best = per
		}
	}
	return best
}

func label(iters uint32) string {
	if iters == 0 {
		return "noop"
	}
	d := calibrate(iters)
	switch {
	case d < time.Microsecond:
		return fmt.Sprintf("%dit-%dns", iters, d.Nanoseconds())
	case d < time.Millisecond:
		return fmt.Sprintf("%dit-%dus", iters, d.Microseconds())
	default:
		return fmt.Sprintf("%dit-%dus", iters, d.Microseconds())
	}
}

// labelsFor calibrates every work size up front, on a warmed core.
//
// tools/internal/benchfile compares the arms' calibrations of this deterministic
// loop across processes to decide whether the recording is trustworthy, so every
// arm has to measure the loop under the same conditions or an honest recording
// fails its own check.
//
// Two separate things broke that, and the first diagnosis of it was wrong, so
// both are worth writing down:
//
//   - Calibrating inside the b.Run loop. b.Run is synchronous, so each size was
//     measured on a process the previous sub-benchmark had already run in.
//     Hoisting fixes that, and hoisting alone made things *worse*, which is what
//     showed the second cause.
//   - A cold core. With calibration hoisted to the very top of the process, the
//     first arm measures the machine at its slowest. Recorded that way,
//     CrossoverSerial/RawCgo calibrated 1 µs / 11 µs / 113 µs / 1048 µs where
//     the later arms reported 709 ns / 6 µs / 69 µs / 696 µs for the identical
//     loop — uniformly ~50% high across every size at once, which is a clock
//     artifact and not the sub-microsecond sampling noise it was first taken
//     for.
//
// The cold-core reading is not a guess: across one recording the four arms
// calibrated 1000, 917, 833 and 709 ns for the identical loop *in the order
// record.sh runs them*. A monotonic decay in arm order is a machine warming up.
// Arm 1 is the coldest because it starts right after record.sh has finished
// sampling CPU idle on a deliberately quiet box; every later arm inherits a core
// that the previous arm just spent a second driving flat out.
//
// So: burn in, then calibrate, then run. The burn-in is `burnIn`, reached
// through calibrate() rather than called here, so it covers every path to a
// label rather than only this one.
func labelsFor(sizes []uint32) []string {
	out := make([]string, len(sizes))
	for i, n := range sizes {
		out[i] = label(n)
	}
	return out
}

func poolSize() int {
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		n = 1
	}
	if n > gusset.MaxPoolSize {
		n = gusset.MaxPoolSize
	}
	return n
}

// BenchmarkCrossoverSerial is the fixed-overhead view: one goroutine, one call
// at a time. Gusset is slower here by construction; the sweep shows how quickly
// that gap stops mattering.
func BenchmarkCrossoverSerial(b *testing.B) {
	names := labelsFor(workSizes)
	for i, iters := range workSizes {
		name := names[i]

		b.Run("RawCgo/"+name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			var acc uint64
			for i := 0; i < b.N; i++ {
				acc += Spin(uint64(iters))
			}
			runtime.KeepAlive(acc)
		})

		b.Run("Gusset/"+name, func(b *testing.B) {
			h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
			if err != nil {
				b.Fatalf("Open failed: %v", err)
			}
			defer h.Close()
			ctx := context.Background()
			payload := spinPayload(iters)
			if _, err := h.Call(ctx, payload); err != nil {
				b.Fatalf("warm-up Call failed: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := h.Call(ctx, payload); err != nil {
					b.Fatalf("Call failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkCrossoverParallel is the deployment view: GOMAXPROCS goroutines
// calling concurrently.
//
// A blocking cgo call holds its M for the duration of the work, so concurrent
// callers force the Go scheduler to create threads — precisely the failure
// Gusset exists to prevent. Gusset's callers park on a Go channel instead, so
// the thread count stays flat. Δthreads is reported for both.
func BenchmarkCrossoverParallel(b *testing.B) {
	names := labelsFor(workSizes)
	for i, iters := range workSizes {
		name := names[i]

		b.Run("RawCgo/"+name, func(b *testing.B) {
			before := gusset.Threads()
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				var acc uint64
				for pb.Next() {
					acc += Spin(uint64(iters))
				}
				runtime.KeepAlive(acc)
			})
			b.StopTimer()
			b.ReportMetric(float64(gusset.Threads()-before), "Δthreads")
		})

		b.Run("Gusset/"+name, func(b *testing.B) {
			h, err := gusset.Open(gusset.WithPoolSize(poolSize()), gusset.WithDiagnosticEngine())
			if err != nil {
				b.Fatalf("Open failed: %v", err)
			}
			defer h.Close()
			ctx := context.Background()
			payload := spinPayload(iters)
			if _, err := h.Call(ctx, payload); err != nil {
				b.Fatalf("warm-up Call failed: %v", err)
			}

			before := gusset.Threads()
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := h.Call(ctx, payload); err != nil {
						b.Errorf("Call failed: %v", err)
						return
					}
				}
			})
			b.StopTimer()
			b.ReportMetric(float64(gusset.Threads()-before), "Δthreads")
		})
	}
}

// threadPressureConcurrency is deliberately far above GOMAXPROCS.
//
// BenchmarkCrossoverParallel cannot show the failure this library exists to
// prevent: RunParallel runs exactly GOMAXPROCS goroutines and the runtime
// already has that many Ms, so nothing has to be created and Δthreads reads
// zero for blocking cgo. The benchmark then quietly proves nothing about thread
// pressure. A server with in-flight requests does not work that way.
const threadPressureConcurrency = 512

// sampleThreads polls the scheduler thread count until stop is closed.
func sampleThreads(stop <-chan struct{}, peak *atomic.Int64) *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if n := gusset.Threads(); n > peak.Load() {
				peak.Store(n)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return &wg
}

// processMemKiB reads this process's resident and virtual size from ps(1).
//
// Measured rather than derived. The obvious shortcut — threads times a stack
// size — needs a stack size to multiply by, and there is no single right one:
// Gusset sizes its Rust workers at 8 MiB explicitly, while a Go M running cgo
// gets whatever the platform gives it, which is not the same number and is not
// the same across darwin, glibc and musl. Multiplying by an assumed constant
// would turn a measurement into an assertion.
//
// runtime.MemStats cannot answer this: thread stacks are not the Go heap.
func processMemKiB() (rss, vsz int64) {
	out, err := exec.Command("ps", "-o", "rss=,vsz=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, 0
	}
	rss, _ = strconv.ParseInt(fields[0], 10, 64)
	vsz, _ = strconv.ParseInt(fields[1], 10, 64)
	return rss, vsz
}

// sampleMemory polls peak RSS and virtual size until stop is closed.
//
// Slower than the thread sampler on purpose: each sample forks ps, so a 1 ms
// cadence would have the measurement competing with the thing being measured.
func sampleMemory(stop <-chan struct{}, peakRSS, peakVSZ *atomic.Int64) *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			rss, vsz := processMemKiB()
			if rss > peakRSS.Load() {
				peakRSS.Store(rss)
			}
			if vsz > peakVSZ.Load() {
				peakVSZ.Store(vsz)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()
	return &wg
}

var (
	threadOwnerMu sync.Mutex
	threadOwner   string
)

// claimThreadMeasurement records which transport first sampled OS threads in
// this process, and reports whether transport may still publish an absolute peak.
//
// Go never destroys an M. A second transport in the same process therefore
// starts from every thread the first one created, so its absolute peak is the
// predecessor's high-water mark rather than a measurement of itself. That is not
// hypothetical: running the whole file in one process reports an identical
// figure for both transports at every concurrency level, and a chart drawn from
// it shows two flat, equal lines — "cgo is fine", from data that measured
// nothing.
//
// An ascending sweep within a single transport stays sound, because the
// high-water mark is monotonic and each point remains "threads needed to sustain
// this concurrency". So the owning transport keeps reporting; only a second one
// is refused.
func claimThreadMeasurement(transport string) (ok bool, owner string) {
	threadOwnerMu.Lock()
	defer threadOwnerMu.Unlock()
	if threadOwner == "" {
		threadOwner = transport
	}
	return threadOwner == transport, threadOwner
}

// reportPeakThreads publishes the delta always and the absolute peak only when
// no other transport has measured threads in this process.
//
// The absolute figure is withheld rather than printed, because a number that
// reads like a measurement and is not one is how "cgo is fine" survives
// benchmarking. A missing metric fails tools/benchplot loudly; a wrong one
// becomes a chart.
func reportPeakThreads(b *testing.B, transport string, peak, base int64) {
	b.ReportMetric(float64(peak-base), "peakDthreads")
	ok, owner := claimThreadMeasurement(transport)
	if !ok {
		b.Logf("absolute peakThreads withheld: this process already measured %s, so "+
			"%s would report %s's threads as its own; run one transport per process "+
			"(`make bench-crossover`, `make bench-scaling`)", owner, transport, owner)
		return
	}
	b.ReportMetric(float64(peak), "peakThreads")
}

// BenchmarkThreadPressure is the measurement the whole design rests on.
//
// Each iteration dispatches threadPressureConcurrency concurrent calls and waits
// for all of them. Under blocking cgo every one of those goroutines parks an M
// inside Rust, so the scheduler must create threads to keep the rest runnable
// and the thread count tracks *concurrency*. Under Gusset the callers park on a
// Go channel and the pool is a fixed set of OS threads, so the thread count
// tracks the *pool*.
//
// ns/op here is per batch of 512 calls, not per call.
//
// Run each transport in its own process, with -count=1:
//
//	go test -run '^$' -bench 'ThreadPressure/10000000it/RawCgo' -benchtime=5x .
//	go test -run '^$' -bench 'ThreadPressure/10000000it/Gusset' -benchtime=5x .
//
// Go never destroys an M, so every thread a run creates is still there for the
// next one. Run both transports in one process and the Gusset figure inherits
// the ~200 threads blocking cgo just created; run -count>1 and every repetition
// after the first measures a delta against threads its own predecessor made,
// which decays to zero. Both contaminations report that nothing happened, which
// is how "cgo is fine" survives benchmarking. `make bench-crossover` enforces
// the isolation.
func BenchmarkThreadPressure(b *testing.B) {
	// Swept, because the effect is a function of how long each call blocks.
	//
	// Go's sysmon only retakes a P from a goroutine that has been inside a cgo
	// call for longer than ~20 us, and it wakes on an adaptive 20 us..10 ms
	// tick. A 70 us call usually returns before sysmon looks, so the thread
	// count barely moves and blocking cgo appears harmless. At millisecond
	// scale sysmon retakes every time, a fresh M picks up the P, that goroutine
	// enters cgo too, and the thread count chases concurrency instead of core
	// count. Measuring only the short case is how "cgo is fine" survives
	// benchmarking.
	for _, iters := range []uint32{100_000, 10_000_000} {
		b.Run(label(iters), func(b *testing.B) { threadPressure(b, iters) })
	}
}

func threadPressure(b *testing.B, iters uint32) {
	b.Run(fmt.Sprintf("RawCgo/conc%d", threadPressureConcurrency), func(b *testing.B) {
		base := gusset.Threads()
		var peak atomic.Int64
		peak.Store(base)
		stop := make(chan struct{})
		sampler := sampleThreads(stop, &peak)

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var wg sync.WaitGroup
			for j := 0; j < threadPressureConcurrency; j++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					runtime.KeepAlive(Spin(uint64(iters)))
				}()
			}
			wg.Wait()
		}
		b.StopTimer()
		close(stop)
		sampler.Wait()
		reportPeakThreads(b, "RawCgo", peak.Load(), base)
	})

	b.Run(fmt.Sprintf("Gusset/conc%d", threadPressureConcurrency), func(b *testing.B) {
		h, err := gusset.Open(gusset.WithPoolSize(poolSize()), gusset.WithDiagnosticEngine())
		if err != nil {
			b.Fatalf("Open failed: %v", err)
		}
		defer h.Close()
		ctx := context.Background()
		payload := spinPayload(iters)
		if _, err := h.Call(ctx, payload); err != nil {
			b.Fatalf("warm-up Call failed: %v", err)
		}

		base := gusset.Threads()
		var peak atomic.Int64
		peak.Store(base)
		stop := make(chan struct{})
		sampler := sampleThreads(stop, &peak)

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var wg sync.WaitGroup
			for j := 0; j < threadPressureConcurrency; j++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := h.Call(ctx, payload); err != nil {
						b.Errorf("Call failed: %v", err)
					}
				}()
			}
			wg.Wait()
		}
		b.StopTimer()
		close(stop)
		sampler.Wait()
		reportPeakThreads(b, "Gusset", peak.Load(), base)
	})
}

// threadScalingConcurrency is the sweep the thread-cost plot is drawn from.
//
// BenchmarkThreadPressure answers "does blocking cgo grow the thread pool" at
// one concurrency level. The question an adopter actually asks is "how does the
// cost scale with my in-flight request count", and that needs a curve: blocking
// cgo should track concurrency, Gusset should track the pool and stay flat.
// A single point cannot tell those two shapes apart.
var threadScalingConcurrency = []int{32, 128, 512, 2048}

// BenchmarkThreadScaling sweeps concurrency at a fixed ~7 ms of work.
//
// One transport per process, `-count=1`, for the reason BenchmarkThreadPressure
// documents: Go never destroys an M, so a shared process makes the second
// measurement inherit the first's threads and a repeated one measure a delta
// against threads its own predecessor made.
//
//	go test -run '^$' -bench 'ThreadScaling/RawCgo' -benchtime=3x -count=1 .
//	go test -run '^$' -bench 'ThreadScaling/Gusset' -benchtime=3x -count=1 .
//
// `make bench-scaling` enforces the isolation and writes the raw output that
// `tools/benchplot` draws from.
func BenchmarkThreadScaling(b *testing.B) {
	const iters = 10_000_000 // ~7 ms, past sysmon's P-retake threshold

	for _, conc := range threadScalingConcurrency {
		b.Run(fmt.Sprintf("RawCgo/conc%d", conc), func(b *testing.B) {
			base := gusset.Threads()
			var peak atomic.Int64
			peak.Store(base)
			stop := make(chan struct{})
			sampler := sampleThreads(stop, &peak)
			var peakRSS, peakVSZ atomic.Int64
			memSampler := sampleMemory(stop, &peakRSS, &peakVSZ)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var wg sync.WaitGroup
				for j := 0; j < conc; j++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						runtime.KeepAlive(Spin(uint64(iters)))
					}()
				}
				wg.Wait()
			}
			b.StopTimer()
			close(stop)
			sampler.Wait()
			memSampler.Wait()
			b.ReportMetric(float64(conc), "concurrency")
			reportPeakThreads(b, "RawCgo", peak.Load(), base)
			b.ReportMetric(float64(peakRSS.Load())/1024, "peakRSSmib")
			b.ReportMetric(float64(peakVSZ.Load())/1024, "peakVSZmib")
		})

		b.Run(fmt.Sprintf("Gusset/conc%d", conc), func(b *testing.B) {
			h, err := gusset.Open(gusset.WithPoolSize(poolSize()), gusset.WithDiagnosticEngine())
			if err != nil {
				b.Fatalf("Open failed: %v", err)
			}
			defer h.Close()
			ctx := context.Background()
			payload := spinPayload(iters)
			if _, err := h.Call(ctx, payload); err != nil {
				b.Fatalf("warm-up Call failed: %v", err)
			}

			base := gusset.Threads()
			var peak atomic.Int64
			peak.Store(base)
			stop := make(chan struct{})
			sampler := sampleThreads(stop, &peak)
			var peakRSS, peakVSZ atomic.Int64
			memSampler := sampleMemory(stop, &peakRSS, &peakVSZ)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var wg sync.WaitGroup
				for j := 0; j < conc; j++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if _, err := h.Call(ctx, payload); err != nil {
							b.Errorf("Call failed: %v", err)
						}
					}()
				}
				wg.Wait()
			}
			b.StopTimer()
			close(stop)
			sampler.Wait()
			memSampler.Wait()
			b.ReportMetric(float64(conc), "concurrency")
			reportPeakThreads(b, "Gusset", peak.Load(), base)
			b.ReportMetric(float64(peakRSS.Load())/1024, "peakRSSmib")
			b.ReportMetric(float64(peakVSZ.Load())/1024, "peakVSZmib")
		})
	}
}
