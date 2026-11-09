package ci_test

import (
	"regexp"
	"strings"
	"testing"
)

// `make ci-local` is what a contributor runs before pushing, and AGENTS.md says
// what it runs. test-uniffi-link is in matrix.yml's unit job, so it must be here
// too: without it the typed Go client is never linked against the compiled
// engine locally (card ft-e20c9192, criterion 4).

var ciLocalRule = regexp.MustCompile(`(?m)^ci-local:[ \t]*(.*)$`)

// ciLocalPrereqs returns the prerequisites of the ci-local target, backslash
// continuations joined; nil when the target is not defined.
func ciLocalPrereqs(mk []string) []string {
	for i := 0; i < len(mk); i++ {
		m := ciLocalRule.FindStringSubmatch(mk[i])
		if m == nil {
			continue
		}
		text := m[1]
		for strings.HasSuffix(text, `\`) && i+1 < len(mk) {
			i++
			text = strings.TrimSuffix(text, `\`) + " " + strings.TrimSpace(mk[i])
		}
		return strings.Fields(text)
	}
	return nil
}

func ciLocalMissing(mk []string, want ...string) []string {
	have := map[string]bool{}
	for _, p := range ciLocalPrereqs(mk) {
		have[p] = true
	}
	var out []string
	for _, w := range want {
		if !have[w] {
			out = append(out, w)
		}
	}
	return out
}

func TestCILocalRunsTheUniffiLink(t *testing.T) {
	mk := readLines(t, "../../Makefile")
	if len(ciLocalPrereqs(mk)) == 0 {
		t.Fatal("Makefile defines no ci-local prerequisites; the check examined nothing")
	}
	if miss := ciLocalMissing(mk, "build", "test", "test-uniffi-link", "lint", "docs-check"); len(miss) != 0 {
		t.Errorf("make ci-local no longer depends on %v; matrix.yml's unit job and AGENTS.md's hand-off checklist run them", miss)
	}
}

func TestPlantedCILocalWithoutTheUniffiLinkIsCaught(t *testing.T) {
	cases := []struct {
		name string
		mk   []string
		miss int
	}{
		{"complete", []string{"ci-local: build test test-uniffi-link lint docs-check", "\ttrue"}, 0},
		{"link removed", []string{"ci-local: build test lint docs-check", "\ttrue"}, 1},
		{"link removed, continued", []string{"ci-local: build test \\", "\tlint docs-check", "\ttrue"}, 1},
		{"link on a continuation", []string{"ci-local: build test \\", "\ttest-uniffi-link lint docs-check"}, 0},
		{"link only in a recipe line", []string{"ci-local: build test lint docs-check", "\tmake test-uniffi-link"}, 1},
		{"a different target holds it", []string{"other: test-uniffi-link", "ci-local: build test lint docs-check"}, 1},
		{"no ci-local at all", []string{"other: build"}, 5},
	}
	for _, c := range cases {
		got := ciLocalMissing(c.mk, "build", "test", "test-uniffi-link", "lint", "docs-check")
		if len(got) != c.miss {
			t.Errorf("%s: missing %v, want %d missing", c.name, got, c.miss)
		}
	}
}
