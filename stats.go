package gusset

import (
	"math"
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
//
// The figures are process-global: they count the Rust allocations of every
// handle in the process, not of one handle.
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
// If total < 0, queries the current limit non-destructively.
//
// It returns the limit that was in force before the call, as
// debug.SetMemoryLimit does. A total of 0, or any total at or below rustLive,
// makes the target negative or zero, which is raised to the 16 MiB floor: 0
// sets a 16 MiB limit, it does not remove the limit.
//
// rustLive is what Gusset's counting allocator and its buffers report. Memory
// allocated outside that — a Metal heap, an mmap the engine made itself — is
// invisible here. Callers that park GPU or file mappings must subtract those
// themselves; this function does not grow an API to observe them.
func AdviseMemoryLimit(total int64) int64 {
	if total < 0 {
		return debug.SetMemoryLimit(-1)
	}

	const floor = int64(16 * 1024 * 1024) // 16 MiB floor
	st := Stats()
	var rustLive int64
	if st.LiveBytes > uint64(math.MaxInt64) {
		rustLive = math.MaxInt64
	} else {
		rustLive = int64(st.LiveBytes)
	}

	target := total - rustLive
	if target < floor {
		target = floor
	}

	return debug.SetMemoryLimit(target)
}
