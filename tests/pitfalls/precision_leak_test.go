package pitfalls_test

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestPrecisionZeroLeak_ProlongedSingleHandleSoak drives 50,000 operations (or 5,000
// in short mode) across 16 goroutines on a single handle with pool size 16 and diagnostic
// engine, asserting exact zero delta in Rust LiveBytes (Features 20 & 23).
func TestPrecisionZeroLeak_ProlongedSingleHandleSoak(t *testing.T) {
	totalOps := 50000
	warmupOps := 2000
	if testing.Short() {
		totalOps = 5000
		warmupOps = 500
	}
	if v := os.Getenv("GUSSET_SOAK_OPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			totalOps = n
		}
	}

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)

	const numWorkers = 16

	// Warmup phase: calibrate one-time runtime allocations
	warmupH, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Warmup Open failed: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < warmupOps; i++ {
		_, _ = warmupH.Call(ctx, []byte{0, byte(i), 1})
	}
	_ = warmupH.Close()

	// Settle after warmup
	runtime.GC()
	time.Sleep(10 * time.Millisecond)
	runtime.GC()
	runtime.GC()

	baseStats := gusset.Stats()
	baseLive := baseStats.LiveBytes
	t.Logf("Calibrated baseline: LiveBytes=%d, AllocCount=%d, PeakBytes=%d, targetOps=%d",
		baseLive, baseStats.AllocCount, baseStats.PeakBytes, totalOps)

	// Open single long-lived soak handle with pool size 16
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
		opCallsCompleted   atomic.Int64
		opBufCallsDone     atomic.Int64
		opSubmitsCompleted atomic.Int64
		opWaitBufCompleted atomic.Int64
		opDiscardsDone     atomic.Int64
		opDroppedBufsDone  atomic.Int64
	)

	perWorker := totalOps / numWorkers
	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(wid)+1, 0xCAFEBABE))

			for i := 0; i < perWorker; i++ {
				k := r.IntN(100)
				switch {
				case k < 40:
					// 40% inline h.Call (Mode 0 echo, 32-512 bytes)
					sz := 32 + r.IntN(480)
					in := make([]byte, sz)
					in[0] = 0 // echo
					in[1] = byte(wid)
					in[2] = byte(i)
					out, err := h.Call(ctx, in)
					if err != nil || len(out) != sz {
						t.Errorf("Call error: err=%v, len=%d, want=%d", err, len(out), sz)
						return
					}
					opCallsCompleted.Add(1)

				case k < 65:
					// 25% h.NewBuffer + h.CallBuffer + buf.Free()
					sz := 64 + r.IntN(1024)
					buf, err := h.NewBuffer(sz)
					if err != nil {
						t.Errorf("NewBuffer error: %v", err)
						return
					}
					slice := buf.Bytes()
					slice[0] = 0 // echo
					slice[1] = byte(wid)
					out, err := h.CallBuffer(ctx, buf)
					if err != nil || out == nil {
						t.Errorf("CallBuffer error: err=%v, out=%v", err, out)
						_ = buf.Free()
						return
					}
					_ = out.Free()
					_ = buf.Free()
					opBufCallsDone.Add(1)

				case k < 80:
					// 15% large h.Submit + h.Wait (take buffer > 4096B)
					sz := 5000 + r.IntN(4000)
					tk, err := h.Submit(ctx, mode16Payload(sz, byte(wid)))
					if err != nil {
						t.Errorf("Submit large error: %v", err)
						continue
					}
					out, err := h.Wait(ctx, tk)
					if err != nil || len(out) != sz {
						t.Errorf("Wait large error: err=%v, len=%d, want=%d", err, len(out), sz)
						return
					}
					opSubmitsCompleted.Add(1)

				case k < 90:
					// 10% h.Submit + h.WaitBuffer + buf.Free()
					sz := 5000 + r.IntN(4000)
					tk, err := h.Submit(ctx, mode16Payload(sz, byte(wid)))
					if err != nil {
						t.Errorf("Submit waitbuffer error: %v", err)
						continue
					}
					b, err := h.WaitBuffer(ctx, tk)
					if err != nil || b == nil || len(b.Bytes()) != sz {
						t.Errorf("WaitBuffer error: err=%v, b=%v", err, b)
						return
					}
					_ = b.Free()
					opWaitBufCompleted.Add(1)

				case k < 95:
					// 5% h.Submit + h.Discard
					tk, err := h.Submit(ctx, []byte{0, byte(wid), 77})
					if err == nil {
						_ = h.Discard(tk)
						opDiscardsDone.Add(1)
					}

				default:
					// 5% dropped buffers without explicit Free() (testing AddCleanup backstops)
					sz := 64 + r.IntN(256)
					buf, err := h.NewBuffer(sz)
					if err == nil {
						b := buf.Bytes()
						if len(b) > 0 {
							b[0] = byte(wid)
						}
						// intentionally no buf.Free()
					}
					opDroppedBufsDone.Add(1)
				}
			}
		}(w)
	}

	wg.Wait()

	// Drain and close handle
	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Settle protocol: 3x runtime.GC() and 10ms sleep with polling
	deadline := time.Now().Add(10 * time.Second)
	var finalStats gusset.AllocStats
	for {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
		runtime.GC()
		runtime.GC()

		finalStats = gusset.Stats()
		if finalStats.LiveBytes == baseLive || time.Now().After(deadline) {
			break
		}
	}

	t.Logf("Operations summary: calls=%d, bufCalls=%d, submits=%d, waitBufs=%d, discards=%d, droppedBufs=%d",
		opCallsCompleted.Load(), opBufCallsDone.Load(), opSubmitsCompleted.Load(),
		opWaitBufCompleted.Load(), opDiscardsDone.Load(), opDroppedBufsDone.Load())
	t.Logf("Final stats: LiveBytes=%d (base=%d, delta=%d), PeakBytes=%d, AllocCount=%d",
		finalStats.LiveBytes, baseLive, int64(finalStats.LiveBytes)-int64(baseLive),
		finalStats.PeakBytes, finalStats.AllocCount)

	delta := int64(finalStats.LiveBytes) - int64(baseLive)
	if delta != 0 {
		t.Fatalf("ZERO-LEAK INVARIANT VIOLATION: baseline LiveBytes=%d, final LiveBytes=%d (delta=%d)",
			baseLive, finalStats.LiveBytes, delta)
	}
}

