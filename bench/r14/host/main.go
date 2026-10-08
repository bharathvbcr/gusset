// Command r14host links three Rust staticlibs into one Go binary: Gusset's
// libgusset.a, engine A and engine B, each built separately with its own copy of
// std. It drives all three at once, including panics in all three, and exits
// non-zero when any of them behaves as if it shared runtime state with another.
//
// Built and run by ../run.sh. See docs/rfc-r14.md for what each check proves.
package main

/*
// run.sh names the directory holding this scenario's archives in CGO_LDFLAGS
// (-L...), and stamps their hash into zz_archives.go before each link, because
// the Go build cache does not hash external archives.
#cgo LDFLAGS: -lengine_a -lengine_b -lpthread -lm -ldl
#cgo noescape ea_call
#cgo nocallback ea_call
#cgo noescape ea_live_bytes
#cgo nocallback ea_live_bytes
#cgo noescape ea_set_quiet_hook
#cgo nocallback ea_set_quiet_hook
#cgo noescape eb_call
#cgo nocallback eb_call
#cgo noescape eb_set_counting_hook
#cgo nocallback eb_set_counting_hook
#cgo noescape eb_hook_hits
#cgo nocallback eb_hook_hits
#cgo noescape ea_hold
#cgo nocallback ea_hold
#cgo noescape ea_release
#cgo nocallback ea_release
#cgo noescape ea_tls_next
#cgo nocallback ea_tls_next
#cgo noescape eb_hold
#cgo nocallback eb_hold
#cgo noescape eb_release
#cgo nocallback eb_release
#cgo noescape eb_tls_next
#cgo nocallback eb_tls_next
#include <stdint.h>
int64_t ea_call(int64_t n);
int64_t ea_live_bytes(void);
void ea_set_quiet_hook(void);
int64_t eb_call(int64_t n);
void eb_set_counting_hook(void);
uint64_t eb_hook_hits(void);
void ea_hold(uint64_t bytes);
void ea_release(void);
int64_t ea_tls_next(void);
void eb_hold(uint64_t bytes);
void eb_release(void);
int64_t eb_tls_next(void);
*/
import "C"

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bharathvbcr/gusset"
)

const (
	goroutines = 32
	iterations = 200
	panicEvery = 7 // iteration i panics in both engines when i%panicEvery == 0
	gussetPans = 16
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func run() error {
	// Engine A silences its panics; engine B counts its own. With one shared std
	// the second set_hook would replace the first, and B's count would include
	// A's and Gusset's panics too.
	C.ea_set_quiet_hook()
	C.eb_set_counting_hook()

	echo, err := gusset.Open(gusset.WithDiagnosticEngine())
	if err != nil {
		return fmt.Errorf("gusset.Open: %w", err)
	}
	defer echo.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var wrong atomic.Int64
	var firstWrong atomic.Value
	note := func(format string, args ...any) {
		wrong.Add(1)
		firstWrong.CompareAndSwap(nil, fmt.Sprintf(format, args...))
	}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				n := int64(i % 50)
				if i%panicEvery == 0 {
					n = -1
				}
				wantA, wantB := n*(n-1)/2, n*2
				if n < 0 {
					wantA, wantB = -1, -2
				}
				if got := int64(C.ea_call(C.int64_t(n))); got != wantA {
					note("ea_call(%d) = %d, want %d", n, got, wantA)
				}
				if got := int64(C.eb_call(C.int64_t(n))); got != wantB {
					note("eb_call(%d) = %d, want %d", n, got, wantB)
				}
				// Mode 0 of the diagnostic engine echoes its input.
				in := []byte{0, byte(i), byte(n)}
				out, err := echo.Call(ctx, in)
				if err != nil || !bytes.Equal(out, in) {
					note("gusset echo: out=%v err=%v", out, err)
				}
			}
		}()
	}

	// Gusset's own panic firewall, firing while both engines are unwinding on
	// other threads: each handle panics once (mode 1), reports ErrPanic with the
	// payload Gusset's hook captured, and is poisoned afterwards (R10).
	for p := 0; p < gussetPans; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := gusset.Open(gusset.WithDiagnosticEngine())
			if err != nil {
				note("gusset.Open: %v", err)
				return
			}
			defer h.Close()
			if _, err := h.Call(ctx, []byte{1}); !errors.Is(err, gusset.ErrPanic) || !strings.Contains(err.Error(), "plain panic") {
				note("gusset panic call: err=%v, want ErrPanic carrying \"plain panic\"", err)
			}
			if _, err := h.Call(ctx, []byte{0}); !errors.Is(err, gusset.ErrPoisoned) {
				note("gusset call after panic: err=%v, want ErrPoisoned", err)
			}
		}()
	}
	wg.Wait()

	enginePanics := uint64(goroutines * ((iterations + panicEvery - 1) / panicEvery))
	hits := uint64(C.eb_hook_hits())
	fmt.Printf("engine calls=%d gusset calls=%d engine panics per engine=%d gusset panics=%d\n",
		goroutines*iterations*2, goroutines*iterations+2*gussetPans, enginePanics, gussetPans)
	fmt.Printf("wrong=%d engine_b hook hits=%d (want %d) engine_a live bytes=%d\n",
		wrong.Load(), hits, enginePanics, int64(C.ea_live_bytes()))

	if n := wrong.Load(); n != 0 {
		return fmt.Errorf("%d wrong results; first: %v", n, firstWrong.Load())
	}
	if hits != enginePanics {
		return fmt.Errorf("engine B's panic hook saw %d panics, want exactly its own %d", hits, enginePanics)
	}
	if err := checkThreadLocals(); err != nil {
		return err
	}
	return checkAllocators()
}

