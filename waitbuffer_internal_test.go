package gusset

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func dataPtr(b []byte) uintptr {
	return uintptr(unsafe.Pointer(unsafe.SliceData(b)))
}

// adoptHeapBuffer takes the slice itself when it is 64-byte aligned, and copies
// otherwise. The alignment is tested here on slices whose placement the test
// chooses, so neither arm depends on where Go's allocator put anything.
func TestAdoptHeapBuffer_AdoptsAlignedAndCopiesUnaligned(t *testing.T) {
	const n = 100
	backing := make([]byte, n+2*heapBufferAlign)
	off := 0
	if rem := int(dataPtr(backing) % heapBufferAlign); rem != 0 {
		off = heapBufferAlign - rem
	}
	fill := func(b []byte) {
		for i := range b {
			b[i] = byte(i*7 + 3)
		}
	}

	t.Run("aligned is adopted", func(t *testing.T) {
		data := backing[off : off+n : off+n]
		fill(data)
		buf := adoptHeapBuffer(nil, data)
		if dataPtr(buf.data) != dataPtr(data) {
			t.Fatal("an aligned result was copied; want it adopted as the Buffer's backing")
		}
		if len(buf.data) != n || cap(buf.data) != n {
			t.Fatalf("len %d cap %d, want %d/%d", len(buf.data), cap(buf.data), n, n)
		}
		if buf.id != 0 {
			t.Fatalf("id %d, want 0", buf.id)
		}
	})

	t.Run("aligned with spare capacity is cut to cap == len", func(t *testing.T) {
		data := backing[off : off+n] // cap runs on past n
		fill(data)
		buf := adoptHeapBuffer(nil, data)
		if dataPtr(buf.data) != dataPtr(data) {
			t.Fatal("an aligned result was copied; want it adopted")
		}
		if cap(buf.data) != n {
			t.Fatalf("cap %d, want %d: an append would write past the result", cap(buf.data), n)
		}
	})

	t.Run("unaligned is copied", func(t *testing.T) {
		data := backing[off+1 : off+1+n : off+1+n]
		fill(data)
		buf := adoptHeapBuffer(nil, data)
		if dataPtr(buf.data) == dataPtr(data) {
			t.Fatal("an unaligned result was adopted")
		}
		if !isAligned64(buf.data) {
			t.Fatal("the copy is not 64-byte aligned")
		}
		if !bytes.Equal(buf.data, data) {
			t.Fatal("the copy differs")
		}
		if cap(buf.data) != n {
			t.Fatalf("cap %d, want %d", cap(buf.data), n)
		}
		// The copy is independent of its source.
		buf.data[0] ^= 0xff
		if buf.data[0] == data[0] {
			t.Fatal("the copy shares its source")
		}
	})

	t.Run("empty is never adopted", func(t *testing.T) {
		buf := adoptHeapBuffer(nil, backing[off:off])
		if len(buf.data) != 0 {
			t.Fatalf("len %d, want 0", len(buf.data))
		}
	})
}

// completedData returns the result a finished ticket is holding in completed,
// the very slice WaitBuffer is about to be handed.
func completedData(t *testing.T, s *handleState, ticket uint64) []byte {
	t.Helper()
	var data []byte
	eventually(t, "the result to complete", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		res, ok := s.completed[ticket]
		data = res.data
		return ok
	})
	return data
}