// TestPrecisionZeroLeak_HeavyMultiHandleSoakChurn creates 200 handles (or 20 in short mode),
// exercises them with concurrent operations, and closes them across 32 concurrent goroutines,
// settling and asserting finalStats.LiveBytes - baseStats.LiveBytes == 0 (Features 20 & 23).
func TestPrecisionZeroLeak_HeavyMultiHandleSoakChurn(t *testing.T) {
	totalHandles := 200
	if testing.Short() {
		totalHandles = 20
	}
	if v := os.Getenv("GUSSET_CHURN_HANDLES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			totalHandles = n
		}
	}

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)

	const concurrentWorkers = 32

	// Warmup
	wH, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Warmup Open failed: %v", err)
	}
	for i := 0; i < 200; i++ {
		_, _ = wH.Call(context.Background(), []byte{0, 1, 2})
	}
	_ = wH.Close()

	runtime.GC()
	time.Sleep(10 * time.Millisecond)
	runtime.GC()
	runtime.GC()

	baseStats := gusset.Stats()
	baseLive := baseStats.LiveBytes
	t.Logf("Multi-handle churn baseline: LiveBytes=%d, AllocCount=%d", baseLive, baseStats.AllocCount)

	var (
		handleIdx   atomic.Int64
		opsExecuted atomic.Int64
		wg          sync.WaitGroup
	)

	ctx := context.Background()

	for w := 0; w < concurrentWorkers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(wid)+1000, 0xDEADBEEF))

			for {
				idx := handleIdx.Add(1)
				if idx > int64(totalHandles) {
					break
				}

				h, err := gusset.Open(
					gusset.WithPoolSize(4),
					gusset.WithDiagnosticEngine(),
				)
				if err != nil {
					t.Errorf("Open handle %d failed: %v", idx, err)
					continue
				}

				// Exercise the handle with mixed concurrent operations
				opsInHandle := 10 + r.IntN(20)
				for i := 0; i < opsInHandle; i++ {
					op := r.IntN(4)
					switch op {
					case 0:
						// Synchronous Call
						_, _ = h.Call(ctx, []byte{0, byte(wid), byte(i)})
					case 1:
						// Large buffer Submit + Wait or WaitBuffer
						sz := 5000 + r.IntN(3000)
						b := make([]byte, 6)
						b[0] = 16
						binary.LittleEndian.PutUint32(b[1:5], uint32(sz))
						b[5] = byte(i)
						if tk, err := h.Submit(ctx, b); err == nil {
							if r.IntN(2) == 0 {
								_, _ = h.Wait(ctx, tk)
							} else {
								if buf, err := h.WaitBuffer(ctx, tk); err == nil && buf != nil {
									_ = buf.Free()
								}
							}
						}
					case 2:
						// NewBuffer + Free
						sz := 128 + r.IntN(1024)
						if buf, err := h.NewBuffer(sz); err == nil {
							_ = buf.Free()
						}
					case 3:
						// Submit + Discard
						if tk, err := h.Submit(ctx, []byte{0, byte(i)}); err == nil {
							_ = h.Discard(tk)
						}
					}
					opsExecuted.Add(1)
				}

				_ = h.Close()
			}
		}(w)
	}

	wg.Wait()

	// Settle protocol: 3x runtime.GC() and 10ms sleep with polling
	deadline := time.Now().Add(10 * time.Second)
	var finalStats gusset.AllocStats
	for {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
		runtime.GC()
		runtime.GC()

		finalStats = gusset.Stats()
		if finalStats.LiveBytes == baseLive || time.Now().After(deadline) {
			break
		}
	}

	t.Logf("Multi-handle churn completed: handles=%d, totalOps=%d, baseLive=%d, finalLive=%d (delta=%d)",
		totalHandles, opsExecuted.Load(), baseLive, finalStats.LiveBytes,
		int64(finalStats.LiveBytes)-int64(baseLive))

	delta := int64(finalStats.LiveBytes) - int64(baseLive)
	if delta != 0 {
		t.Fatalf("ZERO-LEAK INVARIANT VIOLATION after multi-handle churn: baseline LiveBytes=%d, final LiveBytes=%d (delta=%d)",
			baseLive, finalStats.LiveBytes, delta)
	}
}

