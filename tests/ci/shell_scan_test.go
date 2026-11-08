package ci_test

import (
	"strings"
	"testing"
)

func runStep(shell string, body ...string) []string {
	l := []string{"      - name: s"}
	if shell != "" {
		l = append(l, "        shell: "+shell)
	}
	l = append(l, "        run: |")
	for _, b := range body {
		l = append(l, "          "+b)
	}
	return append(l, "      - name: next", "        run: true")
}

// Compliant steps the tee rule must not flag. Each is a form a real workflow
// can use, and each was flagged (or would be) by a regex over one line.
func TestTeeRuleAcceptsCompliantForms(t *testing.T) {
	cases := []struct {
		name string
		step []string
	}{
		{"set -o errexit -o pipefail", runStep("", "set -o errexit -o pipefail", "go test | tee out")},
		{"set +e -o pipefail", runStep("", "set +e -o pipefail", "go test | tee out")},
		{"set +o pipefail; set -o pipefail on one line", runStep("", "set +o pipefail; set -o pipefail", "go test | tee out")},
		{"set -o pipefail; cmd | tee on one line", runStep("", "set -o pipefail; go test | tee out")},
		{"set -euo pipefail", runStep("", "set -euo pipefail", "go test |& tee out")},
		{"tee inside printf", runStep("", `printf '%s\n' 'x | tee y'`)},
		{"tee inside echo", runStep("", `echo "a | tee b"`)},
		{"tee in a trailing comment", runStep("", "go test # | tee out")},
		{"tee in a comment line", runStep("", "# go test | tee out")},
		{"|| tee is not a pipe", runStep("", "false || tee out")},
	}
	for _, c := range cases {
		if v := teeWithoutPipefailViolations("x", c.step); len(v) != 0 {
			t.Errorf("%s: a compliant step was flagged: %v", c.name, v)
		}
	}
}

// Forms the tee rule must flag.
func TestTeeRuleFlagsWhatARegexMissed(t *testing.T) {
	cases := []struct {
		name string
		step []string
	}{
		{"| /usr/bin/tee", runStep("", "go test | /usr/bin/tee out")},
		{"| command tee", runStep("", "go test | command tee out")},
		{"| sudo tee", runStep("", "go test | sudo tee out")},
		{"cmd | with tee on the next line", runStep("", "go test |", "  tee out")},
		{"cmd |& with tee on the next line", runStep("", "go test |&", "  tee out")},
		{"cmd | continued, tee on the next line", runStep("", "go test | \\", "  tee out")},
		{"set -o errexit alone", runStep("", "set -o errexit", "go test | tee out")},
		{"a quoted << is not a heredoc", runStep("", `echo "use cat <<EOF"`, "go test | tee out")},
		{"an arithmetic << is not a heredoc", runStep("", "x=$((x<<y))", "go test | tee out")},
		{"a heredoc body is read as commands", runStep("", "cat <<EOF", "a | tee b", "EOF")},
		{"set -- -o pipefail sets positional parameters", runStep("", "set -- -o pipefail", "go test | tee out")},
		{"set -o pipefail inside a one-line function", runStep("", "f() { set -o pipefail; }", "go test | tee out")},
		{"set +o pipefail last on a line", runStep("", "set -o pipefail; set +o pipefail", "go test | tee out")},
	}
	for _, c := range cases {
		if len(teeWithoutPipefailViolations("x", c.step)) == 0 {
			t.Errorf("%s: the planted violation was not caught", c.name)
		}
	}
}

// The one reader (logicalLines) joins continuations, including a trailing pipe
// or &&, and drops trailing comments, without touching `$#` or quoted `#`.
func TestLogicalLinesJoinsOperatorContinuationsAndDropsComments(t *testing.T) {
	got := logicalLines([]string{
		"a \\", "  b",
		`echo "x | tee"  # c | tee`,
		"c |", "  d",
		"e |&", "  f",
		"g &&", "  h",
		"n=${#x} # trailing",
		`p 'a # b' # real`,
	})
	var texts []string
	for _, g := range got {
		texts = append(texts, strings.Join(strings.Fields(g.text), " "))
	}
	want := []string{"a b", "echo", "c | d", "e |& f", "g && h", "n=${#x}", "p 'a # b'"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Errorf("logicalLines = %q, want %q", texts, want)
	}
}
