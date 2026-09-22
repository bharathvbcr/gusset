package pitfalls_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
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

			var wg sync.WaitGroup
			for c := 0; c < callers; c++ {
				seed := rng.Int63()
				wg.Add(1)
				go func() {
					defer wg.Done()
					r := rand.New(rand.NewSource(seed))
					for i := 0; i < callsPerCaller; i++ {
						runOne(h, r, fail, &successes, &panics, &poisoned, &closed)
					}
				}()
			}

			// Half the rounds close while calls are still in flight.
			if round%2 == 0 {
				time.Sleep(time.Duration(rng.Intn(40)) * time.Millisecond)
				if err := h.Close(); err != nil {
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
func runOne(h *gusset.Handle, r *rand.Rand, fail func(string, ...any),
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
		b := in.Bytes()
		if b == nil { // closed underneath us
			_ = in.Free()
			closed.Add(1)
			return
		}
		b[0] = 0
		for j := 1; j < len(b); j++ {
			b[j] = byte(j ^ int(r.Int31()))
		}
		want = append([]byte(nil), b...)
		res, cerr := h.CallBuffer(ctx, in)
		_ = in.Free()
		err = cerr
		if cerr == nil {
			if rb := res.Bytes(); rb != nil {
				out = append([]byte(nil), rb...)
			} else {
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