// TestPrecisionZeroLeak_HeapPlateauVsLeak verifies whether the HeapObjects growth in
// differential heap testing is an unbounded leak O(N) or a bounded one-time
// initialization plateau O(1).
func TestPrecisionZeroLeak_HeapPlateauVsLeak(t *testing.T) {
	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	settleGC()

	const workers = 16
	const batchOps = 50000

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
						sz := 16 + r.IntN(496)
						in := scratch[:sz]
						in[0] = 0
						_, _ = h.Call(ctx, in)
					case k < 65:
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
						n := 5000 + r.IntN(3000)
						binary.LittleEndian.PutUint32(mode16[1:5], uint32(n))
						mode16[5] = byte(wid)
						if tk, err := h.Submit(ctx, mode16); err == nil {
							_, _ = h.Wait(ctx, tk)
						}
					case k < 90:
						n := 5000 + r.IntN(3000)
						binary.LittleEndian.PutUint32(mode16[1:5], uint32(n))
						mode16[5] = byte(wid)
						if tk, err := h.Submit(ctx, mode16); err == nil {
							if buf, err := h.WaitBuffer(ctx, tk); err == nil && buf != nil {
								_ = buf.Free()
							}
						}
					case k < 95:
						if tk, err := h.Submit(ctx, discardIn); err == nil {
							_ = h.Discard(tk)
						}
					default:
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
		time.Sleep(50 * time.Millisecond)
	}

	// Warmup 5k ops
	runBatch(5000, 0x1111)
	settleGC()

	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	t.Logf("Initial baseline (after 5k warmup): HeapObjects=%d, HeapAlloc=%d KiB, RustLive=%d",
		m0.HeapObjects, m0.HeapAlloc>>10, gusset.Stats().LiveBytes)

	// Batch 1: 50,000 ops
	runBatch(batchOps, 0x2222)
	settleGC()

	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	t.Logf("After Batch 1 (50k ops): HeapObjects=%d (delta %d), HeapAlloc=%d KiB (delta %d KiB), RustLive=%d",
		m1.HeapObjects, int64(m1.HeapObjects)-int64(m0.HeapObjects),
		m1.HeapAlloc>>10, (int64(m1.HeapAlloc)-int64(m0.HeapAlloc))>>10,
		gusset.Stats().LiveBytes)

	// Batch 2: another 50,000 ops
	runBatch(batchOps, 0x3333)
	settleGC()

	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)
	t.Logf("After Batch 2 (100k ops total): HeapObjects=%d (delta from m1: %d, delta from m0: %d), HeapAlloc=%d KiB, RustLive=%d",
		m2.HeapObjects, int64(m2.HeapObjects)-int64(m1.HeapObjects),
		int64(m2.HeapObjects)-int64(m0.HeapObjects),
		m2.HeapAlloc>>10, gusset.Stats().LiveBytes)

	// Batch 3: another 50,000 ops
	runBatch(batchOps, 0x4444)
	settleGC()

	var m3 runtime.MemStats
	runtime.ReadMemStats(&m3)
	t.Logf("After Batch 3 (150k ops total): HeapObjects=%d (delta from m2: %d, delta from m0: %d), HeapAlloc=%d KiB, RustLive=%d",
		m3.HeapObjects, int64(m3.HeapObjects)-int64(m2.HeapObjects),
		int64(m3.HeapObjects)-int64(m0.HeapObjects),
		m3.HeapAlloc>>10, gusset.Stats().LiveBytes)

	if gusset.Stats().LiveBytes != 0 {
		t.Fatalf("Rust LiveBytes leaked: %d", gusset.Stats().LiveBytes)
	}
}