// Every inline size yields a correct, 64-byte aligned, cap == len, id-0 Buffer
// through WaitBuffer, whichever way it was built. A result that arrives aligned
// is the Buffer's own backing; one that does not is a copy. Which of the two
// happens is decided by the run-time check alone, so both are asserted from the
// slice the waiter was actually handed, not from a size.
func TestWaitBuffer_EverySizeIsAlignedAndCorrectWhetherAdoptedOrCopied(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()

	var adopted, copied int
	for n := 1; n <= inlineResultBytes; n++ {
		in := echoInput(n)
		tk, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		held := completedData(t, h.state, tk)
		wasAligned := dataPtr(held)%heapBufferAlign == 0
		buf, err := h.WaitBuffer(ctx, tk)
		if err != nil {
			t.Fatalf("%d B: %v", n, err)
		}
		got := buf.Bytes()
		if buf.ID() != 0 || !bytes.Equal(got, in) || !isAligned64(got) || cap(got) != len(got) {
			t.Fatalf("%d B: id %d, intact %v, aligned %v, len %d cap %d",
				n, buf.ID(), bytes.Equal(got, in), isAligned64(got), len(got), cap(got))
		}
		if shared := dataPtr(got) == dataPtr(held); shared != wasAligned {
			t.Fatalf("%d B: result aligned=%v but adopted=%v; the run-time check alone decides", n, wasAligned, shared)
		}
		if wasAligned {
			adopted++
		} else {
			copied++
		}
		if err := buf.Free(); err != nil {
			t.Fatal(err)
		}
	}
	// Both arms must have run, or this test proved nothing about one of them.
	// A result of 4096 bytes is a whole size class and lands on a page-aligned
	// span, so the adopting arm always runs; the sizes between 64-byte classes
	// are placed at 16-byte multiples and so exercise the copying arm.
	if adopted == 0 || copied == 0 {
		t.Fatalf("adopted %d, copied %d: the sweep did not exercise both arms", adopted, copied)
	}
	t.Logf("adopted %d sizes, copied %d, of %d", adopted, copied, inlineResultBytes)
}

// CallBuffer reaches the same Buffer through waitBuffer, from a Go-heap input
// and, for sizes over 4 KiB, from a Rust-owned one.
func TestCallBuffer_EverySizeIsAlignedAndCorrect(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()

	check := func(n int, in *Buffer, want []byte) {
		t.Helper()
		out, err := h.CallBuffer(ctx, in)
		if err != nil {
			t.Fatalf("%d B: %v", n, err)
		}
		defer out.Free()
		got := out.Bytes()
		if !bytes.Equal(got, want) {
			t.Fatalf("%d B: result differs (%d bytes)", n, len(got))
		}
		if !isAligned64(got) {
			t.Fatalf("%d B: result is not 64-byte aligned", n)
		}
		if n <= inlineResultBytes {
			if out.ID() != 0 || cap(got) != len(got) {
				t.Fatalf("%d B: id %d cap %d len %d, want id 0 and cap == len", n, out.ID(), cap(got), len(got))
			}
		} else if out.ID() == 0 {
			t.Fatalf("%d B: a result over the inline limit lost its Rust id", n)
		}
	}

	for n := 1; n <= inlineResultBytes; n++ {
		in := echoInput(n)
		seed, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		src, err := h.WaitBuffer(ctx, seed)
		if err != nil {
			t.Fatal(err)
		}
		check(n, src, in)
		_ = src.Free()
	}
	for _, n := range []int{inlineResultBytes + 1, inlineResultBytes + 64, 5000, 16384, 65536} {
		in, err := h.NewBuffer(n)
		if err != nil {
			t.Fatal(err)
		}
		want := echoInput(n)
		copy(in.Bytes(), want)
		check(n, in, want)
		_ = in.Free()
	}
}

