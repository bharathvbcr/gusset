package pitfalls_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	randv2 "math/rand/v2"
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

// TestHardening_NilReceiverSafety tests that calling methods on nil *Handle or
// nil *Buffer does not crash with a nil pointer dereference.
func TestHardening_NilReceiverSafety(t *testing.T) {
	var h *gusset.Handle
	ctx := context.Background()

	if err := h.Close(); err == nil {
		t.Error("expected error on (*Handle)(nil).Close(), got nil")
	}
	if _, err := h.Call(ctx, []byte{0}); err == nil {
		t.Error("expected error on (*Handle)(nil).Call(), got nil")
	}
	if _, err := h.Submit(ctx, []byte{0}); err == nil {
		t.Error("expected error on (*Handle)(nil).Submit(), got nil")
	}
	if _, err := h.Wait(ctx, 1); err == nil {
		t.Error("expected error on (*Handle)(nil).Wait(), got nil")
	}
	if _, err := h.WaitBuffer(ctx, 1); err == nil {
		t.Error("expected error on (*Handle)(nil).WaitBuffer(), got nil")
	}
	if _, err := h.NewBuffer(1024); err == nil {
		t.Error("expected error on (*Handle)(nil).NewBuffer(), got nil")
	}

	var b *gusset.Buffer
	if got := b.Bytes(); got != nil {
		t.Errorf("expected nil for (*Buffer)(nil).Bytes(), got %v", got)
	}
	if got := b.ID(); got != 0 {
		t.Errorf("expected 0 for (*Buffer)(nil).ID(), got %d", got)
	}
	if err := b.Free(); err != nil {
		t.Errorf("expected nil error for (*Buffer)(nil).Free(), got %v", err)
	}
}

// TestPitfall_NilContextIsRejected pins the boundary against a nil context.
//
// Call/Submit/Wait pass ctx to select on ctx.Done() and to callHeaderIDs and stampTimeout.
// A nil context panics there and takes the calling goroutine down, which is a
// process-level failure for a library that exists to keep the Go process alive.
func TestPitfall_NilContextIsRejected(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("nil context panicked the process: %v", rec)
		}
	}()

	//lint:ignore SA1012 a nil context is the input under test
	if _, err := h.Call(nil, []byte{0}); err == nil {
		t.Fatal("Call(nil, ...) must be rejected")
	}
	//lint:ignore SA1012 a nil context is the input under test
	if _, err := h.Submit(nil, []byte{0}); err == nil {
		t.Fatal("Submit(nil, ...) must be rejected")
	}
	//lint:ignore SA1012 a nil context is the input under test
	if _, err := h.Wait(nil, 1); err == nil {
		t.Fatal("Wait(nil, ...) must be rejected")
	}
	//lint:ignore SA1012 a nil context is the input under test
	if _, err := h.WaitBuffer(nil, 1); err == nil {
		t.Fatal("WaitBuffer(nil, ...) must be rejected")
	}
}

