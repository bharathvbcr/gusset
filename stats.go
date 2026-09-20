package gusset

import (
	"runtime/debug"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// AllocStats represents memory statistics exported by the Rust allocator.
type AllocStats struct {
	LiveBytes  uint64
	PeakBytes  uint64
	AllocCount uint64
}

// Stats returns the current memory allocation statistics from Rust.
func Stats() AllocStats {
	raw := ffi.GetAllocStats()
	return AllocStats{
		LiveBytes:  raw.LiveBytes,
		PeakBytes:  raw.PeakBytes,
		AllocCount: raw.AllocCount,
	}
}

// AdviseMemoryLimit adjusts Go's runtime memory limit based on total budget and Rust live memory.
// Calls debug.SetMemoryLimit(max(total - rustLive, floor)).
func AdviseMemoryLimit(total int64) int64 {
	const floor = int64(16 * 1024 * 1024) // 16 MiB floor
	st := Stats()
	rustLive := int64(st.LiveBytes)

	target := total - rustLive
	if target < floor {
		target = floor
	}

	return debug.SetMemoryLimit(target)
}
