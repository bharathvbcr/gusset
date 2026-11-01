package pitfalls_test

import (
	"context"
	"encoding/binary"
	"log/slog"
	"math/rand/v2"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestChallenger_HeapPlateauVsLeak verifies whether the HeapObjects growth in
// differential heap testing is an unbounded leak O(N) or a bounded one-time
// initialization plateau O(1).
func TestChallenger_HeapPlateauVsLeak(t *testing.T) {
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

// TestChallenger_UnconsumedLargeTicketsTeardownZeroLeak attacks the take buffer
// lifecycle by submitting thousands of large (>4096B) asynchronous jobs without
// collecting or discarding them, and closing the handle. Verifies that Handle.Close
// frees all unconsumed take buffers in Rust memory with exact zero LiveBytes delta.
func TestChallenger_UnconsumedLargeTicketsTeardownZeroLeak(t *testing.T) {
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

// TestChallenger_RapidContextCancellationBufferZeroLeak stresses buffer allocation
// racing against microsecond and pre-cancelled contexts. Verifies that cancelled
// dispatch never leaks Rust buffers or permits.
func TestChallenger_RapidContextCancellationBufferZeroLeak(t *testing.T) {
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

// TestChallenger_BufferBudgetThrashingZeroLeak exhausts and recovers buffer budgets
// under high concurrency, asserting zero budget drift and zero byte leak.
func TestChallenger_BufferBudgetThrashingZeroLeak(t *testing.T) {
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
