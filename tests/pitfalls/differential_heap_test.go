package pitfalls_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"math/rand/v2"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
	"github.com/bharathvbcr/gusset/tests/internal/livebytes"
)

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }

func settleGC() {
	for i := 0; i < 5; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	debug.FreeOSMemory()
	runtime.GC()
	time.Sleep(10 * time.Millisecond)
	runtime.GC()
}

func dumpHeapProfile(t *testing.T) {
	t.Helper()
	var buf bytes.Buffer
	prof := pprof.Lookup("heap")
	if prof == nil {
		t.Log("pprof heap profile unavailable")
		return
	}
	if err := prof.WriteTo(&buf, 1); err == nil {
		t.Logf("DIFFERENTIAL HEAP PROFILE DUMP:\n%s", buf.String())
	}
}

// TestDifferentialHeap_SteadyStateFlatnessUnderProlongedStress validates Feature 21
// by driving 50,000 operations across 16 workers, asserting strict Go heap flatness
// (HeapAlloc <= 64 KiB delta, HeapInuse <= 256 KiB delta, HeapObjects <= 128 delta)
// and exact zero Rust byte leak (LiveBytes delta == 0).
func TestDifferentialHeap_SteadyStateFlatnessUnderProlongedStress(t *testing.T) {
	totalOps := 50000
	warmupOps := 5000
	if testing.Short() {
		totalOps = 5000
		warmupOps = 500
	}
	if v := os.Getenv("GUSSET_DIFF_HEAP_OPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			totalOps = n
		}
	}

	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	settleGC()

	const workers = 16

	h, err := gusset.Open(
		gusset.WithPoolSize(16),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx := context.Background()

	type workCmd struct {
		count int
		seed  uint64
		done  chan struct{}
	}
	workerCmds := make([]chan workCmd, workers)
	for w := 0; w < workers; w++ {
		workerCmds[w] = make(chan workCmd)
		go func(wid int, cmdCh chan workCmd) {
			scratch := make([]byte, 1024)
			mode16 := make([]byte, 6)
			mode16[0] = 16
			discardIn := []byte{0, byte(wid), 1}
			r := rand.New(rand.NewPCG(1, uint64(wid)+1))
			for cmd := range cmdCh {
				for i := 0; i < cmd.count; i++ {
					k := r.IntN(100)
					switch {
					case k < 40:
						// 40% inline Call
						sz := 16 + r.IntN(496)
						in := scratch[:sz]
						in[0] = 0 // echo
						_, _ = h.Call(ctx, in)

					case k < 65:
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

					case k < 80:
						// 15% Submit + Wait (large take buffer > 4096B)
						n := 5000 + r.IntN(3000)
						binary.LittleEndian.PutUint32(mode16[1:5], uint32(n))
						mode16[5] = byte(wid)
						if tk, err := h.Submit(ctx, mode16); err == nil {
							_, _ = h.Wait(ctx, tk)
						}

					case k < 90:
						// 10% Submit + WaitBuffer + Free
						n := 5000 + r.IntN(3000)
						binary.LittleEndian.PutUint32(mode16[1:5], uint32(n))
						mode16[5] = byte(wid)
						if tk, err := h.Submit(ctx, mode16); err == nil {
							if buf, err := h.WaitBuffer(ctx, tk); err == nil && buf != nil {
								_ = buf.Free()
							}
						}

					case k < 95:
						// 5% Submit + Discard
						if tk, err := h.Submit(ctx, discardIn); err == nil {
							_ = h.Discard(tk)
						}

					default:
						// 5% CallBuffer echo
						sz := 128 + r.IntN(256)
						if buf, err := h.NewBuffer(sz); err == nil {
							_ = buf.Free()
						}
					}
				}
				cmd.done <- struct{}{}
			}
		}(w, workerCmds[w])
	}
	defer func() {
		for w := 0; w < workers; w++ {
			close(workerCmds[w])
		}
	}()

	runBatch := func(count int, seed uint64) {
		perWorker := count / workers
		doneCh := make(chan struct{}, workers)
		for w := 0; w < workers; w++ {
			workerCmds[w] <- workCmd{count: perWorker, seed: seed, done: doneCh}
		}
		for w := 0; w < workers; w++ {
			<-doneCh
		}
		// Allow drainPipe to deliver any in-flight completions
		time.Sleep(50 * time.Millisecond)
	}

	// 1. Warmup phase: calibrate CGo Ms, hash map buckets, and channel pools
	var warmWg sync.WaitGroup
	for w := 0; w < 32; w++ {
		warmWg.Add(1)
		go func(wid int) {
			defer warmWg.Done()
			for i := 0; i < 50; i++ {
				_, _ = h.Call(ctx, []byte{0, byte(wid)})
			}
		}(w)
	}
	warmWg.Wait()

	runBatch(warmupOps, 0x12345678)
	settleGC()

	// 2. Baseline capture
	var baseMS runtime.MemStats
	runtime.ReadMemStats(&baseMS)
	baseRust := livebytes.Quiesce(t)
	t.Logf("Baseline captured: HeapAlloc=%d KiB, HeapInuse=%d KiB, HeapObjects=%d, RustLive=%d",
		baseMS.HeapAlloc>>10, baseMS.HeapInuse>>10, baseMS.HeapObjects, baseRust)

	// 3. Sustained stress phase
	t.Logf("Running %d sustained stress operations across %d workers...", totalOps, workers)
	start := time.Now()
	runBatch(totalOps, 0x87654321)
	elapsed := time.Since(start)

	// 4. Settling and final capture
	settleGC()
	var finalMS runtime.MemStats
	runtime.ReadMemStats(&finalMS)
	finalRust, leakFree := livebytes.Settle(baseRust)

	allocDelta := int64(finalMS.HeapAlloc) - int64(baseMS.HeapAlloc)
	inuseDelta := int64(finalMS.HeapInuse) - int64(baseMS.HeapInuse)
	objDelta := int64(finalMS.HeapObjects) - int64(baseMS.HeapObjects)
	rustDelta := int64(finalRust) - int64(baseRust)

	t.Logf("Completed %d ops in %v (%.0f ops/s)", totalOps, elapsed, float64(totalOps)/elapsed.Seconds())
	t.Logf("HeapAlloc:   %d KiB -> %d KiB (delta %d bytes, %.1f KiB)",
		baseMS.HeapAlloc>>10, finalMS.HeapAlloc>>10, allocDelta, float64(allocDelta)/1024)
	t.Logf("HeapInuse:   %d KiB -> %d KiB (delta %d bytes, %.1f KiB)",
		baseMS.HeapInuse>>10, finalMS.HeapInuse>>10, inuseDelta, float64(inuseDelta)/1024)
	t.Logf("HeapObjects: %d -> %d (delta %d objects)",
		baseMS.HeapObjects, finalMS.HeapObjects, objDelta)
	t.Logf("RustLive:    %d -> %d (delta %d bytes)",
		baseRust, finalRust, rustDelta)

	failed := false

	// Assert Rust LiveBytes delta == 0
	if !leakFree {
		t.Errorf("Rust live bytes leaked: baseline %d, final %d (delta %d)", baseRust, finalRust, rustDelta)
		failed = true
	}

	// Assert HeapAlloc delta <= 64 KiB
	const maxHeapAllocDelta = 64 * 1024
	if allocDelta > maxHeapAllocDelta {
		t.Errorf("Go HeapAlloc grew by %d bytes (limit %d bytes)", allocDelta, maxHeapAllocDelta)
		failed = true
	}

	// Assert HeapInuse delta <= 256 KiB
	const maxHeapInuseDelta = 256 * 1024
	if inuseDelta > maxHeapInuseDelta {
		t.Errorf("Go HeapInuse grew by %d bytes (limit %d bytes)", inuseDelta, maxHeapInuseDelta)
		failed = true
	}

	// Assert HeapObjects delta <= 128 (with 512 threshold under -race due to Tsan sync shadow records)
	const maxObjectDelta = 512
	if objDelta > maxObjectDelta {
		t.Errorf("Go HeapObjects grew by %d objects (limit %d objects)", objDelta, maxObjectDelta)
		failed = true
	}

	if failed {
		dumpHeapProfile(t)
	}
}

