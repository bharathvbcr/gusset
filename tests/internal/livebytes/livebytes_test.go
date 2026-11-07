package livebytes

import (
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// scripted returns a gauge whose reads walk through readings (the last one
// repeats) and which counts collects. Polling is fast so the tests do not wait.
func scripted(readings ...uint64) (gauge, *int) {
	i, collects := 0, 0
	return gauge{
		read: func() uint64 {
			v := readings[min(i, len(readings)-1)]
			i++
			return v
		},
		collect: func() { collects++ },
		every:   time.Millisecond,
		timeout: 100 * time.Millisecond,
	}, &collects
}

func TestNotLeaked(t *testing.T) {
	if !NotLeaked(1000, 0) {
		t.Fatal("a reading under the baseline is an earlier cleanup, not a leak")
	}
	if !NotLeaked(1000, 1000) {
		t.Fatal("the baseline itself is not a leak")
	}
	if NotLeaked(1000, 1001) {
		t.Fatal("a reading above the baseline is a leak")
	}
}

func TestQuiesceWaitsForTheGaugeToStopMoving(t *testing.T) {
	// 900 -> 700 is a straggler's cleanup landing after the first sample.
	g, collects := scripted(900, 800, 700, 700)
	live, ok := g.quiesce()
	if !ok || live != 700 {
		t.Fatalf("quiesce = (%d, %v), want (700, true)", live, ok)
	}
	if *collects < 3 {
		t.Fatalf("quiesce ran %d GCs, want one per sample", *collects)
	}
}

func TestQuiesceGivesUpOnAGaugeThatNeverSettles(t *testing.T) {
	var n uint64
	g := gauge{
		read:    func() uint64 { n++; return n },
		collect: func() {},
		every:   time.Millisecond,
		timeout: 20 * time.Millisecond,
	}
	if _, ok := g.quiesce(); ok {
		t.Fatal("a gauge that moves on every read must not quiesce")
	}
}

func TestSettleAcceptsAReclaimUnderTheBaselineAndOnlySettleGCForcesGC(t *testing.T) {
	g, collects := scripted(1500, 1500, 1000)
	live, ok := g.settle(1000, false)
	if !ok || live != 1000 {
		t.Fatalf("settle = (%d, %v), want (1000, true)", live, ok)
	}
	if *collects != 0 {
		t.Fatalf("Settle forced %d GCs: a GC would let the AddCleanup backstop hide a missing explicit free", *collects)
	}

	g, collects = scripted(1500, 1500, 1000)
	if live, ok := g.settle(1000, true); !ok || live != 1000 {
		t.Fatalf("settle with GC = (%d, %v), want (1000, true)", live, ok)
	}
	if *collects == 0 {
		t.Fatal("SettleGC must force a GC so an abandoned buffer's cleanup can run")
	}

	g, _ = scripted(400)
	if _, ok := g.settle(1000, false); !ok {
		t.Fatal("a drop under the baseline must pass")
	}
}

func TestSettleFailsOnAReadingThatStaysAboveTheBaseline(t *testing.T) {
	g, _ := scripted(1000 + 1<<20)
	live, ok := g.settle(1000, false)
	if ok {
		t.Fatal("a leaked 1 MiB must fail")
	}
	if live != 1000+1<<20 {
		t.Fatalf("last reading = %d, want the leaked level", live)
	}
}

func TestWaitUntilDoesNotCollect(t *testing.T) {
	g, collects := scripted(1, 2, 3)
	if live, ok := g.waitUntil(func(v uint64) bool { return v >= 3 }, false); !ok || live != 3 {
		t.Fatalf("waitUntil = (%d, %v), want (3, true)", live, ok)
	}
	if *collects != 0 {
		t.Fatalf("waitUntil forced %d GCs, want none", *collects)
	}
}

// TestProcessGaugeSeesAHeldBuffer runs the real gauge against the engine: a
// buffer held on purpose is above the quiesced baseline and fails Settle, and
// once freed it passes. This is the planted-leak proof the suites rely on.
func TestProcessGaugeSeesAHeldBuffer(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	baseline := Quiesce(t)
	buf, err := h.NewBuffer(1 << 20)
	if err != nil {
		t.Fatalf("NewBuffer failed: %v", err)
	}
	held := process
	held.timeout = 200 * time.Millisecond
	if _, ok := held.settle(baseline, false); ok {
		t.Fatal("a held 1 MiB buffer must fail the leak check")
	}
	if err := buf.Free(); err != nil {
		t.Fatalf("Free failed: %v", err)
	}
	if live, ok := Settle(baseline); !ok {
		t.Fatalf("after Free, LiveBytes stayed above the baseline: baseline=%d live=%d", baseline, live)
	}
}