// TestStress_MassiveConcurrentChaos hammers Submit/Wait/WaitBuffer/Call/cancel
// and Close against one handle, then against handle churn, and fails on any
// unexpected error or hang. Poisoning is isolated: a panic byte is not mixed
// into the healthy-handle swarm, because that would turn every later error into
// ErrPoisoned and hide I4/I3 failures.
func TestStress_MassiveConcurrentChaos(t *testing.T) {
	const numGoroutines = 32
	const iterationsPerGoroutine = 20

	h, err := gusset.Open(gusset.WithPoolSize(8), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	var wg sync.WaitGroup
	var completed atomic.Int64
	var cancelled atomic.Int64

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iterationsPerGoroutine; i++ {
				switch (gid + i) % 4 {
				case 0:
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					res, err := h.Call(ctx, []byte{0, byte(gid), byte(i)})
					cancel()
					if err != nil {
						if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
							cancelled.Add(1)
							return
						}
						t.Errorf("Call: %v", err)
						return
					}
					if len(res) < 3 || res[1] != byte(gid) || res[2] != byte(i) {
						t.Errorf("corrupted Call response: %v", res)
						return
					}
					completed.Add(1)

				case 1:
					buf, err := h.NewBuffer(512)
					if err != nil {
						t.Errorf("NewBuffer: %v", err)
						return
					}
					b := buf.Bytes()
					b[0] = 0
					b[1] = byte(gid)
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					ticket, err := h.Submit(ctx, buf)
					if err != nil {
						cancel()
						_ = buf.Free()
						t.Errorf("Submit buffer: %v", err)
						return
					}
					res, err := h.Wait(ctx, ticket)
					cancel()
					_ = buf.Free()
					if err != nil {
						if errors.Is(err, context.DeadlineExceeded) {
							cancelled.Add(1)
							return
						}
						t.Errorf("Wait: %v", err)
						return
					}
					if len(res) < 2 || res[1] != byte(gid) {
						t.Errorf("corrupted buffer echo: %v", res)
						return
					}
					completed.Add(1)

				case 2:
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					ticket, err := h.Submit(ctx, []byte{0, 42, 99})
					if err != nil {
						cancel()
						t.Errorf("Submit: %v", err)
						return
					}
					outBuf, err := h.WaitBuffer(ctx, ticket)
					cancel()
					if err != nil {
						if errors.Is(err, context.DeadlineExceeded) {
							cancelled.Add(1)
							return
						}
						t.Errorf("WaitBuffer: %v", err)
						return
					}
					got := outBuf.Bytes()
					if len(got) != 3 || got[1] != 42 || got[2] != 99 {
						t.Errorf("corrupted WaitBuffer: %v", got)
					}
					if err := outBuf.Free(); err != nil {
						t.Errorf("WaitBuffer Free: %v", err)
					}
					completed.Add(1)

				case 3:
					ctx, cancel := context.WithTimeout(context.Background(), 500*time.Microsecond)
					_, err := h.Call(ctx, []byte{5, 8})
					cancel()
					if err != nil {
						cancelled.Add(1)
					} else {
						completed.Add(1)
					}
				}
			}
		}(g)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("chaos swarm hung")
	}

	t.Logf("chaos: completed=%d cancelled=%d", completed.Load(), cancelled.Load())
	if completed.Load() == 0 {
		t.Fatal("chaos completed no successful calls")
	}

	// Contract probes mixed into the same handle after the swarm: these fail
	// closed rather than hang or copy a megabyte on the cgo thread.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := h.Call(ctx, make([]byte, 4097)); err == nil {
		t.Fatal("chaos: 4097-byte []byte must be refused (R16)")
	}
	if _, err := h.Wait(ctx, 1<<40); !errors.Is(err, gusset.ErrUnknownTicket) {
		t.Fatalf("chaos: Wait on a never-issued ticket must be ErrUnknownTicket, got %v", err)
	}
}

// TestHardening_ConcurrentSubmitClosePermitIntegrity races Submit/Wait against
// Close across handle lifecycles. The assertion is that nothing hangs: Close
// must drain every permit it still owns, and a waiter must not block forever
// on a handle that has already shut its completion pipe.
func TestHardening_ConcurrentSubmitClosePermitIntegrity(t *testing.T) {
	for trial := 0; trial < 10; trial++ {
		h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("trial %d: Open failed: %v", trial, err)
		}

		var wg sync.WaitGroup
		const submitters = 20
		hung := atomic.Bool{}
		for s := 0; s < submitters; s++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 25; i++ {
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					ticket, submitErr := h.Submit(ctx, []byte{0, 1, 2})
					if submitErr == nil {
						_, _ = h.Wait(ctx, ticket)
					}
					cancel()
				}
			}()
		}

		time.Sleep(2 * time.Millisecond)
		if err := h.Close(); err != nil {
			t.Fatalf("trial %d: Close failed: %v", trial, err)
		}

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			hung.Store(true)
		}
		if hung.Load() {
			t.Fatalf("trial %d: Submit/Wait hung after Close", trial)
		}
	}
}

