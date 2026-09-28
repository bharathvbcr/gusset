package gusset

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSoak_SingleHandleBookkeepingStaysFlat runs a long mix of Call,
// Submit/Wait, WaitBuffer and abandoned deadlines on ONE handle that is never
// closed, and at every checkpoint — with nothing in flight — requires every
// bookkeeping map to be empty and the heap, Rust live bytes and goroutine
// count to be where the first checkpoint left them.
//
// The hammer checks bookkeeping only after Close, so anything Close sweeps up
// (a completed entry, a nil collecting marker, a take id, an abandoned ticket
// whose completion was never matched) is invisible to it. A handle that lives
// for weeks never gets that sweep: a per-ticket leak there is unbounded growth.
//
// GUSSET_SOAK sets the total operations (default 40000; the audit ran 2000000).
func TestSoak_SingleHandleBookkeepingStaysFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("soak skipped in -short mode")
	}
	total := 40000
	if v := os.Getenv("GUSSET_SOAK"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("GUSSET_SOAK=%q: want a positive operation count", v)
		}
		total = n
	}
	const workers, checkpoints = 8, 8
	perPhase := total / checkpoints / workers

	h, err := Open(WithPoolSize(4), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state

	var ops [5]atomic.Int64 // call, submitWaitLarge, completedPath, waitBuffer, abandon
	var bad, lateResults atomic.Int64
	fail := func(format string, args ...any) {
		if bad.Add(1) <= 10 {
			t.Errorf(format, args...)
		}
	}

	var inputsMu sync.Mutex
	var inputs []*Buffer
	worker := func(r *rand.Rand) {
		for i := 0; i < perPhase; i++ {
			switch k := r.IntN(100); {
			case k < 55: // Call: inline record, or a take copied onto the Go heap
				n := r.IntN(64)
				if r.IntN(8) == 0 {
					n = 64 + r.IntN(inlineResultBytes-64)
				}
				in := make([]byte, n+1)
				for j := 1; j < len(in); j++ {
					in[j] = byte(r.Uint32())
				}
				out, err := h.Call(context.Background(), in)
				if err != nil || !bytes.Equal(out, in) {
					fail("Call(%d bytes): %d bytes, %v", len(in), len(out), err)
				}
				ops[0].Add(1)
			case k < 75: // Submit + Wait of a large result: a take buffer id in takeIDs
				n, seed := 4097+r.IntN(12000), byte(r.Uint32())
				tk, err := h.Submit(context.Background(), mode16(n, seed))
				if err != nil {
					fail("Submit: %v", err)
					continue
				}
				out, err := h.Wait(context.Background(), tk)
				if err != nil || !generated(out, n, seed) {
					fail("Wait(large %d): %d bytes, %v", n, len(out), err)
				}
				ops[1].Add(1)
			case k < 88: // the completed-map path: the result lands before Wait
				in := []byte{0, byte(r.Uint32()), 3}
				if r.IntN(2) == 0 {
					in = mode16(4097+r.IntN(4000), 7)
				}
				tk, err := h.Submit(context.Background(), in)
				if err != nil {
					fail("Submit: %v", err)
					continue
				}
				time.Sleep(50 * time.Microsecond)
				if _, err := h.Wait(context.Background(), tk); err != nil {
					fail("Wait(completed): %v", err)
				}
				if _, err := h.Wait(context.Background(), tk); !errors.Is(err, ErrUnknownTicket) {
					fail("second Wait: %v", err)
				}
				ops[2].Add(1)
			case k < 99: // WaitBuffer of a large result, freed by the caller
				n := 4097 + r.IntN(8000)
				tk, err := h.Submit(context.Background(), mode16(n, 1))
				if err != nil {
					fail("Submit: %v", err)
					continue
				}
				buf, err := h.WaitBuffer(context.Background(), tk)
				if err != nil || !generated(buf.Bytes(), n, 1) {
					fail("WaitBuffer: %v", err)
				}
				if buf != nil {
					_ = buf.Free()
				}
				ops[3].Add(1)
			default: // abandon: a 1 ms deadline on a 20 ms non-cooperative job
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				var err error
				var out, want []byte
				start := time.Now()
				if r.IntN(2) == 0 {
					want = []byte{0, 1, 2, 3}
					out, err = h.Call(ctx, []byte{9, 2, 0, 1, 2, 3})
				} else {
					// A large abandoned result: a take buffer deliver must free.
					// The input stays allocated until the phase ends — the job
					// still reads it after the caller has gone.
					in, aerr := h.NewBuffer(8000)
					if aerr != nil {
						fail("NewBuffer: %v", aerr)
						cancel()
						continue
					}
					b := in.Bytes()
					for j := range b {
						b[j] = byte(j * 5)
					}
					b[0], b[1], b[2] = 9, 2, 0
					want = append([]byte(nil), b[2:]...)
					inputsMu.Lock()
					inputs = append(inputs, in)
					inputsMu.Unlock()
					var tk uint64
					if tk, err = h.Submit(ctx, in); err == nil {
						out, err = h.Wait(ctx, tk)
					}
				}
				cancel()
				// A result can win only when the deadline's timer fired late
				// (the job takes 20 ms); then it must be the right result.
				if err == nil {
					lateResults.Add(1)
					if el := time.Since(start); el < 20*time.Millisecond || !bytes.Equal(out, want) {
						fail("abandon: nil error after %v with %d bytes (want %d, equal %v)", el, len(out), len(want), bytes.Equal(out, want))
					}
				}
				ops[4].Add(1)
			}
		}
	}

	type sample struct {
		heap, live      uint64
		goroutines, ops int
	}
	var samples []sample
	quiesce := func() {
		deadline := time.Now().Add(10 * time.Second)
		for {
			s.mu.Lock()
			n := len(s.semTickets) + len(s.pending) + len(s.completed) + len(s.abandoned) + len(s.takeIDs)
			m := fmt.Sprintf("sem %d semTickets %d pending %d completed %d abandoned %d takeIDs %d",
				len(s.sem), len(s.semTickets), len(s.pending), len(s.completed), len(s.abandoned), len(s.takeIDs))
			s.mu.Unlock()
			if n == 0 && len(s.sem) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("bookkeeping left behind on an open, idle handle: %s", m)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	measure := func(done int) {
		quiesce()
		for _, in := range inputs {
			_ = in.Free()
		}
		inputs = nil
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		samples = append(samples, sample{ms.HeapInuse, Stats().LiveBytes, runtime.NumGoroutine(), done})
	}

	seed := uint64(time.Now().UnixNano())
	t.Logf("seed %d, %d ops over %d workers", seed, perPhase*workers*checkpoints, workers)
	measure(0)
	for c := 0; c < checkpoints; c++ {
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(r *rand.Rand) { defer wg.Done(); worker(r) }(rand.New(rand.NewPCG(seed, uint64(c*workers+w))))
		}
		wg.Wait()
		measure((c + 1) * perPhase * workers)
	}

	for _, sm := range samples {
		t.Logf("after %8d ops: HeapInuse %6d KiB, Rust live %6d KiB, goroutines %d", sm.ops, sm.heap>>10, sm.live>>10, sm.goroutines)
	}
	t.Logf("call %d submit+wait(large) %d completed-path %d waitbuffer %d abandon-attempts %d (%d got the result: the 1 ms timer fired after the 20 ms job)",
		ops[0].Load(), ops[1].Load(), ops[2].Load(), ops[3].Load(), ops[4].Load(), lateResults.Load())

	// Compare against the first loaded checkpoint: it has the warmed maps,
	// channels and reader buffers every later one reuses.
	base := samples[1]
	const heapSlack, liveSlack = 2 << 20, 512 << 10
	for _, sm := range samples[2:] {
		if sm.heap > base.heap+heapSlack {
			t.Errorf("Go heap grew: %d KiB after %d ops, %d KiB after %d", sm.heap>>10, sm.ops, base.heap>>10, base.ops)
		}
		if sm.live > base.live+liveSlack {
			t.Errorf("Rust live bytes grew: %d after %d ops, %d after %d", sm.live, sm.ops, base.live, base.ops)
		}
		if sm.goroutines > base.goroutines {
			t.Errorf("goroutines grew: %d after %d ops, %d after %d", sm.goroutines, sm.ops, base.goroutines, base.ops)
		}
	}
}

// mode16 asks the diagnostic engine for n generated bytes, byte i = i ^ seed.
func mode16(n int, seed byte) []byte {
	in := make([]byte, 6)
	in[0] = 16
	binary.LittleEndian.PutUint32(in[1:5], uint32(n))
	in[5] = seed
	return in
}

func generated(b []byte, n int, seed byte) bool {
	if len(b) != n {
		return false
	}
	for i, v := range b {
		if v != byte(i)^seed {
			return false
		}
	}
	return true
}
