package ci_test

import (
	"regexp"
	"strings"
	"testing"
)

// A small shell-aware line reader for the CI rules that look at what a `run:`
// body does. It is still a line scanner, not a shell parser. What it handles:
// backslash continuations and a trailing `|`, `&&` or `||` (the command goes on
// on the next line), heredoc bodies (not commands), quoted strings and trailing
// `#` comments (not commands), and `;`, `&&`, `||` command boundaries inside one
// line. What it does not, by design:
//   - `xargs tee`, `find -exec tee`: tee is run by another program, not piped to.
//   - `set -o pipefail` inside a function or an `if` arms the whole script; the
//     reader does not track scope, so the rule errs toward accepting it.
//   - `$(...)` and subshell scope: `( cd d && cargo build ); cargo test` is read
//     as if the second cargo ran in d.

var heredocStart = regexp.MustCompile(`(?:^|[^<])<<(-?)\s*(?:'([A-Za-z_]\w*)'|"([A-Za-z_]\w*)"|([A-Za-z_]\w*))`)

// stripShellLine blanks the contents of quoted strings and drops a trailing
// `#` comment. inQuote carries a quote left open at the end of a physical line
// (a multi-line string) into the next one.
func stripShellLine(line string, inQuote byte) (string, byte) {
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote != 0:
			if c == '\\' && inQuote == '"' && i+1 < len(line) {
				i++
				continue
			}
			if c == inQuote {
				inQuote = 0
				b.WriteByte(c)
			}
		case c == '\'' || c == '"':
			inQuote = c
			b.WriteByte(c)
		case c == '\\' && i+1 < len(line):
			b.WriteByte(c)
			i++
			b.WriteByte(line[i])
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return b.String(), inQuote
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), inQuote
}

// scriptLines returns the logical command lines of a script body: heredoc
// bodies removed, strings blanked, comments dropped, continuations joined.
func scriptLines(body []string) []logicalLine {
	var out []logicalLine
	var inQuote byte
	heredoc := ""
	for i := 0; i < len(body); i++ {
		if heredoc != "" {
			// The body keeps its YAML indentation, which the YAML parser would
			// have removed, so a terminator is compared with it trimmed.
			if strings.TrimSpace(body[i]) == heredoc {
				heredoc = ""
			}
			continue
		}
		start := i + 1
		text, q := stripShellLine(body[i], inQuote)
		inQuote = q
		if m := heredocStart.FindStringSubmatch(body[i]); m != nil && q == 0 {
			heredoc = m[2] + m[3] + m[4]
		}
		for continues(text) && i+1 < len(body) && heredoc == "" {
			next, q := stripShellLine(body[i+1], inQuote)
			inQuote = q
			i++
			text = strings.TrimSuffix(strings.TrimRight(text, " \t"), `\`) + " " + strings.TrimSpace(next)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, logicalLine{start, text})
	}
	return out
}

// continues says a command goes on on the next physical line: a trailing
// backslash, or an operator that cannot end a command.
func continues(text string) bool {
	t := strings.TrimRight(text, " \t")
	return strings.HasSuffix(t, `\`) || strings.HasSuffix(t, "|") || strings.HasSuffix(t, "|&") || strings.HasSuffix(t, "&&")
}

var simpleSplit = regexp.MustCompile(`;|&&|\|\|`)

// shellSegments splits a command line at `;`, `&&` and `||` but not at a single
// `|`, so a pipeline stays whole.
func shellSegments(s string) []string { return simpleSplit.Split(s, -1) }

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
		{"tee inside a heredoc body", runStep("", "cat <<EOF > f", "a | tee b", "EOF")},
		{"tee inside a quoted heredoc body", runStep("", "cat <<'EOF'", "a | tee b", "EOF", "true")},
		{"tee inside an indented <<- heredoc body", runStep("", "cat <<-EOT", "\t\ta | tee b", "\tEOT")},
		{"tee inside printf", runStep("", `printf '%s\n' 'x | tee y'`)},
		{"tee inside echo", runStep("", `echo "a | tee b"`)},
		{"tee inside a multi-line string", runStep("", "printf '", "a | tee b", "'")},
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
		{"set +o pipefail last on a line", runStep("", "set -o pipefail; set +o pipefail", "go test | tee out")},
		{"a heredoc ends, then a real tee", runStep("", "cat <<EOF", "x", "EOF", "go test | tee out")},
	}
	for _, c := range cases {
		if len(teeWithoutPipefailViolations("x", c.step)) == 0 {
			t.Errorf("%s: the planted violation was not caught", c.name)
		}
	}
}

func TestScriptLinesReadsHeredocsQuotesAndContinuations(t *testing.T) {
	got := scriptLines([]string{
		"a \\", "  b",
		"cat <<EOF",
		"not | tee",
		"EOF",
		`echo "x | tee"  # c | tee`,
		"c |",
		"  d",
	})
	var texts []string
	for _, g := range got {
		texts = append(texts, strings.Join(strings.Fields(g.text), " "))
	}
	want := []string{"a b", "cat <<EOF", `echo ""`, "c | d"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Errorf("scriptLines = %q, want %q", texts, want)
	}
}
