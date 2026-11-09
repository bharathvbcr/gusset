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
		{"escaped apostrophe outside quotes opens no string", runStep("", `echo it\'s; go test | tee out`)},
		{"escaped double quotes outside quotes open no string", runStep("", `echo \"x\"; go test | tee out`)},
		{"echo with escaped quotes, then a pipe into tee", runStep("", `echo "say \"hi\" now" | tee out`)},
		{"a quoted << is not a heredoc", runStep("", `echo "use cat <<EOF"`, "go test | tee out")},
		{"an arithmetic << is not a heredoc", runStep("", "x=$((x<<y))", "go test | tee out")},
		{"an uncalled one-line function sets pipefail (f is never called)", runStep("", "f() { set -o pipefail; }", "go test | tee out")},
		{"set -- -o pipefail sets positional parameters", runStep("", "set -- -o pipefail", "go test | tee out")},
		{"set +o pipefail last on a line", runStep("", "set -o pipefail; set +o pipefail", "go test | tee out")},
	}
	for _, c := range cases {
		if len(teeWithoutPipefailViolations("x", c.step)) == 0 {
			t.Errorf("%s: the planted violation was not caught", c.name)
		}
	}
}

// Known limits of the line scanner that pin its CURRENT over-flagging: each of
// these steps is compliant shell that the rule flags anyway (see the limits in
// teeWithoutPipefailViolations). They are not requirements. A later, better
// scanner may stop flagging them; it should then change this table, and no
// test about what the rule must flag has to move.
func TestTeeRuleKnownOverFlagging(t *testing.T) {
	cases := []struct {
		name string
		step []string
	}{
		{"a heredoc body is read as commands", runStep("", "cat <<EOF", "a | tee b", "EOF")},
		{"a one-line function that sets pipefail is then called", runStep("", "f() { set -o pipefail; }; f", "go test | tee out")},
		{"set -o pipefail inside a subshell paren", runStep("", "( set -o pipefail; go test | tee out )")},
		{"a multi-line string is read line by line", runStep("", "printf '", "a | tee b", "'")},
	}
	for _, c := range cases {
		if len(teeWithoutPipefailViolations("x", c.step)) == 0 {
			t.Logf("%s: no longer flagged; the known-limits table can drop it", c.name)
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