// Results that were produced together share no memory: writing through one
// Buffer, or appending to its Bytes, changes nothing in any other result, and
// no two results overlap. Sizes below 16 bytes come from Go's tiny allocator,
// which packs several objects into one block; the rest cover the size classes.
func TestWaitBuffer_ResultsDoNotAliasEachOther(t *testing.T) {
	h, err := Open(WithPoolSize(8), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()

	sizes := []int{1, 2, 3, 7, 8, 15, 16, 17, 48, 49, 63, 64, 65, 100, 128, 256, 1024, 1152, 4095, 4096}
	const workers = 8
	const perWorker = 40
	type result struct {
		buf  *Buffer
		want []byte
	}
	results := make([]result, workers*perWorker)
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				idx := w*perWorker + i
				n := sizes[idx%len(sizes)]
				in := echoInput(n)
				// Distinct content per job in the tail; byte 0 stays the echo mode.
				for j := 1; j < n; j++ {
					in[j] ^= byte(idx * 31)
				}
				tk, err := h.Submit(ctx, in)
				if err != nil {
					errs <- err
					return
				}
				buf, err := h.WaitBuffer(ctx, tk)
				if err != nil {
					errs <- err
					return
				}
				results[idx] = result{buf: buf, want: in}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// No two live results overlap.
	type span struct{ lo, hi uintptr }
	spans := make([]span, 0, len(results))
	for _, r := range results {
		b := r.buf.Bytes()
		spans = append(spans, span{dataPtr(b), dataPtr(b) + uintptr(len(b))})
	}
	for i := range spans {
		for j := i + 1; j < len(spans); j++ {
			if spans[i].lo < spans[j].hi && spans[j].lo < spans[i].hi {
				t.Fatalf("results %d and %d overlap in memory", i, j)
			}
		}
	}

	// Scribble over each result in turn, and append to it; every other result
	// must still hold exactly what its job returned.
	for i, r := range results {
		b := r.buf.Bytes()
		if !bytes.Equal(b, r.want) {
			t.Fatalf("result %d was changed before it was touched", i)
		}
		for j := range b {
			b[j] = 0xA5
		}
		grown := append(b, bytes.Repeat([]byte{0x5A}, 64)...)
		if len(b) > 0 && &grown[0] == &b[0] {
			t.Fatalf("result %d: append wrote in place, cap %d len %d", i, cap(b), len(b))
		}
		for k := i + 1; k < len(results); k++ {
			if !bytes.Equal(results[k].buf.Bytes(), results[k].want) {
				t.Fatalf("scribbling over result %d changed result %d", i, k)
			}
		}
	}
	for i, r := range results {
		for _, c := range r.buf.Bytes() {
			if c != 0xA5 {
				t.Fatalf("result %d holds %#x after its own scribble; something else wrote to it", i, c)
			}
		}
		if err := r.buf.Free(); err != nil {
			t.Fatal(err)
		}
	}
}

// A WaitBuffer result of 4096 bytes is the slice drainPipe made, with nothing
// copied: the whole of what it costs beyond Wait is the *Buffer.
//
// 4096 is the size this exists for. It is a full size class, so the result is
// 64-byte aligned on every Go runtime this library supports; if that stops
// being true the fast path silently stops being taken, and this test says so.
func TestWaitBuffer_AlignedResultCostsOnlyTheBufferHeader(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()
	in := echoInput(inlineResultBytes)

	tk, err := h.Submit(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if held := completedData(t, h.state, tk); dataPtr(held)%heapBufferAlign != 0 {
		t.Fatalf("a %d-byte result is not 64-byte aligned (%#x): the size-class placement this fast path relies on no longer holds", inlineResultBytes, dataPtr(held))
	}
	if err := h.Discard(tk); err != nil {
		t.Fatal(err)
	}

	waitAllocs := testing.AllocsPerRun(200, func() {
		tk, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Wait(ctx, tk); err != nil {
			t.Fatal(err)
		}
	})
	waitBufferAllocs := testing.AllocsPerRun(200, func() {
		tk, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		buf, err := h.WaitBuffer(ctx, tk)
		if err != nil {
			t.Fatal(err)
		}
		_ = buf.Free()
	})
	t.Logf("allocs/op at %d B: Wait %v, WaitBuffer %v", inlineResultBytes, waitAllocs, waitBufferAllocs)
	// Wait allocates the result once (drainPipe). WaitBuffer may add the
	// *Buffer, and nothing else: a second backing array is the copy.
	if waitBufferAllocs > waitAllocs+1 {
		t.Fatalf("WaitBuffer costs %v allocs/op against Wait's %v: the result was copied instead of adopted", waitBufferAllocs, waitAllocs)
	}
}

// TestWaitBuffer_CloseCannotFreeTheResultBeingFilled pins the window between
// WaitBuffer deciding how to carry an inline result and copying the result
// into its Buffer.
//
// The 2026-09-30 write-after-free (CHANGELOG 0.0.3, I1): waitBuffer allocated a
// Rust buffer for an inline result and copied into it with no lock held. Close
// takes cgoMu exclusively and frees every Rust buffer of the handle, so a Close
// landing in that gap finished first and the copy wrote the result into freed
// allocator memory: a write of up to inlineResultBytes into the C heap, on a
// size class the next posix_memalign of the same size walks (the hammer SIGSEGV
// was a 973-byte request in the 992-byte class finding a corrupted tcache next
// pointer).
//
// The fix first held cgoMu from the allocation to the end of the copy, and this
// test asserted that Close could not finish while the fill was pending. An
// inline result no longer touches Rust at all: drainPipe has already copied it
// into Go memory, and WaitBuffer carries it in a Go-heap Buffer with id 0. There
// is no Rust buffer for Close to free and no lock to hold, so "Close cannot
// finish first" stopped being true and stopped being needed. The invariant under
// it is the one asserted here, with the race still forced by the hook: a Close
// that starts and finishes before the copy lands does not touch the result, the
// Buffer holds Go memory (nothing of the handle's was written), the bytes are
// intact, and the Buffer can still be freed.
func TestWaitBuffer_CloseCannotFreeTheResultBeingFilled(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	in := make([]byte, 973) // inline (<= inlineResultBytes): the size class of the original SIGSEGV
	for i := 1; i < len(in); i++ {
		in[i] = byte(i*7 + 3)
	}
	in[0] = 0 // diagnostic echo
	ctx := context.Background()
	tk, err := h.Submit(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	hooked := false
	closeFinishedBeforeFill := false
	testHookBeforeResultFill = func(s *handleState) {
		hooked = true
		go func() { _ = h.Close() }()
		select {
		case <-s.closeDone:
			closeFinishedBeforeFill = true
		case <-time.After(5 * time.Second):
		}
	}
	defer func() { testHookBeforeResultFill = nil }()

	buf, werr := h.WaitBuffer(ctx, tk)
	testHookBeforeResultFill = nil
	if !hooked {
		t.Fatalf("the fill hook never ran (WaitBuffer: %v); the result did not take the inline path", werr)
	}
	if !closeFinishedBeforeFill {
		t.Fatal("Close did not finish while the fill was pending: the inline fill holds a lock it has no need of, or Close is blocked behind WaitBuffer")
	}
	if werr != nil {
		t.Fatalf("WaitBuffer: %v", werr)
	}
	if buf.ID() != 0 {
		t.Fatalf("an inline result has Rust buffer id %d: Close frees those, and the fill would race it", buf.ID())
	}
	// Bytes is nil once the handle is closed (its documented contract), so the
	// test reads the Go slice the fill wrote, which is what a write into freed
	// memory would have corrupted or never landed in.
	if !bytes.Equal(buf.data, in) {
		t.Fatalf("result corrupted by the Close that finished during the fill: len %d, want %d", len(buf.data), len(in))
	}
	if b := buf.Bytes(); b != nil {
		t.Fatalf("Bytes after Close = %d bytes, want nil", len(b))
	}
	if err := buf.Free(); err != nil {
		t.Fatalf("Free of a result carried across Close: %v", err)
	}
}

// echoInput is a diagnostic-engine request whose result is itself: mode 0
// echoes, so an n-byte request yields an n-byte result (n >= 1).
func echoInput(n int) []byte {
	in := make([]byte, n)
	for i := 1; i < n; i++ {
		in[i] = byte(i*13 + 5)
	}
	return in
}

func isAligned64(b []byte) bool {
	return len(b) == 0 || uintptr(unsafe.Pointer(unsafe.SliceData(b)))%64 == 0
}

// An inline result (<= inlineResultBytes) is carried in 64-byte aligned Go
// memory with id 0; anything larger stays a Rust take buffer. Both are used the
// same way: Bytes, Free (idempotent, Bytes nil after), Submit input,
// CallBuffer input. The Go-memory kind owns nothing in Rust.
func TestWaitBuffer_InlineResultIsGoMemoryWithTheSameContract(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()

	for _, n := range []int{1, 48, 256, 1024, inlineResultBytes} {
		t.Run(fmt.Sprintf("%dB", n), func(t *testing.T) {
			in := echoInput(n)
			tk, err := h.Submit(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			buf, err := h.WaitBuffer(ctx, tk)
			if err != nil {
				t.Fatal(err)
			}
			if buf.ID() != 0 {
				t.Fatalf("inline result of %d bytes has Rust id %d, want a Go-memory buffer (id 0)", n, buf.ID())
			}
			got := buf.Bytes()
			if !bytes.Equal(got, in) {
				t.Fatalf("Bytes() = %d bytes, want the %d-byte result", len(got), n)
			}
			if !isAligned64(got) {
				t.Fatal("result is not 64-byte aligned")
			}
			if buf.owner != h {
				t.Fatal("the Buffer does not keep its Handle reachable")
			}

			// Reused as input, by Submit and by CallBuffer: its bytes travel inline.
			tk2, err := h.Submit(ctx, buf)
			if err != nil {
				t.Fatalf("Submit(result buffer): %v", err)
			}
			out, err := h.Wait(ctx, tk2)
			if err != nil || !bytes.Equal(out, in) {
				t.Fatalf("Wait of a job fed the result buffer: %d bytes, err %v", len(out), err)
			}
			cb, err := h.CallBuffer(ctx, buf)
			if err != nil {
				t.Fatalf("CallBuffer(result buffer): %v", err)
			}
			if cb.ID() != 0 || !bytes.Equal(cb.Bytes(), in) {
				t.Fatalf("CallBuffer output: id %d, %d bytes", cb.ID(), len(cb.Bytes()))
			}
			if err := cb.Free(); err != nil {
				t.Fatal(err)
			}

			if err := buf.Free(); err != nil {
				t.Fatalf("Free: %v", err)
			}
			if err := buf.Free(); err != nil {
				t.Fatalf("second Free: %v", err)
			}
			if buf.Bytes() != nil {
				t.Fatal("Bytes() after Free must be nil")
			}
			if _, err := h.Submit(ctx, buf); err == nil || errors.Is(err, ErrClosed) {
				t.Fatalf("Submit of a freed result buffer on an open handle: %v, want a non-ErrClosed refusal", err)
			}
			// The handle is still fully usable.
			if out, err := h.Call(ctx, in); err != nil || !bytes.Equal(out, in) {
				t.Fatalf("Call after: %d bytes, err %v", len(out), err)
			}
		})
	}

	t.Run("over the inline limit stays Rust-owned", func(t *testing.T) {
		const n = inlineResultBytes + 1
		in, err := h.NewBuffer(n)
		if err != nil {
			t.Fatal(err)
		}
		defer in.Free()
		copy(in.Bytes(), echoInput(n))
		tk, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		out, err := h.WaitBuffer(ctx, tk)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Free()
		if out.ID() == 0 {
			t.Fatalf("a %d-byte result is a take buffer and must keep its Rust id", n)
		}
		if !bytes.Equal(out.Bytes(), echoInput(n)) {
			t.Fatal("result differs")
		}
	})
}

// An inline result already stored when Close begins is refused, as a take
// result is: claimLocked hands out a stored result before it looks at closed,
// and the Go-memory path takes no lock that would refuse it.
func TestWaitBuffer_InlineResultAfterCloseBeginsIsErrClosed(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	s := h.state
	ctx := context.Background()
	for _, n := range []int{0, 48, 1024} {
		in := echoInput(n)
		tk, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "the result to complete", func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			_, ok := s.completed[tk]
			return ok
		})
		s.closed.Store(true)
		_, err = h.WaitBuffer(ctx, tk)
		s.closed.Store(false)
		wantClosed(t, fmt.Sprintf("WaitBuffer of a stored %d-byte inline result after close began", n), err, "gusset: handle is closed")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

// A Go-memory result raced against Close: WaitBuffer either refuses with
// ErrClosed, or returns a Buffer whose bytes are intact, whose Free works, and
// whose slice, if one was taken before the close, stays valid after Close and a
// GC (Go memory, unlike a Rust view, is never released under it).
func TestWaitBuffer_InlineResultSurvivesConcurrentClose(t *testing.T) {
	const trials = 100
	for trial := 0; trial < trials; trial++ {
		h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
		if err != nil {
			t.Fatal(err)
		}
		in := echoInput(973)
		ctx := context.Background()
		tk, err := h.Submit(ctx, in)
		if err != nil {
			t.Fatal(err)
		}

		var (
			wg   sync.WaitGroup
			buf  *Buffer
			werr error
			got  []byte
		)
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			buf, werr = h.WaitBuffer(ctx, tk)
			if buf != nil {
				got = buf.Bytes()
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = h.Close()
		}()
		close(start)
		wg.Wait()
		_ = h.Close()

		if werr != nil {
			if !errors.Is(werr, ErrClosed) {
				t.Fatalf("trial %d: WaitBuffer raced with Close: %v, want a result or ErrClosed", trial, werr)
			}
			continue
		}
		runtime.GC()
		runtime.GC()
		if got != nil && !bytes.Equal(got, in) {
			t.Fatalf("trial %d: a slice taken before Close changed after it", trial)
		}
		if buf.ID() != 0 || !bytes.Equal(buf.data, in) {
			t.Fatalf("trial %d: result buffer id %d, intact=%v", trial, buf.ID(), bytes.Equal(buf.data, in))
		}
		if b := buf.Bytes(); b != nil {
			t.Fatalf("trial %d: Bytes after Close = %d bytes, want nil", trial, len(b))
		}
		if err := buf.Free(); err != nil {
			t.Fatalf("trial %d: Free after Close: %v", trial, err)
		}
		if err := buf.Free(); err != nil {
			t.Fatalf("trial %d: second Free: %v", trial, err)
		}
	}
}

// TestTakeResult_CloseCannotFreeTheViewBeingUsed pins the Rust-owned half of I1
// for a result the library hands to a waiter.
//
// A result over 4 KiB is promoted onto a Rust buffer by the worker and reaches
// the waiter as a Go slice over that buffer. Wait copies it out and WaitBuffer
// wraps it; both read Rust memory that Close frees, and Close frees it under
// cgoMu held exclusively. The v0.0.3 write-after-free (CHANGELOG 0.0.3, 38f0296)
// was the same shape on the path WaitBuffer no longer has for inline results: a
// Rust buffer used after cgoMu was released. TestWaitBuffer_CloseCannotFreeTheResultBeingFilled
// stopped asserting lock ordering when an inline result moved to Go memory, so
// this test holds the ordering for the paths that still touch Rust: a Close that
// starts while the view is about to be used must not finish until the view has
// been copied or wrapped.
//
// Before this test, the take paths were covered only by races
// (TestPitfall_WaitBufferOfLargeResultDoesNotDangleAcrossClose and the stress
// hammers), which pass whenever the scheduler does not land a Close in the gap;
// the hook holds the gap open every run.
func TestTakeResult_CloseCannotFreeTheViewBeingUsed(t *testing.T) {
	for _, size := range []int{inlineResultBytes + 1, 16 << 10} {
		for _, viaBuffer := range []bool{false, true} {
			name := "Wait"
			if viaBuffer {
				name = "WaitBuffer"
			}
			t.Run(name+"/"+sizeName(size), func(t *testing.T) {
				h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
				if err != nil {
					t.Fatal(err)
				}
				s := h.state
				ctx := context.Background()
				want := echoInput(size)
				in, err := h.NewBuffer(size)
				if err != nil {
					t.Fatal(err)
				}
				copy(in.Bytes(), want)
				tk, err := h.Submit(ctx, in)
				if err != nil {
					t.Fatal(err)
				}

				hooked := false
				closeFinishedFirst := false
				testHookBeforeTakeView = func(s *handleState) {
					hooked = true
					go func() { _ = h.Close() }()
					select {
					case <-s.closeDone:
						closeFinishedFirst = true
					case <-time.After(500 * time.Millisecond):
					}
				}
				defer func() { testHookBeforeTakeView = nil }()

				var (
					out  []byte
					buf  *Buffer
					werr error
				)
				if viaBuffer {
					buf, werr = h.WaitBuffer(ctx, tk)
				} else {
					out, werr = h.Wait(ctx, tk)
				}
				testHookBeforeTakeView = nil
				if !hooked {
					t.Fatalf("the view hook never ran (%s: %v); the result did not take the Rust buffer path", name, werr)
				}
				if closeFinishedFirst {
					t.Fatalf("Close freed the handle's Rust buffers while %s had yet to use the take buffer: the use reads freed memory", name)
				}
				if werr != nil {
					t.Fatalf("%s: %v", name, werr)
				}
				if viaBuffer {
					if buf.ID() == 0 {
						t.Fatal("a result over 4 KiB is Go memory; it should be the Rust take buffer")
					}
				} else if !bytes.Equal(out, want) {
					t.Fatalf("Wait copied a corrupted result: len %d, want %d", len(out), len(want))
				}

				// The use is over, so Close can finish. The buffer is Rust
				// memory Close has freed or is about to free, so it is read
				// only through Bytes, which withdraws it.
				select {
				case <-s.closeDone:
				case <-time.After(5 * time.Second):
					t.Fatal("Close did not finish once the take buffer was released")
				}
				if viaBuffer {
					if b := buf.Bytes(); b != nil {
						t.Fatalf("Bytes after Close = %d bytes, want nil", len(b))
					}
					if err := buf.Free(); err != nil {
						t.Fatalf("Free after Close: %v", err)
					}
				}
				_ = in.Free()
			})
		}
	}
}

func sizeName(n int) string {
	if n == inlineResultBytes+1 {
		return "smallest-promoted"
	}
	return "16KiB"
}