// TestPitfall_WaitBufferOfLargeResultDoesNotDangleAcrossClose is the large-result
// twin of TestStress_ConcurrentZeroCopyEgressAndCloseRace.
//
// That test submits 3-byte payloads, so drainPipe copies onto the Go heap and
// WaitBuffer allocates a fresh Buffer. The take-buffer path (R16 egress, >4 KiB)
// stores a Go slice over Rust memory in completed/takeIDs and wraps it later
// without holding cgoMu. Close can HandleClose that memory between waitInternal
// returning and newBufferFromRaw, and WaitBuffer then returns (buf, nil) whose
// Bytes() is nil — success wrapping freed pages. The existing close-race used
// payloads too small to take that path.
func TestPitfall_WaitBufferOfLargeResultDoesNotDangleAcrossClose(t *testing.T) {
	const payload = 8192
	for trial := 0; trial < 8; trial++ {
		h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("trial %d: Open failed: %v", trial, err)
		}

		const submitters = 16
		var wg sync.WaitGroup
		var badSuccess atomic.Int64
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)

		for s := 0; s < submitters; s++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				in, err := h.NewBuffer(payload)
				if err != nil {
					return
				}
				defer in.Free()
				b := in.Bytes()
				if b == nil {
					return
				}
				b[0] = 0
				for i := 1; i < len(b); i++ {
					b[i] = byte(i)
				}
				ticket, err := h.Submit(ctx, in)
				if err != nil {
					return
				}
				out, err := h.WaitBuffer(ctx, ticket)
				if err != nil {
					return
				}
				defer out.Free()
				got := out.Bytes()
				// nil Bytes after a successful wrap is Close winning the API
				// contract (handle gone → view withdrawn). The bug is a
				// non-nil view of the wrong length or torn contents: that is
				// a wrap of already-freed pages, which the 3-byte close-race
				// never reached because drainPipe copied those onto the Go heap.
				if got == nil {
					return
				}
				if len(got) != payload {
					badSuccess.Add(1)
					t.Errorf("WaitBuffer succeeded with a truncated view: len=%d", len(got))
					return
				}
				first := got[100]
				// This test used to read got after out.Free() and while Close
				// ran, which the Bytes contract forbids: it measured its own
				// use-after-free (about 1 run in 30, on main as well). Close
				// sets closed before it frees anything, so a non-nil Bytes()
				// after the read proves the read saw live memory; only then
				// is a wrong byte the wrap-of-freed-pages bug this test hunts.
				if out.Bytes() == nil {
					return
				}
				if first != byte(100) {
					badSuccess.Add(1)
					t.Errorf("WaitBuffer succeeded with corrupted bytes: got[100]=%d", first)
				}
			}()
		}

		time.Sleep(time.Duration(2+trial) * time.Millisecond)
		_ = h.Close()
		cancel()

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Fatalf("trial %d: WaitBuffer hung after Close", trial)
		}
		if badSuccess.Load() > 0 {
			t.Fatalf("trial %d: WaitBuffer returned success wrapping freed Rust memory", trial)
		}
	}
}

// TestPitfall_BufferSizeIsBounded pins the allocation ceiling.
//
// NewBuffer used to feed the caller's int straight into posix_memalign. A
// production caller that passed 1<<40 either truncated or asked the OS for
// more memory than the process can hold. The ceiling is refused, not clamped,
// matching WithPoolSize.
func TestPitfall_BufferSizeIsBounded(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	if _, err := h.NewBuffer(gusset.MaxBufferBytes + 1); err == nil {
		t.Fatal("NewBuffer above MaxBufferBytes must be refused, not allocated")
	} else if !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("refusal must name the maximum, got: %v", err)
	}

	buf, err := h.NewBuffer(64)
	if err != nil {
		t.Fatalf("NewBuffer(64) must still succeed, got %v", err)
	}
	if err := buf.Free(); err != nil {
		t.Fatalf("Free failed: %v", err)
	}
}

