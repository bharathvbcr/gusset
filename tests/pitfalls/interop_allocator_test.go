package pitfalls_test

import (
	"context"
	"encoding/binary"
	"errors"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/bharathvbcr/gusset"
)

// modeAllocated is diagnostic mode 16: a generated output built through
// gusset::BufferAlloc. With Rust 1.100's stable Allocator trait the output is
// adopted as the result buffer with no copy; on older toolchains the same bytes
// arrive as a plain vector. Either way the Go-visible contract is identical, and
// that is what these tests pin.
const modeAllocated = 16

func allocatedInput(n int, seed byte) []byte {
	in := make([]byte, 6)
	in[0] = modeAllocated
	binary.LittleEndian.PutUint32(in[1:5], uint32(n))
	in[5] = seed
	return in
}

func checkPattern(t *testing.T, what string, got []byte, n int, seed byte) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("%s: got %d bytes, want %d", what, len(got), n)
	}
	for i, b := range got {
		if b != byte(i)^seed {
			t.Fatalf("%s: byte %d = %#x, want %#x", what, i, b, byte(i)^seed)
		}
	}
}

func aligned64(b []byte) bool {
	return len(b) == 0 || uintptr(unsafe.Pointer(unsafe.SliceData(b)))%64 == 0
}

// Every size class through every egress path: inline copy, Wait over a take
// buffer, WaitBuffer zero-copy. Live Rust bytes return to where they started.
func TestInterop_AllocatorBackedOutputAllPaths(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	base := gusset.Stats().LiveBytes
	for _, n := range []int{1, 63, 4096, 4097, 65536, 1 << 20, 16 << 20} {
		seed := byte(n * 7)

		out, err := h.Call(ctx, allocatedInput(n, seed))
		if err != nil {
			t.Fatalf("Call %d: %v", n, err)
		}
		checkPattern(t, "Call", out, n, seed)

		ticket, err := h.Submit(ctx, allocatedInput(n, seed+1))
		if err != nil {
			t.Fatalf("Submit %d: %v", n, err)
		}
		buf, err := h.WaitBuffer(ctx, ticket)
		if err != nil {
			t.Fatalf("WaitBuffer %d: %v", n, err)
		}
		b := buf.Bytes()
		checkPattern(t, "WaitBuffer", b, n, seed+1)
		if !aligned64(b) {
			t.Fatalf("WaitBuffer %d: result not 64-byte aligned", n)
		}
		runtime.KeepAlive(buf)
		if err := buf.Free(); err != nil {
			t.Fatalf("Free %d: %v", n, err)
		}
	}
	if live := gusset.Stats().LiveBytes; live > base {
		t.Fatalf("live Rust bytes %d above baseline %d after every buffer was freed", live, base)
	}
}

