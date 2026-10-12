package gusset

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/gusset/internal/ffi"
)

// I6/R12: the ABI check is the only guard between a drifted header and a
// corrupted Go heap. init() runs it once, so this table drives checkABI with
// every kind of drift and requires an error for each; a check that was deleted
// or weakened fails here, not silently at the next struct write.
func TestCheckABI_RefusesEveryDrift(t *testing.T) {
	base := gatherABIReport()
	if err := checkABI(base); err != nil {
		t.Fatalf("the live report must pass its own check: %v", err)
	}

	type abiCase struct {
		name   string
		want   string
		mutate func(r *abiReport)
	}
	var cases []abiCase
	add := func(name, want string, mutate func(r *abiReport)) {
		cases = append(cases, abiCase{name, want, mutate})
	}

	add("version+1", "version mismatch", func(r *abiReport) { r.rust.Version++ })
	add("version-1", "version mismatch", func(r *abiReport) { r.rust.Version-- })
	for i, n := range ffi.AbiTypeNames {
		i, n := i, n
		add("rust size "+n, "size/alignment mismatch", func(r *abiReport) { r.rust.Sizes[i]++ })
		add("rust align "+n, "size/alignment mismatch", func(r *abiReport) { r.rust.Aligns[i] *= 2 })
		// Rust agrees with the Go constants but cgo's compiled header does not.
		add("cgo size "+n, "layout drift for "+n, func(r *abiReport) { r.localSizes[i]++ })
		add("cgo align "+n, "layout drift for "+n, func(r *abiReport) { r.localAlign[i] *= 2 })
	}
	add("field count short", "field count mismatch", func(r *abiReport) { r.rustCount = ffi.AbiFieldCount - 1 })
	add("field count long", "field count mismatch", func(r *abiReport) { r.rustCount = ffi.AbiFieldCount + 1 })

	// Two equal-width fields trade places: every struct size and alignment is
	// unchanged, only the offsets move. FfiStatus.msg and FfiStatus.file are
	// both pointers.
	add("equal-width field swap", "ABI field FfiStatus.", func(r *abiReport) {
		a, b := fieldIndex(t, "FfiStatus.msg"), fieldIndex(t, "FfiStatus.file")
		if r.rustFields[1][a] != r.rustFields[1][b] || r.rustFields[0][a] == r.rustFields[0][b] {
			t.Fatalf("test premise broken: fields must be equal width at different offsets")
		}
		r.rustFields[0][a], r.rustFields[0][b] = r.rustFields[0][b], r.rustFields[0][a]
	})
	for i, name := range ffi.AbiFieldNames {
		i, name := i, name
		add("field offset "+name, "ABI field "+name+" drifted", func(r *abiReport) { r.rustFields[0][i]++ })
		add("field size "+name, "ABI field "+name+" drifted", func(r *abiReport) { r.localFlds[1][i]++ })
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			err := checkABI(r)
			if err == nil {
				t.Fatalf("checkABI accepted a drifted report")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the drift (want substring %q)", err, tc.want)
			}
		})
	}
}

func fieldIndex(t *testing.T, name string) int {
	t.Helper()
	for i, n := range ffi.AbiFieldNames {
		if n == name {
			return i
		}
	}
	t.Fatalf("no ABI field named %q", name)
	return -1
}
