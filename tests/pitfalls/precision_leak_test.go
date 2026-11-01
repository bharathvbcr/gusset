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