// TestConcurrentChaos_Invariants_I1_to_I6 executes a multi-threaded chaos swarm
// over 3 seconds with randomized calls, mode 16 take buffers, discards, microsecond
// cancellations, mode 17 non-cooperative engine holds, panics, allocations, and
// concurrent handle rotation, asserting Invariants I1–I6 under the race detector.
func TestConcurrentChaos_Invariants_I1_to_I6(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrent chaos in short mode")
	}

	baselineLive := gusset.Stats().LiveBytes
	baselineAlloc := gusset.Stats().AllocCount

	const (
		poolSize      = 4
		numWorkers    = 32
		chaosDuration = 3 * time.Second
		bufferSize    = 8192 // Mode 16 large take buffer (>4096 MAX_INLINE_INPUT)
	)

	// Probe handle to reset Mode 17/18 in-engine peak gauge (Invariant I1)
	probe, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open probe failed: %v", err)
	}
	defer probe.Close()
	// Reset gauge peak
	_, _ = probe.Call(context.Background(), []byte{18})

	// Shared active handle holder with concurrent rotation
	type handleHolder struct {
		mu sync.RWMutex
		h  *gusset.Handle
	}
	newHandle := func() *gusset.Handle {
		h, err := gusset.Open(
			gusset.WithPoolSize(poolSize),
			gusset.WithDiagnosticEngine(),
			gusset.WithMaxUncollected(1024),
		)
		if err != nil {
			panic(err)
		}
		return h
	}

	holder := &handleHolder{h: newHandle()}

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	var (
		opCallsCompleted   atomic.Int64
		opSubmitsCompleted atomic.Int64
		opCancelled        atomic.Int64
		opDiscards         atomic.Int64
		opClosedHandled    atomic.Int64
		opPanicsCaught     atomic.Int64
		handleChurnCount   atomic.Int64
		mode17Submits      atomic.Int64
	)

	// Mode 16 payload: [16, len_le_u32, seed]
	makeMode16Payload := func(seed byte) []byte {
		buf := make([]byte, 6)
		buf[0] = 16
		binary.LittleEndian.PutUint32(buf[1:5], uint32(bufferSize))
		buf[5] = seed
		return buf
	}

	// 1. Concurrent Chaos Worker Swarm
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			r := randv2.New(randv2.NewPCG(uint64(workerID)+100, 54321))

			for {
				select {
				case <-stopCh:
					return
				default:
				}

				holder.mu.RLock()
				curH := holder.h
				holder.mu.RUnlock()
				if curH == nil {
					time.Sleep(1 * time.Millisecond)
					continue
				}

				op := r.IntN(8)
				switch op {
				case 0:
					// Normal synchronous Call (Mode 0 echo)
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					res, err := curH.Call(ctx, []byte{0, byte(workerID), byte(op)})
					cancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							opClosedHandled.Add(1)
						} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
							opCancelled.Add(1)
						}
					} else {
						if len(res) >= 3 && res[1] == byte(workerID) {
							opCallsCompleted.Add(1)
						}
					}

				case 1:
					// Large Take Buffer Submit + WaitBuffer + Free (Invariant I3)
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					tk, err := curH.Submit(ctx, makeMode16Payload(byte(workerID)))
					cancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							opClosedHandled.Add(1)
						}
						continue
					}
					wCtx, wCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					buf, err := curH.WaitBuffer(wCtx, tk)
					wCancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) || errors.Is(err, gusset.ErrUnknownTicket) {
							opClosedHandled.Add(1)
						} else if errors.Is(err, context.DeadlineExceeded) {
							opCancelled.Add(1)
						}
					} else {
						opSubmitsCompleted.Add(1)
						if buf != nil {
							_ = buf.Free()
						}
					}

				case 2:
					// Submit + Discard (Invariant I4 permit return)
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					tk, err := curH.Submit(ctx, []byte{0, 42})
					cancel()
					if err != nil {
						if errors.Is(err, gusset.ErrClosed) {
							opClosedHandled.Add(1)
						}
						continue
					}
					if err := curH.Discard(tk); err == nil {
						opDiscards.Add(1)
					} else if errors.Is(err, gusset.ErrClosed) {
						opClosedHandled.Add(1)
					}

				case 3:
					// Microsecond timeout Call (triggering early cancellation)
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Microsecond)
					_, err := curH.Call(ctx, []byte{5, 20}) // Mode 5 cooperative sleep
					cancel()
					if err != nil {
						opCancelled.Add(1)
					}

				case 4:
					// Non-cooperative engine hold (Mode 17) verifying Invariant I1
					ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
					// Mode 17 holds in engine for 3 ms
					tk, err := curH.Submit(ctx, []byte{17, 3})
					cancel()
					if err != nil {
						// The handle may be mid-rotation or the pool full; anything else is a bug.
						if !errors.Is(err, gusset.ErrClosed) && !errors.Is(err, context.DeadlineExceeded) &&
							!errors.Is(err, context.Canceled) && !errors.Is(err, gusset.ErrTooManyUncollected) {
							t.Errorf("mode 17 Submit: unexpected error: %v", err)
						}
					} else {
						mode17Submits.Add(1)
						wCtx, wCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
						_, werr := curH.Wait(wCtx, tk)
						wCancel()
						if werr != nil && (errors.Is(werr, context.DeadlineExceeded) || errors.Is(werr, context.Canceled)) {
							opCancelled.Add(1)
						} else if werr == nil {
							opSubmitsCompleted.Add(1)
						}
					}

				case 5:
					// Ephemeral handle panic containment test (Invariant I2)
					panickyHandle, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
					if err == nil {
						pCtx, pCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
						// Mode 1: plain panic, or Mode 15: double panic DropBomb
						panicMode := byte(1)
						if workerID%2 == 0 {
							panicMode = 15
						}
						_, perr := panickyHandle.Call(pCtx, []byte{panicMode, 2})
						pCancel()
						if perr != nil && (errors.Is(perr, gusset.ErrPanic) || errors.Is(perr, gusset.ErrPoisoned)) {
							opPanicsCaught.Add(1)
						}
						_ = panickyHandle.Close()
					}

				case 6:
					// NewBuffer allocation and concurrent free stress
					buf, err := curH.NewBuffer(1024)
					if err == nil {
						b := buf.Bytes()
						if len(b) > 0 {
							b[0] = 0xAA
						}
						_ = buf.Free()
					}

				case 7:
					// Fast inline Submit + Wait
					ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
					tk, err := curH.Submit(ctx, []byte{0, 1, 2, 3})
					cancel()
					if err == nil {
						wCtx, wCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
						res, werr := curH.Wait(wCtx, tk)
						wCancel()
						if werr == nil && len(res) >= 4 {
							opSubmitsCompleted.Add(1)
						} else if errors.Is(werr, gusset.ErrClosed) {
							opClosedHandled.Add(1)
						}
					}
				}
			}
		}(i)
	}

	// 2. Handle Lifecycle Churner (concurrent rotation and close)
	churnStop := make(chan struct{})
	var churnWg sync.WaitGroup
	churnWg.Add(1)
	go func() {
		defer churnWg.Done()
		ticker := time.NewTicker(60 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-churnStop:
				return
			case <-ticker.C:
				holder.mu.Lock()
				oldH := holder.h
				// Close old handle cleanly before swapping, ensuring strict peak gauge bounding
				if oldH != nil {
					_ = oldH.Close() // Invariant I4: must close cleanly without hang
				}
				holder.h = newHandle()
				holder.mu.Unlock()

				handleChurnCount.Add(1)
			}
		}
	}()

	time.Sleep(chaosDuration)

	// Stop churner
	close(churnStop)
	churnWg.Wait()

	// Stop workers
	close(stopCh)
	wg.Wait()

	// Close final handle
	holder.mu.Lock()
	if holder.h != nil {
		_ = holder.h.Close()
		holder.h = nil
	}
	holder.mu.Unlock()

	// Invariant Assertions:

	// Invariant I1: Read Mode 18 in-engine peak gauge
	res, err := probe.Call(context.Background(), []byte{18})
	if err != nil {
		t.Fatalf("Failed to read Mode 18 gauge: %v", err)
	}
	peakInRust := binary.LittleEndian.Uint64(res)
	t.Logf("Invariant I1: Peak in-engine concurrent jobs = %d (configured poolSize = %d)", peakInRust, poolSize)
	if peakInRust > poolSize {
		t.Fatalf("INVARIANT I1 VIOLATION: peak concurrent jobs in Rust (%d) exceeded poolSize (%d)", peakInRust, poolSize)
	}
	// An upper bound alone passes with a dead gauge (peak 0). Mode 17 must have
	// run, and the engine must have seen at least two of its jobs at once.
	if mode17Submits.Load() == 0 {
		t.Fatalf("no mode 17 job was admitted, so the I1 gauge was never exercised")
	}
	if peakInRust < 2 {
		t.Fatalf("I1 gauge saw a peak of %d after %d mode 17 submits on a %d-worker pool; want at least 2 (dead gauge or no concurrency)",
			peakInRust, mode17Submits.Load(), poolSize)
	}

	// Invariant I2: Assert panics were caught
	t.Logf("Invariant I2: Caught panics on ephemeral handles = %d", opPanicsCaught.Load())
	if opPanicsCaught.Load() == 0 {
		t.Errorf("INVARIANT I2: no panic tests executed during chaos run")
	}

	// Invariant I6 & I3: Assert Stats monotonicity and zero leak
	finalStats := gusset.Stats()
	t.Logf("Chaos Stats Summary: Calls=%d, Submits=%d, Cancelled=%d, Discards=%d, Closed=%d, Churns=%d",
		opCallsCompleted.Load(), opSubmitsCompleted.Load(), opCancelled.Load(),
		opDiscards.Load(), opClosedHandled.Load(), handleChurnCount.Load())
	t.Logf("Memory Stats: LiveBytes baseline=%d final=%d (delta=%d), Allocs baseline=%d final=%d",
		baselineLive, finalStats.LiveBytes, int64(finalStats.LiveBytes)-int64(baselineLive),
		baselineAlloc, finalStats.AllocCount)

	if finalStats.PeakBytes < finalStats.LiveBytes {
		t.Errorf("INVARIANT I6 VIOLATION: PeakBytes (%d) < LiveBytes (%d)", finalStats.PeakBytes, finalStats.LiveBytes)
	}
	if finalStats.AllocCount < baselineAlloc {
		t.Errorf("INVARIANT I6 VIOLATION: AllocCount decreased: baseline %d, final %d", baselineAlloc, finalStats.AllocCount)
	}
	if finalStats.LiveBytes != baselineLive {
		t.Fatalf("INVARIANT I3/I6 VIOLATION: Memory leak detected! baseline LiveBytes=%d, final LiveBytes=%d (leaked %d bytes)",
			baselineLive, finalStats.LiveBytes, int64(finalStats.LiveBytes)-int64(baselineLive))
	}
}

