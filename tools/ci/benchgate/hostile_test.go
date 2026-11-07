package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// Shapes that used to end the sec/op table, or report a header as a benchmark.
// A short row inside sec/op fails closed and stays in the table; a short
// header is still a header; B/op is not the sec/op gate.

func TestHostileShortHeaderThenOneSidedThenRegression(t *testing.T) {
	in := `,sec/op
BaseOnly-18,1.0,2%
"Weird, name-18",,,1.0,2%
LaterRegress-18,1.0,2%,1.3,3%,+30.00%,p=0.000 n=10
,B/op
BopOnly-18,9.0,1%
`
	var out bytes.Buffer
	err := checkRegress(strings.NewReader(in), 10, 1, nil, &out)
	if err == nil {
		t.Fatal("short header plus a later regression passed")
	}
	for _, want := range []string{"LaterRegress-18", "BaseOnly-18", "+30.00%"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q:\n%s", want, err.Error())
		}
	}
	// The quoted name with a comma is new-only: a notice, not a failure.
	if strings.Contains(err.Error(), "Weird, name-18") || !strings.Contains(out.String(), "Weird, name-18") {
		t.Errorf("new-only quoted row was failed or not reported:\nerr %v\nout %s", err, out.String())
	}

	allow := map[string]struct{}{"BaseOnly-18": {}, "Weird, name-18": {}}
	err = checkRegress(strings.NewReader(in), 10, 1, allow, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "LaterRegress-18") || !strings.Contains(err.Error(), "+30.00%") {
		t.Fatalf("allow-listing the one-sided names hid the +30%% row: %v", err)
	}
	if strings.Contains(err.Error(), "BopOnly-18") || strings.Contains(err.Error(), "BaseOnly-18") || strings.Contains(err.Error(), "Weird, name-18") {
		t.Fatalf("allow-listed or B/op-only row was gated: %v", err)
	}
}

func TestHostileBopOneSidedDoesNotFailSecOp(t *testing.T) {
	in := csvHeader + `Steady-18,1.0,2%,1.01,2%,~,p=0.5 n=10
,B/op
BopOnly-18,1.0,2%
`
	if err := checkRegress(strings.NewReader(in), 10, 1, nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("a one-sided row under a short ,B/op header failed the sec/op gate: %v", err)
	}
}

func TestHostileShortSecOpHeaderIsNotAnEmptyBenchmark(t *testing.T) {
	in := `,sec/op
LaterRegress-18,1.0,2%,1.3,3%,+30.00%,p=0.000 n=10
`
	err := checkRegress(strings.NewReader(in), 10, 1, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "LaterRegress-18") || !strings.Contains(err.Error(), "+30.00%") {
		t.Fatalf("the row after a 2-field ,sec/op header was not gated: %v", err)
	}
	if strings.Contains(err.Error(), "one-sided") {
		t.Fatalf("the ,sec/op header was reported as a benchmark: %v", err)
	}
}

func TestHostileEmptyAllowOneSidedIsRejected(t *testing.T) {
	for _, args := range [][]string{
		{"regress", "-allow-one-sided", "", "delta.csv"},
		{"regress", "-allow-one-sided=", "delta.csv"},
	} {
		var stderr bytes.Buffer
		code := run(args, &bytes.Buffer{}, &stderr)
		if code == 0 || !strings.Contains(stderr.String(), "empty benchmark name") {
			t.Errorf("%q: exit %d, stderr %q; want non-zero and empty benchmark name", args, code, stderr.String())
		}
	}
}

func TestHostileLoneRegressionStillFails(t *testing.T) {
	in := csvHeader + "LaterRegress-18,1.0,2%,1.3,3%,+30.00%,p=0.000 n=10\n"
	err := checkRegress(strings.NewReader(in), 10, 1, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "LaterRegress-18") || !strings.Contains(err.Error(), "+30.00%") {
		t.Fatalf("a lone +30%% row passed: %v", err)
	}
}

// n1_regressed.csv is real `benchstat -format csv` output for two single-sample
// files whose second is 1.8x and 2.1x slower. With one sample per side every
// row is "~", so a gate that only reads the delta column says "no regression".
func TestHostileSingleSampleFilesFailMinSamples(t *testing.T) {
	in, err := os.ReadFile("testdata/n1_regressed.csv")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = checkRegress(bytes.NewReader(in), 10, 6, nil, &out)
	if err == nil {
		t.Fatalf("n=1 fixture passed the gate: %s", out.String())
	}
	for _, want := range []string{"A-8", "B-8", "-min-samples 6"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q:\n%s", want, err.Error())
		}
	}
	if strings.Contains(out.String(), "no regression") {
		t.Errorf("printed a pass for a comparison of nothing: %s", out.String())
	}
}

func TestMinSamplesAcceptsSixAndRejectsUnequalSides(t *testing.T) {
	in, err := os.ReadFile("testdata/n6_steady.csv")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkRegress(bytes.NewReader(in), 10, 6, nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("n=6 fixture failed: %v", err)
	}
	if err := checkRegress(bytes.NewReader(in), 10, 7, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("n=6 passed -min-samples 7")
	}
	// benchstat prints n=8+3 when the sides differ; the smaller side counts.
	uneven := csvHeader + "A-8,1.0,2%,1.01,2%,~,p=0.5 n=8+3\n"
	err = checkRegress(strings.NewReader(uneven), 10, 6, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "3 sample") {
		t.Fatalf("n=8+3 was not judged by its smaller side: %v", err)
	}
	// No count at all cannot prove the floor, so it fails rather than passes.
	if err := checkRegress(strings.NewReader(csvHeader+"A-8,1.0,2%,1.01,2%,~,p=0.5\n"), 10, 6, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("a row with no n= passed")
	}
}

func TestRunMinSamplesFlag(t *testing.T) {
	path := "testdata/n1_regressed.csv"
	var stderr bytes.Buffer
	if code := run([]string{"regress", path}, &bytes.Buffer{}, &stderr); code == 0 || !strings.Contains(stderr.String(), "-min-samples 6") {
		t.Errorf("default run on n=1: exit %d, stderr %q", code, stderr.String())
	}
	if code := run([]string{"regress", "-min-samples", "1", path}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Errorf("-min-samples 1 on n=1: exit %d, want 0", code)
	}
	stderr.Reset()
	if code := run([]string{"regress", "-min-samples", "0", path}, &bytes.Buffer{}, &stderr); code == 0 || !strings.Contains(stderr.String(), "at least 1") {
		t.Errorf("-min-samples 0: exit %d, stderr %q", code, stderr.String())
	}
}