// checkThreadLocals pins one OS thread and alternates both engines' thread-local
// counters with panics in each. Each copy of std keeps its own thread-local
// storage and its own panic count on the shared thread: a counter that skips
// or repeats, or a copy that still reports the thread as unwinding after the
// other copy's caught panic, fails the check.
func checkThreadLocals() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	a0, b0 := int64(C.ea_tls_next()), int64(C.eb_tls_next())
	if a0 < 1 || b0 < 1 {
		return fmt.Errorf("thread-local probes before panics: a=%d b=%d", a0, b0)
	}
	const rounds = 100
	for i := int64(1); i <= rounds; i++ {
		if got := int64(C.ea_call(-1)); got != -1 {
			return fmt.Errorf("pinned ea_call(-1) = %d, want -1", got)
		}
		if got := int64(C.eb_tls_next()); got != b0+i {
			return fmt.Errorf("round %d: engine B's thread-local = %d after engine A panicked, want %d", i, got, b0+i)
		}
		if got := int64(C.eb_call(-1)); got != -2 {
			return fmt.Errorf("pinned eb_call(-1) = %d, want -2", got)
		}
		if got := int64(C.ea_tls_next()); got != a0+i {
			return fmt.Errorf("round %d: engine A's thread-local = %d after engine B panicked, want %d", i, got, a0+i)
		}
	}
	fmt.Printf("thread-locals: %d alternating panics on one OS thread, both counters exact\n", 2*rounds)
	return nil
}

// checkAllocators holds 64 MiB in each engine in turn. Only the engine that
// allocated may see it: engine A's counting allocator must not move when B
// allocates, and Gusset's accounting must not move for either.
func checkAllocators() error {
	const held = 64 << 20
	const slack = 1 << 20
	near := func(a, b int64) bool { d := a - b; return -slack < d && d < slack }

	a0, g0 := int64(C.ea_live_bytes()), int64(gusset.Stats().LiveBytes)
	C.eb_hold(held)
	a1, g1 := int64(C.ea_live_bytes()), int64(gusset.Stats().LiveBytes)
	C.eb_release()
	if !near(a1, a0) || !near(g1, g0) {
		return fmt.Errorf("engine B held %d bytes; engine A's live moved %d -> %d, Gusset's %d -> %d", held, a0, a1, g0, g1)
	}
	C.ea_hold(held)
	a2, g2 := int64(C.ea_live_bytes()), int64(gusset.Stats().LiveBytes)
	C.ea_release()
	a3 := int64(C.ea_live_bytes())
	if a2-a0 < held || !near(a3, a0) || !near(g2, g0) {
		return fmt.Errorf("engine A held %d bytes; its live went %d -> %d -> %d, Gusset's %d -> %d", held, a0, a2, a3, g0, g2)
	}
	fmt.Printf("allocators: B's %d MiB invisible to A and Gusset; A's own %d MiB counted by A only\n", held>>20, held>>20)
	return nil
}