// Many goroutines, random sizes, random egress path, random deadlines that
// abandon work mid-flight. Every completed result is checked byte for byte and
// every Rust byte is accounted for once the handle closes.
func TestInterop_AllocatorStress(t *testing.T) {
	if testing.Short() {
		t.Skip("stress")
	}
	base := gusset.Stats().LiveBytes
	h, err := gusset.Open(gusset.WithPoolSize(8), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 48
	const perG = 60
	var ok, timedOut atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			for i := 0; i < perG; i++ {
				n := 1 + rng.Intn(512*1024)
				seed := byte(rng.Intn(256))
				in := allocatedInput(n, seed)
				// A fifth of calls get a deadline short enough to abandon the
				// job; the rest a generous one.
				d := 20 * time.Second
				if rng.Intn(5) == 0 {
					d = time.Duration(rng.Intn(3)) * time.Millisecond
				}
				ctx, cancel := context.WithTimeout(context.Background(), d)
				var got []byte
				var buf *gusset.Buffer
				var err error
				if rng.Intn(2) == 0 {
					got, err = h.Call(ctx, in)
				} else {
					var ticket uint64
					ticket, err = h.Submit(ctx, in)
					if err == nil {
						buf, err = h.WaitBuffer(ctx, ticket)
						if err == nil {
							got = buf.Bytes()
						}
					}
				}
				cancel()
				switch {
				case err == nil:
					for j, b := range got {
						if b != byte(j)^seed || len(got) != n {
							errs <- errors.New("corrupted result")
							return
						}
					}
					if buf != nil {
						if !aligned64(got) {
							errs <- errors.New("unaligned WaitBuffer result")
							return
						}
						runtime.KeepAlive(buf)
						_ = buf.Free()
					}
					ok.Add(1)
				case errors.Is(err, context.DeadlineExceeded):
					timedOut.Add(1)
				default:
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if live := gusset.Stats().LiveBytes; live > base {
		t.Fatalf("live Rust bytes %d above baseline %d after Close", live, base)
	}
	t.Logf("ok=%d timed_out=%d", ok.Load(), timedOut.Load())
}

// A Buffer must keep its Handle alive. The Handle's GC cleanup closes it, and
// closing frees every Rust buffer of the handle, so a caller who kept only the
// Buffer had its memory released under a live slice.
func TestInterop_BufferKeepsHandleAlive(t *testing.T) {
	const n = 256 * 1024
	buf, view := func() (*gusset.Buffer, []byte) {
		h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		ticket, err := h.Submit(ctx, allocatedInput(n, 0x42))
		if err != nil {
			t.Fatal(err)
		}
		b, err := h.WaitBuffer(ctx, ticket)
		if err != nil {
			t.Fatal(err)
		}
		return b, b.Bytes() // h is unreachable from here on, except through b
	}()

	for i := 0; i < 5; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
	if buf.Bytes() == nil {
		t.Fatal("the handle was collected and closed while one of its buffers was still reachable")
	}
	checkPattern(t, "view after GC", view, n, 0x42)
	runtime.KeepAlive(buf)
	_ = buf.Free()
}

// A finished small result collected with WaitBuffer after a sibling panicked
// must still be returned. NewBuffer is poison-checked, and WaitBuffer used it
// to re-wrap results under 4 KiB, so the result was lost as ErrPoisoned after
// it had already left the completed map.
func TestInterop_WaitBufferSmallResultSurvivesSiblingPanic(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Mode 9 delay prefix: 30 units (300 ms), then echo {0,1,2,3}.
	slow, err := h.Submit(ctx, []byte{9, 30, 0, 1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	// 5 units (50 ms), then mode 1: plain panic.
	boom, err := h.Submit(ctx, []byte{9, 5, 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(ctx, boom); !errors.Is(err, gusset.ErrPanic) {
		t.Fatalf("expected ErrPanic from the sibling, got %v", err)
	}
	buf, err := h.WaitBuffer(ctx, slow)
	if err != nil {
		t.Fatalf("a finished result was lost to a sibling's panic: %v", err)
	}
	defer buf.Free()
	got := buf.Bytes()
	if string(got) != string([]byte{0, 1, 2, 3}) {
		t.Fatalf("got %v", got)
	}
	if !aligned64(got) {
		t.Fatal("heap-carried result is not 64-byte aligned")
	}
}

// A second concurrent Close must not return before the first has finished
// joining the workers.
func TestInterop_ConcurrentCloseWaitsForTheFirst(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	// 40 units (400 ms) without a cancellation check: close must join it.
	if _, err := h.Submit(context.Background(), []byte{9, 40}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)

	var firstDone atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = h.Close()
		firstDone.Store(time.Now().UnixNano())
	}()
	time.Sleep(50 * time.Millisecond)
	_ = h.Close()
	second := time.Now().UnixNano()
	wg.Wait()
	if first := firstDone.Load(); second < first {
		t.Fatalf("second Close returned %v before the first finished", time.Duration(first-second))
	}
}

// The runtime runs cleanups one at a time. A forgotten handle whose engine never
// checks for cancellation used to close inline in its cleanup, joining the
// worker and stalling every other cleanup in the process behind it.
func TestInterop_ForgottenHandleCleanupDoesNotStallOtherCleanups(t *testing.T) {
	func() {
		h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
		if err != nil {
			t.Fatal(err)
		}
		// 150 units: 1.5 s, never checks ctx.
		if _, err := h.Submit(context.Background(), []byte{9, 150}); err != nil {
			t.Fatal(err)
		}
	}()
	runtime.GC() // queue the handle's cleanup first
	time.Sleep(50 * time.Millisecond)

	fired := make(chan struct{})
	func() {
		obj := new([64]byte)
		runtime.AddCleanup(obj, func(ch chan struct{}) { close(ch) }, fired)
	}()
	deadline := time.After(1 * time.Second)
	for {
		runtime.GC()
		select {
		case <-fired:
			time.Sleep(1600 * time.Millisecond) // let the background close finish
			return
		case <-deadline:
			t.Fatal("an unrelated cleanup was stalled behind a forgotten handle's close")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// A forgotten Buffer's cleanup used to take cgoMu for reading while a Close
// held it exclusively across an unbounded worker join. Cleanups in one queued
// block run one after another, so every cleanup queued behind it waited for
// the engine to finish. Buffers keep their Handle alive now, so a handle and
// its forgotten buffers become unreachable together: this is the common case.
func TestInterop_ForgottenBufferCleanupDoesNotStallDuringClose(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	// 150 units: 1.5 s, never checks ctx, so Close joins it for that long.
	if _, err := h.Submit(context.Background(), []byte{9, 150}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // running, so Close has a join to wait on
	func() {
		for i := 0; i < 20; i++ {
			if _, err := h.NewBuffer(4096); err != nil {
				t.Fatal(err)
			}
		}
	}()
	closed := make(chan struct{})
	go func() { _ = h.Close(); close(closed) }()
	time.Sleep(100 * time.Millisecond) // Close now holds cgoMu for the join

	fired := make(chan struct{}, 100)
	func() {
		for i := 0; i < 100; i++ {
			obj := new([64]byte)
			runtime.AddCleanup(obj, func(ch chan struct{}) { ch <- struct{}{} }, fired)
		}
	}()
	start := time.Now()
	got := 0
	deadline := time.After(700 * time.Millisecond)
	for got < 100 {
		runtime.GC()
		select {
		case <-fired:
			got++
		case <-deadline:
			t.Fatalf("only %d/100 unrelated cleanups ran in %v: stalled behind a buffer cleanup waiting on Close", got, time.Since(start))
		case <-time.After(10 * time.Millisecond):
		}
	}
	<-closed
}

// Bytes on a buffer whose handle is mid-Close must return nil at once, not park
// behind Close's exclusive lock for the whole worker join.
func TestInterop_BytesDuringCloseDoesNotBlock(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	buf, err := h.NewBuffer(64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Submit(context.Background(), []byte{9, 100}); err != nil { // 1 s
		t.Fatal(err)
	}
	// Let the worker dequeue it: a job cancelled before it starts never runs,
	// and Close would have nothing to join.
	time.Sleep(50 * time.Millisecond)
	closed := make(chan struct{})
	go func() { _ = h.Close(); close(closed) }()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	b := buf.Bytes()
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Fatalf("Bytes blocked %v behind Close", took)
	}
	if b != nil {
		t.Fatal("Bytes returned a view of a buffer whose handle is closing")
	}
	<-closed
}
