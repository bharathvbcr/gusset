package main

import (
	"bytes"
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
	err := checkRegress(strings.NewReader(in), 10, nil, &bytes.Buffer{})
	if err == nil {
		t.Fatal("short header plus a later regression passed")
	}
	for _, want := range []string{"LaterRegress-18", "BaseOnly-18", "Weird, name-18", "+30.00%"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q:\n%s", want, err.Error())
		}
	}

	allow := map[string]struct{}{"BaseOnly-18": {}, "Weird, name-18": {}}
	err = checkRegress(strings.NewReader(in), 10, allow, &bytes.Buffer{})
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
	if err := checkRegress(strings.NewReader(in), 10, nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("a one-sided row under a short ,B/op header failed the sec/op gate: %v", err)
	}
}

func TestHostileShortSecOpHeaderIsNotAnEmptyBenchmark(t *testing.T) {
	in := `,sec/op
LaterRegress-18,1.0,2%,1.3,3%,+30.00%,p=0.000 n=10
`
	err := checkRegress(strings.NewReader(in), 10, nil, &bytes.Buffer{})
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
	err := checkRegress(strings.NewReader(in), 10, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "LaterRegress-18") || !strings.Contains(err.Error(), "+30.00%") {
		t.Fatalf("a lone +30%% row passed: %v", err)
	}
}
