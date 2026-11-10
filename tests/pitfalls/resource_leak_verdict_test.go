//go:build !windows

package pitfalls_test

import "testing"

// scriptedFDs returns an fd reader that walks through readings (the last one
// repeats) and a pointer to how many times it was read.
func scriptedFDs(readings ...int) (func() int, *int) {
	i := 0
	return func() int {
		v := readings[min(i, len(readings)-1)]
		i++
		return v
	}, &i
}

// TestResourceBaselineQuiescesFDs pins the fd baseline every ResourceLeak
// test compares exactly. An earlier test's dropped *os.File closing under a
// GC finalizer after the first read (12 -> 11) must not end up in the
// baseline, or the exact check reports a -1 "leak" that is a late close. A
// count that holds still is taken as is, so a later real +1 stays visible.
//
// It tests the baseline helper only, from scripted readings. The exact
// comparison against this baseline is TestFDLeakVerdict below.
func TestResourceBaselineQuiescesFDs(t *testing.T) {
	cases := []struct {
		name     string
		readings []int
		wantBase int
	}{
		{"straggler closes after the first read", []int{12, 11, 11}, 11},
		{"steady count is taken as is", []int{11, 11}, 11},
		{"no fd directory to count", []int{-1}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read, _ := scriptedFDs(tc.readings...)
			base := takeResourceBaselineWith(t, read)
			if base.fds != tc.wantBase {
				t.Fatalf("baseline fds = %d, want %d (readings %v)", base.fds, tc.wantBase, tc.readings)
			}
		})
	}
}

// TestFDLeakVerdict plants leaked fds against a baseline taken from scripted
// readings and requires the verdict every ResourceLeak test uses to reject
// them. The -1 case is the one a one-sided "final > base" check would pass: a
// real +1 leak masked by a straggler closing two fds.
func TestFDLeakVerdict(t *testing.T) {
	cases := []struct {
		name     string
		readings []int
		final    int
		leaked   bool
	}{
		{"same count is clean", []int{11, 11}, 11, false},
		{"planted +1 leak", []int{12, 11, 11}, 12, true},
		{"planted +3 leak", []int{11, 11}, 14, true},
		{"masked leak shows as -1", []int{11, 11}, 10, true},
		{"uncountable on both sides is clean", []int{-1}, -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read, _ := scriptedFDs(tc.readings...)
			base := takeResourceBaselineWith(t, read)
			if got := fdLeaked(base.fds, tc.final); got != tc.leaked {
				t.Fatalf("fdLeaked(base=%d, final=%d) = %v, want %v", base.fds, tc.final, got, tc.leaked)
			}
		})
	}
}

// TestRustLiveVerdict pins the predicate every ResourceLeak test uses for
// Rust live bytes. A reading below the quiesced baseline is a late reclaim
// from an earlier test and must pass; only a reading above it is a leak.
func TestRustLiveVerdict(t *testing.T) {
	cases := []struct {
		name      string
		baseline  uint64
		live      uint64
		wantDelta int64
		leaked    bool
	}{
		{"equal", 4096, 4096, 0, false},
		{"below baseline is a reclaim", 4096, 1024, -3072, false},
		{"zero live under a nonzero baseline", 4096, 0, -4096, false},
		{"one byte above is a leak", 4096, 4097, 1, true},
		{"well above is a leak", 4096, 1 << 20, 1<<20 - 4096, true},
		{"zero baseline, any live is a leak", 0, 1, 1, true},
		{"zero baseline, zero live", 0, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delta, leaked := rustLiveVerdict(tc.baseline, tc.live)
			if delta != tc.wantDelta || leaked != tc.leaked {
				t.Fatalf("rustLiveVerdict(%d, %d) = (%d, %v), want (%d, %v)",
					tc.baseline, tc.live, delta, leaked, tc.wantDelta, tc.leaked)
			}
		})
	}
}
