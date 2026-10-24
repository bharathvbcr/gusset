package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const zeroAllocs = `goos: darwin
BenchmarkGussetCallNoop-18   	   10113	     70906 ns/op	       1 B/op	       1 allocs/op
BenchmarkGussetCallNoop-18   	    6039	     52022 ns/op	       1 B/op	       1 allocs/op
BenchmarkChannelHop-18       	 6398918	        53.80 ns/op	       0 B/op	       0 allocs/op
BenchmarkChannelHop-18       	 4205302	        92.43 ns/op	       0 B/op	       0 allocs/op
PASS
`

func gate() bounds { return bounds{"ChannelHop": 0, "GussetCallNoop": 1} }

func TestAllocsPassesAtTheBound(t *testing.T) {
	var out bytes.Buffer
	if err := checkAllocs(strings.NewReader(zeroAllocs), gate(), &out); err != nil {
		t.Fatalf("clean summary rejected: %v", err)
	}
}

// The step this replaced grepped for the benchmark's name and passed at any
// allocation count. A fixture with one allocating sample must now fail.
func TestAllocsFailsWhenOverTheBound(t *testing.T) {
	over := zeroAllocs + "BenchmarkChannelHop-18 \t 1000 \t 55.0 ns/op \t 8 B/op \t 1 allocs/op\n"
	err := checkAllocs(strings.NewReader(over), gate(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "ChannelHop: 1 allocs/op exceeds the bound of 0") {
		t.Fatalf("want a ChannelHop bound failure, got %v", err)
	}
	cgo := strings.Replace(zeroAllocs, "1 allocs/op", "2 allocs/op", 1)
	err = checkAllocs(strings.NewReader(cgo), gate(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "GussetCallNoop: 2 allocs/op exceeds the bound of 1") {
		t.Fatalf("want a CallNoop bound failure, got %v", err)
	}
}

func TestAllocsFailsWhenABenchmarkDidNotRun(t *testing.T) {
	for name, in := range map[string]string{
		"empty":         "",
		"no -benchmem":  "BenchmarkChannelHop-18 \t 1000 \t 55.0 ns/op\nBenchmarkGussetCallNoop-18 \t 10 \t 5 ns/op\n",
		"only one arm":  "BenchmarkChannelHop-18 \t 1000 \t 55.0 ns/op \t 0 B/op \t 0 allocs/op\n",
		"summary table": "ChannelHop-18   53.80n ± 5%\n",
	} {
		err := checkAllocs(strings.NewReader(in), gate(), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "no allocs/op sample") {
			t.Errorf("%s: a gate that did not run passed: %v", name, err)
		}
	}
}

func TestAllocsSubBenchmarkNamesAreNotPrefixMatched(t *testing.T) {
	in := "BenchmarkChannelHopBig-18 \t 1 \t 1 ns/op \t 9 B/op \t 9 allocs/op\n" + zeroAllocs
	if err := checkAllocs(strings.NewReader(in), gate(), &bytes.Buffer{}); err != nil {
		t.Fatalf("a differently named benchmark tripped the gate: %v", err)
	}
}

func TestBoundsRejectsMalformed(t *testing.T) {
	for _, v := range []string{"", "Name", "=1", "Name=x", "Name=-1"} {
		if err := (bounds{}).Set(v); err == nil {
			t.Errorf("-max %q accepted", v)
		}
	}
}

// Shapes are copied from a real `benchstat -format csv old new`.
const csvHeader = `goos: darwin
pkg: x

,old.txt,,new.txt,,,
,sec/op,CI,sec/op,CI,vs base,P
`

func TestRegressIgnoresNoiseTheOldGrepFailedOn(t *testing.T) {
	// 492ecfa: +14.55% with 9% and 7% dispersion. Significant by p-value,
	// but the intervals overlap: 1.0*(1.09)=1.09 vs 1.1455*(0.93)=1.065.
	in := csvHeader + `GussetBufferLarge-18,1.0,9%,1.1455,7%,+14.55%,p=0.004 n=10
GussetCallNoop-18,5e-05,5%,4e-05,5%,-20.00%,p=0.001 n=10
geomean,2e-06,,3e-06,,+50.00%,
`
	var out bytes.Buffer
	if err := checkRegress(strings.NewReader(in), 10, nil, &out); err != nil {
		t.Fatalf("overlapping intervals failed the gate: %v", err)
	}
	if !strings.Contains(out.String(), "noise GussetBufferLarge-18") {
		t.Errorf("the suppressed row is not reported: %q", out.String())
	}
}

func TestRegressFailsOnARealRegression(t *testing.T) {
	in := csvHeader + `GussetBufferLarge-18,1.0,2%,1.3,3%,+30.00%,p=0.000 n=10
`
	err := checkRegress(strings.NewReader(in), 10, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "GussetBufferLarge-18") {
		t.Fatalf("a +30%% row with tight intervals passed: %v", err)
	}
}

func TestRegressHonoursThresholdAndNotSignificant(t *testing.T) {
	in := csvHeader + `A-18,1.0,1%,1.05,1%,+5.00%,p=0.000 n=10
B-18,1.0,50%,3.0,1%,~,p=0.300 n=10
`
	if err := checkRegress(strings.NewReader(in), 10, nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("below-threshold and not-significant rows failed: %v", err)
	}
}

func TestRegressFailsClosedWhenItCannotRead(t *testing.T) {
	if err := checkRegress(strings.NewReader(""), 10, nil, &bytes.Buffer{}); err == nil {
		t.Error("empty benchstat output passed; nothing was compared")
	}
	allocsOnly := ",old.txt,,new.txt,,,\n,allocs/op,CI,allocs/op,CI,vs base,P\nA-18,1,0%,1,0%,~,p=1.000 n=10\n"
	if err := checkRegress(strings.NewReader(allocsOnly), 10, nil, &bytes.Buffer{}); err == nil {
		t.Error("a file with no sec/op rows passed")
	}
	noCI := csvHeader + "A-18,1.0,,2.0,,+100.00%,p=0.000 n=3\n"
	if err := checkRegress(strings.NewReader(noCI), 10, nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "interval is unreadable") {
		t.Errorf("a regression with no interval was waved through: %v", err)
	}
}

func TestRunExitCodes(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.txt")
	bad := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(good, []byte(zeroAllocs), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(strings.Replace(zeroAllocs, "0 allocs/op", "3 allocs/op", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	args := func(f string) []string {
		return []string{"allocs", "-max", "ChannelHop=0", "-max", "GussetCallNoop=1", f}
	}
	if code := run(args(good), &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Errorf("clean file exit %d", code)
	}
	if code := run(args(bad), &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Errorf("allocating file exit %d, want 1", code)
	}
	if code := run(args(filepath.Join(dir, "missing")), &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Errorf("missing file exit %d, want 1", code)
	}
	if code := run(nil, &bytes.Buffer{}, &bytes.Buffer{}); code != 2 {
		t.Errorf("no subcommand exit %d, want 2", code)
	}
}

// benchstat prints a B/op and an allocs/op table after sec/op, and the csv reader
// drops the blank line between them. Their rows must not be read as timings.
func TestRegressStopsAtTheNextTable(t *testing.T) {
	in := csvHeader + `A-18,1.0,2%,1.01,2%,~,p=0.5 n=10
geomean,1e-06,,1e-06,,+0.00%,

,old.txt,,new.txt,,,
,B/op,CI,B/op,CI,vs base,P
A-18,1.000,1600%,9.000,0%,+800.00%,p=0.000 n=10
`
	if err := checkRegress(strings.NewReader(in), 10, nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("a B/op row was gated as a timing: %v", err)
	}
}

// A renamed benchmark is a short row in the middle of the sec/op table
// (benchstat emits 3 fields for base-only, 5 for new-only, 7 for a comparison).
// It must fail closed, and it must not end the table: the +30% row after it
// is still gated. An allow-listed name does not fail by itself and still does
// not hide that later row.
func TestRegressOneSidedRowDoesNotUngateTheNext(t *testing.T) {
	const renamed = "RenamedAway-18,1.0,2%\n" // 3 fields: base only
	in := csvHeader + `Steady-18,1.0,2%,1.01,2%,~,p=0.5 n=10
` + renamed + `LaterRegress-18,1.0,2%,1.3,3%,+30.00%,p=0.000 n=10
`
	err := checkRegress(strings.NewReader(in), 10, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "LaterRegress-18") {
		t.Fatalf("the +30%% row was ungated or the gate passed: %v", err)
	}
	if !strings.Contains(err.Error(), "RenamedAway-18") {
		t.Fatalf("the one-sided row was not reported: %v", err)
	}

	allow := map[string]struct{}{"RenamedAway-18": {}}
	err = checkRegress(strings.NewReader(in), 10, allow, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "LaterRegress-18") {
		t.Fatalf("allow-listing the renamed row hid the regression: %v", err)
	}
	if strings.Contains(err.Error(), "RenamedAway-18") {
		t.Fatalf("allow-listed row still failed the gate: %v", err)
	}

	quiet := csvHeader + `Steady-18,1.0,2%,1.01,2%,~,p=0.5 n=10
` + renamed
	if err := checkRegress(strings.NewReader(quiet), 10, allow, &bytes.Buffer{}); err != nil {
		t.Fatalf("an allow-listed one-sided row failed the gate by itself: %v", err)
	}

	// Five fields is benchstat's new-only shape: a notice, not a failure, and
	// it does not end the table either.
	five := csvHeader + `Steady-18,1.0,2%,1.01,2%,~,p=0.5 n=10
NewOnly-18,,,1.0,2%
LaterRegress-18,1.0,2%,1.3,3%,+30.00%,p=0.000 n=10
`
	var out bytes.Buffer
	err = checkRegress(strings.NewReader(five), 10, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "LaterRegress-18") {
		t.Fatalf("a new-only row ungated the table: %v", err)
	}
	if strings.Contains(err.Error(), "NewOnly-18") {
		t.Fatalf("a new-only row failed the gate: %v", err)
	}
	if !strings.Contains(out.String(), "::notice title=New benchmark::NewOnly-18") {
		t.Fatalf("a new-only row was not reported as a notice:\n%s", out.String())
	}
}

// The rows Matrix 39270656716 failed on: benchmarks a push added, which its
// base cannot have. They are reported and the gate passes on the compared rows.
func TestRegressNewOnlyRowsAreNoticesNotFailures(t *testing.T) {
	in := csvHeader + `Steady-4,1.0,2%,1.01,2%,~,p=0.5 n=10
DeadlineExpiry-4,,,71.6,1%
Discard-4,,,40.0,2%
Stats-4,,,12.0,3%
`
	var out bytes.Buffer
	if err := checkRegress(strings.NewReader(in), 10, nil, &out); err != nil {
		t.Fatalf("new-only rows failed the gate: %v", err)
	}
	for _, name := range []string{"DeadlineExpiry-4", "Discard-4", "Stats-4"} {
		if !strings.Contains(out.String(), "::notice title=New benchmark::"+name) {
			t.Errorf("no notice for %s:\n%s", name, out.String())
		}
	}
}

// A rename is a base-only row plus a new-only row. The base-only half still
// fails, so a renamed benchmark is not a quiet pass.
func TestRegressRenameStillFails(t *testing.T) {
	in := csvHeader + `Steady-4,1.0,2%,1.01,2%,~,p=0.5 n=10
OldName-4,1.0,2%
NewName-4,,,1.0,2%
`
	err := checkRegress(strings.NewReader(in), 10, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "OldName-4") {
		t.Fatalf("a renamed benchmark passed: %v", err)
	}
	if strings.Contains(err.Error(), "NewName-4") {
		t.Fatalf("the new half of a rename was failed instead of noticed: %v", err)
	}
}

// Allow-list entries do not depend on the runner's core count.
func TestRegressAllowMatchesWithoutProcsSuffix(t *testing.T) {
	in := csvHeader + "Steady-4,1.0,2%,1.01,2%,~,p=0.5 n=10\nRenamedAway-4,1.0,2%\n"
	for _, allow := range []string{"RenamedAway", "RenamedAway-4"} {
		if err := checkRegress(strings.NewReader(in), 10, map[string]struct{}{allow: {}}, &bytes.Buffer{}); err != nil {
			t.Errorf("allow %q did not match RenamedAway-4: %v", allow, err)
		}
	}
	if err := checkRegress(strings.NewReader(in), 10, map[string]struct{}{"RenamedAway-8": {}}, &bytes.Buffer{}); err == nil {
		t.Error("an allow entry for a different -N matched")
	}
}

func TestRegressAllowOneSidedFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "delta.csv")
	body := csvHeader + "Steady-18,1.0,2%,1.01,2%,~,p=0.5 n=10\nRenamedAway-18,1.0,2%\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := run([]string{"regress", "-threshold", "10", path}, &bytes.Buffer{}, &stderr); code != 1 || !strings.Contains(stderr.String(), "RenamedAway-18") {
		t.Fatalf("default allow-list exit want 1 naming the row, got %d\n%s", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"regress", "-threshold", "10", "-allow-one-sided", "RenamedAway-18", path}, &bytes.Buffer{}, &stderr); code != 0 {
		t.Fatalf("allow-listed exit %d, want 0\n%s", code, stderr.String())
	}
}
