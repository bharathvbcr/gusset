package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// header is the per-section preamble benchstat prints above each unit's rows.
func header(unit string) string {
	return "                 │ bench/results/x.txt │\n" +
		"                 │       " + unit + "        │\n"
}

const meta = "goos: darwin\ngoarch: arm64\npkg: github.com/bharathvbcr/gusset/bench\ncpu: Apple M5 Pro\n"

func TestParseBenchstat(t *testing.T) {
	for _, tc := range []struct {
		name     string
		out      string
		wantRows []row
		wantMeta []string
		wantErr  string
	}{
		{
			name: "three sections merge into one row per benchmark",
			out: meta + header("sec/op") +
				"GussetCallNoop-18      12.98µ ± 3%\n" +
				"ChannelHop-18          16.56n ± 2%\n" +
				"geomean                3.848µ\n\n" +
				header("B/op") +
				"GussetCallNoop-18      161.0 ± 0%\n" +
				"ChannelHop-18          0.000 ± 0%\n" +
				"geomean                          ¹\n" +
				"¹ summaries must be >0 to compute geomean\n\n" +
				header("allocs/op") +
				"GussetCallNoop-18      3.000 ± 33%\n" +
				"ChannelHop-18          0.000 ±  0%\n",
			wantRows: []row{
				{name: "GussetCallNoop", secOp: "12.98µ ± 3%", bytesOp: "161.0 ± 0%", allocs: "3.000 ± 33%"},
				{name: "ChannelHop", secOp: "16.56n ± 2%", bytesOp: "0.000 ± 0%", allocs: "0.000 ± 0%"},
			},
			wantMeta: strings.Split(strings.TrimSpace(meta), "\n"),
		},
		{
			// The old prefix filter dropped this row entirely, contradicting the
			// promise that an unmapped benchmark shows up as an ugly row.
			name: "a benchmark outside the Gusset and Channel prefixes is kept",
			out: header("sec/op") +
				"GussetCallNoop-18      12.98µ ± 3%\n" +
				"RawCgoNoop-18          1.200µ ± 1%\n",
			wantRows: []row{
				{name: "GussetCallNoop", secOp: "12.98µ ± 3%"},
				{name: "RawCgoNoop", secOp: "1.200µ ± 1%"},
			},
		},
		{
			// LastIndex("-") alone truncated this to "GussetBuffer/size".
			name: "only a numeric -N suffix is stripped",
			out: header("sec/op") +
				"GussetBuffer/size-large-18    19.87µ ± 1%\n" +
				"GussetBuffer/size-small       1.000µ ± 1%\n" +
				"GussetCallNoop                12.98µ ± 3%\n",
			wantRows: []row{
				{name: "GussetBuffer/size-large", secOp: "19.87µ ± 1%"},
				{name: "GussetBuffer/size-small", secOp: "1.000µ ± 1%"},
				{name: "GussetCallNoop", secOp: "12.98µ ± 3%"},
			},
		},
		{
			// benchstat repeats the header lines for each package. Once the prefix
			// filter is gone they must not become rows named "pkg" or "cpu".
			name: "metadata repeated inside a section is metadata, not a row",
			out: header("sec/op") +
				"GussetCallNoop-18      12.98µ ± 3%\n" +
				"pkg: github.com/bharathvbcr/gusset/other\n" +
				"cpu: Apple M5 Pro\n" +
				"GussetOther-18         2.000µ ± 1%\n",
			wantRows: []row{
				{name: "GussetCallNoop", secOp: "12.98µ ± 3%"},
				{name: "GussetOther", secOp: "2.000µ ± 1%"},
			},
			wantMeta: []string{"pkg: github.com/bharathvbcr/gusset/other", "cpu: Apple M5 Pro"},
		},
		{
			// A custom metric's section used to inherit the previous unit, so its
			// values overwrote the allocs/op column.
			name: "a section for an unknown unit does not overwrite a known column",
			out: header("sec/op") +
				"GussetCallNoop-18      12.98µ ± 3%\n\n" +
				header("allocs/op") +
				"GussetCallNoop-18      3.000 ± 33%\n\n" +
				header("peak-threads") +
				"GussetCallNoop-18      9.000 ± 0%\n",
			wantRows: []row{
				{name: "GussetCallNoop", secOp: "12.98µ ± 3%", allocs: "3.000 ± 33%"},
			},
		},
		{
			name: "small-sample footnote markers are trimmed from the value",
			out: header("sec/op") +
				"GussetCallNoop-18      21.55µ ± ∞ ¹\n" +
				"¹ need >= 6 samples for confidence interval at level 0.95\n",
			wantRows: []row{{name: "GussetCallNoop", secOp: "21.55µ ± ∞"}},
		},
		{
			name: "a benchmark with no sec/op summary fails rather than rendering a blank cell",
			out: header("B/op") +
				"GussetCallNoop-18      161.0 ± 0%\n",
			wantErr: "no sec/op summary",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, gotMeta, err := parseBenchstat(tc.out)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q (rows %+v)", err, tc.wantErr, rows)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rows, tc.wantRows) {
				t.Errorf("rows:\n got  %+v\n want %+v", rows, tc.wantRows)
			}
			if !reflect.DeepEqual(gotMeta, tc.wantMeta) {
				t.Errorf("meta:\n got  %q\n want %q", gotMeta, tc.wantMeta)
			}
		})
	}
}

func TestResultsFile(t *testing.T) {
	dir := t.TempDir()
	touch := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if _, err := resultsFile(dir, nil); err == nil || !strings.Contains(err.Error(), "make bench") {
		t.Errorf("empty dir: err = %v, want a pointer to `make bench`", err)
	}

	only := touch("darwin-arm64.txt")
	if got, err := resultsFile(dir, nil); err != nil || got != only {
		t.Errorf("one file: got %q, %v; want %q", got, err, only)
	}

	linux := touch("linux-amd64.txt")
	if _, err := resultsFile(dir, nil); err == nil || !strings.Contains(err.Error(), "benchdoc [-check] <results-file>") {
		t.Errorf("two files, none named: err = %v, want it to say how to name one", err)
	}

	// The case the old error message asked for and the tool had no way to take.
	if got, err := resultsFile(dir, []string{linux}); err != nil || got != linux {
		t.Errorf("explicit file among several: got %q, %v; want %q", got, err, linux)
	}

	if _, err := resultsFile(dir, []string{only, linux}); err == nil {
		t.Error("two explicit files accepted; the README can describe only one")
	}
	if _, err := resultsFile(dir, []string{filepath.Join(dir, "missing.txt")}); err == nil {
		t.Error("a results file that does not exist was accepted")
	}
}