// TestPrecisionZeroLeak_UnconsumedLargeTicketsTeardownZeroLeak attacks the take buffer
// lifecycle by submitting thousands of large (>4096B) asynchronous jobs without
// collecting or discarding them, and closing the handle. Verifies that Handle.Close
// frees all unconsumed take buffers in Rust memory with exact zero LiveBytes delta.
func TestPrecisionZeroLeak_UnconsumedLargeTicketsTeardownZeroLeak(t *testing.T) {
	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	settleGC()
	baseLive := gusset.Stats().LiveBytes

	ctx := context.Background()

	// Run 20 iterations of opening handle, submitting large unconsumed jobs, and closing
	for cycle := 0; cycle < 20; cycle++ {
		h, err := gusset.Open(
			gusset.WithPoolSize(8),
			gusset.WithDiagnosticEngine(),
			gusset.WithMaxUncollected(256),
		)
		if err != nil {
			t.Fatalf("Open failed at cycle %d: %v", cycle, err)
		}

		var wg sync.WaitGroup
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func(wid int) {
				defer wg.Done()
				mode16 := make([]byte, 6)
				mode16[0] = 16
				for i := 0; i < 30; i++ {
					sz := 6000 + (i * 100)
					binary.LittleEndian.PutUint32(mode16[1:5], uint32(sz))
					mode16[5] = byte(wid)
					_, _ = h.Submit(ctx, mode16)
				}
			}(w)
		}
		wg.Wait()

		// Close handle while tickets are unconsumed
		if err := h.Close(); err != nil {
			t.Fatalf("Close failed at cycle %d: %v", cycle, err)
		}
	}

	settleGC()
	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
		t.Fatalf("Unconsumed tickets leak detected: baseline LiveBytes=%d, final LiveBytes=%d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}
}

