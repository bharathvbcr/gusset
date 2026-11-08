package main

import (
	"bytes"
	"strings"
	"testing"
)

// comparedTable is a package both files have rows for: benchstat names both
// files in the table header and every row is a 7-field comparison.
const comparedTable = `pkg: x

,old.txt,,new.txt,,,
,sec/op,CI,sec/op,CI,vs base,P
Steady-4,1.0,2%,1.01,2%,~,p=0.5 n=10
geomean,1.0,,1.01,,+1.00%,
`

// singleFileTable is a package only one file has rows for. benchstat gives it a
// table of its own whose header names that one file, so each row has 3 fields
// whichever side it is on. This is the shape the bench job produced when the
// base had no internal/isolate benchmarks and the head did.
func singleFileTable(file string) string {
	return `pkg: x/internal/isolate
,` + file + `,
,sec/op,CI
ProcCallEcho-4,0.000147,24%
SupervisorCallEcho-4,0.000100,49%
geomean,0.000121,
`
}

// The fixtures carry only sec/op tables, as benchstat prints without
// -benchmem, so the second package's pkg: line follows a sec/op table directly.
// That line is configuration, not a one-sided benchmark.
func TestRegressNewPackageTableIsNewOnly(t *testing.T) {
	in := comparedTable + "\n" + singleFileTable("new.txt")
	var out bytes.Buffer
	if err := checkRegress(strings.NewReader(in), 10, 6, nil, &out); err != nil {
		t.Fatalf("a package the change adds failed the gate: %v", err)
	}
	for _, name := range []string{"ProcCallEcho-4", "SupervisorCallEcho-4"} {
		if !strings.Contains(out.String(), "::notice title=New benchmark::"+name) {
			t.Errorf("no notice for %s:\n%s", name, out.String())
		}
	}
}

// benchstat's table order is not the job's: a new package may come first.
func TestRegressNewPackageTableBeforeTheComparisonIsNewOnly(t *testing.T) {
	in := singleFileTable("new.txt") + "\n" + comparedTable
	if err := checkRegress(strings.NewReader(in), 10, 6, nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("a package the change adds, printed before the compared one, failed the gate: %v", err)
	}
}

func TestRegressDroppedPackageTableStillFails(t *testing.T) {
	in := comparedTable + "\n" + singleFileTable("old.txt")
	err := checkRegress(strings.NewReader(in), 10, 6, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "ProcCallEcho-4") || !strings.Contains(err.Error(), "base-only") {
		t.Fatalf("a package the change drops passed the gate: %v", err)
	}
}

// With no table naming both files there is nothing to tell which side a
// single-file table is on, so its rows fail as they always did.
func TestRegressSingleFileTableWithoutAComparisonFailsClosed(t *testing.T) {
	err := checkRegress(strings.NewReader(singleFileTable("new.txt")), 10, 6, nil, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a single-file table with no two-file table to place it passed the gate")
	}
}
