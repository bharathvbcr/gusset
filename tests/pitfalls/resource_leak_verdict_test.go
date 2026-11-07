//go:build !windows

package pitfalls_test

import "testing"

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
