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

// init() must have run the check, not merely have it available to call: the
// report it verified is kept, and a package that never ran the check leaves it
// zero. This is what fails if the ABI checks are removed from init (I6/R12).
func TestInit_RanTheABICheckAgainstTheLiveLibrary(t *testing.T) {
	if abiAtInit.rust.Version != ExpectedAbiVersion {
		t.Fatalf("init did not verify an ABI report: version %d, want %d", abiAtInit.rust.Version, ExpectedAbiVersion)
	}
	if fresh := gatherABIReport(); fresh != abiAtInit {
		t.Fatalf("the report init verified differs from the live library's now:\n init  %+v\n live  %+v", abiAtInit, fresh)
	}
}

// What init verified covers every layer the check compares: all four structs,
// all named fields, both Rust's and cgo's side.
func TestInit_VerifiedReportCoversEveryStructAndField(t *testing.T) {
	r := abiAtInit
	for i, n := range ffi.AbiTypeNames {
		if r.rust.Sizes[i] == 0 || r.rust.Aligns[i] == 0 || r.localSizes[i] == 0 || r.localAlign[i] == 0 {
			t.Errorf("init verified no layout for %s", n)
		}
	}
	if r.rustCount != ffi.AbiFieldCount {
		t.Errorf("init verified %d fields, want %d", r.rustCount, ffi.AbiFieldCount)
	}
	for i, n := range ffi.AbiFieldNames {
		if r.rustFields[1][i] == 0 || r.localFlds[1][i] == 0 {
			t.Errorf("init verified no size for field %s", n)
		}
	}
	if err := checkABI(r); err != nil {
		t.Errorf("the report init accepted fails the check now: %v", err)
	}
}

// init panics on drift rather than logging and carrying on.
func TestMustCheckABI_PanicsOnDriftAndNamesIt(t *testing.T) {
	mustCheckABI(abiAtInit) // a clean report does not panic

	drifted := abiAtInit
	drifted.rust.Version++
	defer func() {
		v := recover()
		msg, _ := v.(string)
		if !strings.Contains(msg, "ABI version mismatch") {
			t.Fatalf("want a panic naming the version mismatch, got %v", v)
		}
	}()
	mustCheckABI(drifted)
	t.Fatal("mustCheckABI accepted a drifted report")
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