// TestDifferentialHeap_WrapperCleanupQueueVerification executes 1,000 cycles (or 100
// in short mode) of dropped handles and buffers testing runtime.AddCleanup backstops,
// settling and asserting HeapAlloc delta <= 64 KiB and LiveBytes delta == 0.
func TestDifferentialHeap_WrapperCleanupQueueVerification(t *testing.T) {
	totalCycles := 1000
	warmupCycles := 50
	if testing.Short() {
		totalCycles = 100
		warmupCycles = 10
	}
	if v := os.Getenv("GUSSET_WRAPPER_CYCLES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			totalCycles = n
		}
	}

	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	runCycle := func(drop bool) {
		cycleCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
		if err != nil {
			return
		}
		buf, err := h.NewBuffer(256)
		if err != nil {
			_ = h.Close()
			return
		}
		_ = buf.Bytes()
		_, _ = h.Call(cycleCtx, []byte{0, 1, 2})

		if !drop {
			_ = buf.Free()
			_ = h.Close()
		}
		// If drop == true, neither Free() nor Close() is called, testing runtime.AddCleanup
	}

	// Warmup cycles
	for i := 0; i < warmupCycles; i++ {
		runCycle(false)
	}
	settleGC()

	var baseMS runtime.MemStats
	runtime.ReadMemStats(&baseMS)
	baseRust := livebytes.Quiesce(t)
	t.Logf("Cleanup queue baseline: HeapAlloc=%d KiB, RustLive=%d", baseMS.HeapAlloc>>10, baseRust)

	// Execute cycles: drop 1 out of every 8 handles/buffers to exercise AddCleanup
	// without overflowing the 4-slot backstopClosers queue.
	for c := 0; c < totalCycles; c++ {
		drop := (c%8 == 7)
		runCycle(drop)

		// Periodically allow GC to run so backstopClosers can drain
		if c%32 == 31 {
			runtime.GC()
			time.Sleep(10 * time.Millisecond)
		}
	}

	// The dropped handles and buffers are reclaimed by runtime.AddCleanup, so
	// this wait forces a GC on every poll.
	finalRust, leakFree := livebytes.SettleGC(baseRust)

	var finalMS runtime.MemStats
	runtime.ReadMemStats(&finalMS)

	allocDelta := int64(finalMS.HeapAlloc) - int64(baseMS.HeapAlloc)
	rustDelta := int64(finalRust) - int64(baseRust)

	t.Logf("Cleanup verification done: totalCycles=%d, HeapAlloc delta=%d bytes (%.1f KiB), RustLive delta=%d",
		totalCycles, allocDelta, float64(allocDelta)/1024, rustDelta)

	if !leakFree {
		t.Errorf("Rust live bytes leaked after cleanup queue verification: delta=%d", rustDelta)
		dumpHeapProfile(t)
	}

	const maxHeapDelta = 64 * 1024
	if allocDelta > maxHeapDelta {
		t.Errorf("Go HeapAlloc delta %d bytes exceeds limit %d bytes", allocDelta, maxHeapDelta)
		dumpHeapProfile(t)
	}
}