// TestConcurrentChaos_ConcurrentCloseRaces tests that rapid concurrent Close() calls
// against in-flight operations never deadlock and return identical results (Invariant I4).
func TestConcurrentChaos_ConcurrentCloseRaces(t *testing.T) {
	for trial := 0; trial < 10; trial++ {
		h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatalf("trial %d: Open failed: %v", trial, err)
		}

		var wg sync.WaitGroup
		const callers = 16
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				_, _ = h.Call(ctx, []byte{0, 1})
			}()
		}

		// Concurrently invoke Close() from 4 distinct goroutines
		const closers = 4
		closeResults := make([]error, closers)
		var closeWg sync.WaitGroup
		for c := 0; c < closers; c++ {
			closeWg.Add(1)
			go func(idx int) {
				defer closeWg.Done()
				closeResults[idx] = h.Close()
			}(c)
		}

		closeDone := make(chan struct{})
		go func() {
			closeWg.Wait()
			wg.Wait()
			close(closeDone)
		}()

		select {
		case <-closeDone:
		case <-time.After(3 * time.Second):
			t.Fatalf("trial %d: concurrent Close deadlock detected", trial)
		}

		// Assert all closers received identical error (either nil or same error)
		for c := 1; c < closers; c++ {
			if closeResults[c] != closeResults[0] {
				t.Fatalf("trial %d: closer %d returned %v, while closer 0 returned %v",
					trial, c, closeResults[c], closeResults[0])
			}
		}
	}
}