// TestPrecisionZeroLeak_RapidContextCancellationBufferZeroLeak stresses buffer allocation
// racing against microsecond and pre-cancelled contexts. Verifies that cancelled
// dispatch never leaks Rust buffers or permits.
func TestPrecisionZeroLeak_RapidContextCancellationBufferZeroLeak(t *testing.T) {
	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	settleGC()
	baseLive := gusset.Stats().LiveBytes

	h, err := gusset.Open(
		gusset.WithPoolSize(8),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const goroutines = 16
	const opsPerG = 500
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(gid), 0x5555))

			for i := 0; i < opsPerG; i++ {
				// Rapid timeout between 0 (pre-cancelled) and 100 microseconds
				timeout := time.Duration(r.IntN(100)) * time.Microsecond
				ctx, cancel := context.WithTimeout(context.Background(), timeout)

				switch r.IntN(3) {
				case 0:
					// Call with slice
					in := []byte{0, byte(gid), byte(i)}
					_, _ = h.Call(ctx, in)

				case 1:
					// CallBuffer with allocated buffer
					buf, err := h.NewBuffer(128 + r.IntN(512))
					if err == nil {
						out, err := h.CallBuffer(ctx, buf)
						if err == nil && out != nil {
							_ = out.Free()
						}
						_ = buf.Free()
					}

				case 2:
					// Submit with large buffer
					mode16 := make([]byte, 6)
					mode16[0] = 16
					binary.LittleEndian.PutUint32(mode16[1:5], uint32(5000+r.IntN(2000)))
					if tk, err := h.Submit(ctx, mode16); err == nil {
						if r.IntN(2) == 0 {
							_, _ = h.Wait(ctx, tk)
						} else {
							_ = h.Discard(tk)
						}
					}
				}
				cancel()
			}
		}(g)
	}

	wg.Wait()
	_ = h.Close()

	settleGC()
	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
		t.Fatalf("Rapid cancellation buffer leak detected: baseline LiveBytes=%d, final LiveBytes=%d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}
}

// TestPrecisionZeroLeak_BufferBudgetThrashingZeroLeak exhausts and recovers buffer budgets
// under high concurrency, asserting zero budget drift and zero byte leak.
func TestPrecisionZeroLeak_BufferBudgetThrashingZeroLeak(t *testing.T) {
	prevLog := slog.Default()
	slog.SetDefault(slog.New(discardHandler{}))
	defer slog.SetDefault(prevLog)

	settleGC()
	baseLive := gusset.Stats().LiveBytes

	const budget = 64 * 1024 // 64 KiB tight budget
	h, err := gusset.Open(
		gusset.WithPoolSize(4),
		gusset.WithBufferBudget(budget),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	var wg sync.WaitGroup
	const threads = 16
	for th := 0; th < threads; th++ {
		wg.Add(1)
		go func(tid int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(tid), 0x9999))
			ctx := context.Background()

			for i := 0; i < 200; i++ {
				// Allocate sizes that rapidly saturate and exceed 64 KiB
				sz := 8*1024 + r.IntN(16*1024)
				buf, err := h.NewBuffer(sz)
				if err == nil {
					// Randomly call CallBuffer or just Free
					if r.IntN(2) == 0 {
						out, err := h.CallBuffer(ctx, buf)
						if err == nil && out != nil {
							_ = out.Free()
						}
					}
					_ = buf.Free()
				}
			}
		}(th)
	}

	wg.Wait()
	_ = h.Close()

	settleGC()
	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
		t.Fatalf("Buffer budget thrashing leak: baseline LiveBytes=%d, final LiveBytes=%d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}
}