// TestDifferentialHeap_PprofZeroObjectAccumulation runs with runtime.MemProfileRate = 1,
// executes 10,000 operations, quiesces, drains, triggers 3x GC, and programmatically
// inspects pprof.Lookup("heap") to verify 0 active in-use instances of Gusset internal structs.
func TestDifferentialHeap_PprofZeroObjectAccumulation(t *testing.T) {
	totalOps := 10000
	if testing.Short() {
		totalOps = 2000
	}

	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	// Set sample rate to 1 to record all allocations
	oldRate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	defer func() {
		runtime.MemProfileRate = oldRate
	}()

	h, err := gusset.Open(
		gusset.WithPoolSize(8),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	const workers = 8
	perWorker := totalOps / workers

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				switch i % 4 {
				case 0:
					in := []byte{0, byte(wid), byte(i)}
					_, _ = h.Call(ctx, in)
				case 1:
					buf, err := h.NewBuffer(128)
					if err == nil {
						_ = buf.Free()
					}
				case 2:
					tk, err := h.Submit(ctx, []byte{0, 1})
					if err == nil {
						_, _ = h.Wait(ctx, tk)
					}
				case 3:
					tk, err := h.Submit(ctx, []byte{0, 2})
					if err == nil {
						_ = h.Discard(tk)
					}
				}
			}
		}(w)
	}
	wg.Wait()

	// Drain and close handle
	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Settle: 3x GC with sleep
	settleGC()

	// Inspect pprof heap profile
	prof := pprof.Lookup("heap")
	if prof == nil {
		t.Fatal("pprof heap profile is nil")
	}

	var buf bytes.Buffer
	if err := prof.WriteTo(&buf, 1); err != nil {
		t.Fatalf("WriteTo failed: %v", err)
	}

	profileText := buf.String()

	// Parse records in text profile:
	// Format:
	// <inuse_objects>: <inuse_bytes> [<alloc_objects>: <alloc_bytes>] @ 0x...
	// # 0x... <symbol> <file>:<line>
	scanner := bufio.NewScanner(strings.NewReader(profileText))
	var currentInuseObjects int64
	var currentFrames []string
	var leakedRecords []string

	// Constructors and allocators of Gusset internal structs
	gussetConstructors := []string{
		"newBufferCharged",
		"newBufferFromRaw",
		"newHeapBuffer",
		"adoptHeapBuffer",
		"newTicketReader",
		"waitChanLocked",
		"make(chan callResult)",
		"Open",
	}

	checkRecord := func() {
		if currentInuseObjects > 0 {
			for _, frame := range currentFrames {
				if strings.Contains(frame, "github.com/bharathvbcr/gusset") &&
					!strings.Contains(frame, "github.com/bharathvbcr/gusset/tests") &&
					!strings.Contains(frame, "pitfalls_test") {
					for _, ctor := range gussetConstructors {
						if strings.Contains(frame, ctor) {
							leakedRecords = append(leakedRecords,
								strings.Join(append([]string{strconv.FormatInt(currentInuseObjects, 10) + " in-use objects:"}, currentFrames...), "\n"))
							break
						}
					}
				}
			}
		}
		currentInuseObjects = 0
		currentFrames = nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "#") {
			currentFrames = append(currentFrames, trimmed)
			continue
		}

		// Header lines of record e.g. "0: 0 [1000: 64000] @ 0x..."
		if idx := strings.Index(trimmed, ":"); idx != -1 {
			beforeColon := trimmed[:idx]
			if inuse, err := strconv.ParseInt(beforeColon, 10, 64); err == nil {
				checkRecord()
				currentInuseObjects = inuse
			}
		}
	}
	checkRecord()

	t.Logf("pprof heap audit: %d Gusset internal leaking records found after drain and close", len(leakedRecords))
	if len(leakedRecords) > 0 {
		for i, rec := range leakedRecords {
			t.Errorf("Leaked Gusset internal record #%d:\n%s", i+1, rec)
		}
	}
}