// TestConcurrentChaos_CooperativeCancellationStorm tests a storm of rapid context
// cancellations, verifying permit conservation and zero memory leak.
func TestConcurrentChaos_CooperativeCancellationStorm(t *testing.T) {
	baseLive := gusset.Stats().LiveBytes
	const poolSize = 8
	h, err := gusset.Open(
		gusset.WithPoolSize(poolSize),
		gusset.WithDiagnosticEngine(),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	const callers = 64
	const iterations = 50
	var wg sync.WaitGroup

	for c := 0; c < callers; c++ {
		wg.Add(1)
		go func(cid int) {
			defer wg.Done()
			r := randv2.New(randv2.NewPCG(uint64(cid)+500, 11111))
			for i := 0; i < iterations; i++ {
				// Random microsecond timeout between 10µs and 500µs
				timeout := time.Duration(10+r.IntN(490)) * time.Microsecond
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				// Mode 5 cooperative sleep
				_, _ = h.Call(ctx, []byte{5, 10})
				cancel()
			}
		}(c)
	}

	wg.Wait()

	// Verify permit conservation:
	// All poolSize permits must be available and functional
	for i := 0; i < poolSize; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		res, err := h.Call(ctx, []byte{0, byte(i)})
		cancel()
		if err != nil {
			t.Fatalf("permit check %d failed: %v", i, err)
		}
		if len(res) < 2 || res[1] != byte(i) {
			t.Fatalf("permit check %d returned invalid result: %v", i, res)
		}
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	finalLive := gusset.Stats().LiveBytes
	if finalLive != baseLive {
		t.Fatalf("Cancellation storm leaked memory: baseline %d, final %d (delta %d)",
			baseLive, finalLive, int64(finalLive)-int64(baseLive))
	}
}

// TestStress_ContainmentUnderLoad drives every failure the worker must contain
// at once, with callers that have NO deadline.
//
// A caller with context.Background() is the one a stranded ticket hurts most:
// nothing but a completion can release it. So every call here is deadline-free
// and the whole test runs under a watchdog. The mix includes panics whose
// payload destructor panics again (mode 15, direct and delayed), plain and
// delayed panics beside slow successes (a result that finishes after a
// sibling poisoned the handle must still be delivered intact), large
// zero-copy round trips, handles closed while work is in flight, and child
// processes forked continuously so any descriptor that lacks close-on-exec is
// inherited mid-Close.
//
// Invariants:
//   - every call returns before the watchdog fires;
//   - every error is ErrPanic, ErrPoisoned, or a closed-handle error;
//   - every success is byte-for-byte the expected output;
//   - goroutines and Rust-owned live bytes return to baseline afterwards.
func TestStress_ContainmentUnderLoad(t *testing.T) {
	rounds := 40
	if testing.Short() {
		rounds = 8
	}
	if v, err := strconv.Atoi(os.Getenv("GUSSET_STRESS_ROUNDS")); err == nil && v > 0 {
		rounds = v
	}
	const callers = 16
	const callsPerCaller = 8

	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()
	baseLive := gusset.Stats().LiveBytes

	// Continuous fork/exec for the whole run.
	stopForks := make(chan struct{})
	var forks sync.WaitGroup
	var forked atomic.Int64
	forks.Add(1)
	go func() {
		defer forks.Done()
		for {
			select {
			case <-stopForks:
				return
			default:
			}
			if err := exec.Command("true").Run(); err == nil {
				forked.Add(1)
			}
		}
	}()

	var (
		successes, panics, poisoned, closed atomic.Int64
		failures                            = make(chan string, 64)
	)
	fail := func(format string, args ...any) {
		select {
		case failures <- fmt.Sprintf(format, args...):
		default:
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for round := 0; round < rounds; round++ {
			rng := rand.New(rand.NewSource(int64(round)))
			h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
			if err != nil {
				fail("round %d: Open: %v", round, err)
				return
			}

			// A slice from Bytes is valid only until Close (see Buffer.Bytes),
			// and the mid-flight Close below frees every buffer of h. Callers
			// hold guard's read side while they touch a slice; Close takes the
			// write side. Every gusset call still races Close freely.
			var guard sync.RWMutex
			var wg sync.WaitGroup
			for c := 0; c < callers; c++ {
				seed := rng.Int63()
				wg.Add(1)
				go func() {
					defer wg.Done()
					r := rand.New(rand.NewSource(seed))
					for i := 0; i < callsPerCaller; i++ {
						runOne(h, &guard, r, fail, &successes, &panics, &poisoned, &closed)
					}
				}()
			}

			// Half the rounds close while calls are still in flight.
			if round%2 == 0 {
				time.Sleep(time.Duration(rng.Intn(40)) * time.Millisecond)
				guard.Lock()
				err := h.Close()
				guard.Unlock()
				if err != nil {
					fail("round %d: Close mid-flight: %v", round, err)
				}
				wg.Wait()
			} else {
				wg.Wait()
				if err := h.Close(); err != nil {
					fail("round %d: Close: %v", round, err)
				}
			}
		}
	}()

	watchdog := time.Duration(rounds)*3*time.Second + 30*time.Second
	select {
	case <-done:
	case <-time.After(watchdog):
		close(stopForks)
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("stress run did not finish within %v: a deadline-free call was stranded\n%s", watchdog, buf[:n])
	}
	close(stopForks)
	forks.Wait()

	close(failures)
	for f := range failures {
		t.Error(f)
	}

	// Leak checks: goroutines and Rust-owned buffers return to baseline.
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		g := runtime.NumGoroutine()
		live := gusset.Stats().LiveBytes
		if g <= baseGoroutines+2 && live <= baseLive {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("leak after all handles closed: goroutines %d (baseline %d), Rust live bytes %d (baseline %d)",
				g, baseGoroutines, live, baseLive)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Logf("rounds=%d calls=%d success=%d panic=%d poisoned=%d closed=%d forks=%d",
		rounds, rounds*callers*callsPerCaller, successes.Load(), panics.Load(),
		poisoned.Load(), closed.Load(), forked.Load())
	if successes.Load() == 0 || panics.Load() == 0 {
		t.Errorf("mix did not exercise both outcomes: success=%d panic=%d", successes.Load(), panics.Load())
	}
}

// runOne issues one deadline-free call of a randomly chosen shape and checks
// its outcome.
func runOne(h *gusset.Handle, guard *sync.RWMutex, r *rand.Rand, fail func(string, ...any),
	successes, panics, poisoned, closed *atomic.Int64) {
	ctx := context.Background()

	var (
		out    []byte
		err    error
		want   []byte
		mayErr bool // outcome may legitimately be a panic
	)

	switch k := r.Intn(20); {
	case k < 6: // small echo
		in := []byte{0, byte(r.Intn(256)), byte(r.Intn(256))}
		want = in
		out, err = h.Call(ctx, in)
	case k < 11: // slow small echo: may finish after a sibling poisons the handle
		in := []byte{9, byte(1 + r.Intn(5)), 0, byte(r.Intn(256)), 7}
		want = in[2:]
		out, err = h.Call(ctx, in)
	case k == 11: // plain panic
		mayErr = true
		_, err = h.Call(ctx, []byte{1})
	case k == 12: // payload destructor panics, re-thrown up to 8 times
		mayErr = true
		_, err = h.Call(ctx, []byte{15, byte(r.Intn(10))})
	case k == 13: // delayed destructor bomb
		mayErr = true
		_, err = h.Call(ctx, []byte{9, byte(1 + r.Intn(5)), 15, byte(r.Intn(3))})
	case k == 14: // delayed plain panic
		mayErr = true
		_, err = h.Call(ctx, []byte{9, byte(1 + r.Intn(5)), 1})
	default: // large zero-copy round trip
		in, berr := h.NewBuffer(5000)
		if berr != nil {
			err = berr
			break
		}
		guard.RLock()
		b := in.Bytes()
		if b == nil { // closed underneath us
			guard.RUnlock()
			_ = in.Free()
			closed.Add(1)
			return
		}
		b[0] = 0
		for j := 1; j < len(b); j++ {
			b[j] = byte(j ^ int(r.Int31()))
		}
		want = append([]byte(nil), b...)
		guard.RUnlock()
		res, cerr := h.CallBuffer(ctx, in)
		_ = in.Free()
		err = cerr
		if cerr == nil {
			guard.RLock()
			rb := res.Bytes()
			if rb != nil {
				out = append([]byte(nil), rb...)
			}
			guard.RUnlock()
			if rb == nil {
				_ = res.Free()
				closed.Add(1)
				return
			}
			_ = res.Free()
		}
	}

	switch {
	case err == nil:
		if mayErr {
			fail("panicking call returned success")
			return
		}
		if !bytes.Equal(out, want) {
			fail("result corrupted: got %d bytes, want %d", len(out), len(want))
			return
		}
		successes.Add(1)
	case errors.Is(err, gusset.ErrPanic):
		panics.Add(1)
	case errors.Is(err, gusset.ErrPoisoned):
		poisoned.Add(1)
	case strings.Contains(err.Error(), "closed"):
		closed.Add(1)
	default:
		fail("untyped error: %v", err)
	}
}
