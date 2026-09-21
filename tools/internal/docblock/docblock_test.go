package docblock

import (
	"strings"
	"testing"
)

const doc = `# Title

Prose above.

<!-- BENCHDOC:BEGIN -->
stale numbers
<!-- BENCHDOC:END -->

Prose below.
`

func TestReplaceSwapsOnlyTheBlock(t *testing.T) {
	got, err := Replace(doc, "README.md", "BENCHDOC", "| a | b |\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "stale numbers") {
		t.Error("old block survived the replacement")
	}
	for _, want := range []string{"Prose above.", "Prose below.", "| a | b |",
		"<!-- BENCHDOC:BEGIN -->", "<!-- BENCHDOC:END -->"} {
		if !strings.Contains(got, want) {
			t.Errorf("result is missing %q:\n%s", want, got)
		}
	}
}

// Idempotence is what -check relies on: it compares the regenerated document
// against the one on disk and calls any difference "stale". If Replace added or
// dropped a newline on each pass, every check would fail on a current document.
func TestReplaceIsIdempotent(t *testing.T) {
	once, err := Replace(doc, "README.md", "BENCHDOC", "body\n")
	if err != nil {
		t.Fatal(err)
	}
	twice, err := Replace(once, "README.md", "BENCHDOC", "body\n")
	if err != nil {
		t.Fatal(err)
	}
	if once != twice {
		t.Errorf("second pass changed the document:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

// Trailing newlines in the body must not accumulate, for the same reason.
func TestReplaceNormalisesBodyNewlines(t *testing.T) {
	a, err := Replace(doc, "README.md", "BENCHDOC", "body")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Replace(doc, "README.md", "BENCHDOC", "body\n\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("body newline count changed the result; -check would report a current document as stale")
	}
}

func TestReplaceRefusesMissingMarkers(t *testing.T) {
	_, err := Replace("# Title\n\nNo markers here.\n", "docs/choosing.md", "BENCHPLOT", "body")
	if err == nil {
		t.Fatal("accepted a document with no block; the numbers would have gone nowhere")
	}
	for _, want := range []string{"docs/choosing.md", "BENCHPLOT:BEGIN", "BENCHPLOT:END"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestReplaceRefusesReversedMarkers(t *testing.T) {
	reversed := "<!-- X:END -->\nbody\n<!-- X:BEGIN -->\n"
	if _, err := Replace(reversed, "doc.md", "X", "new"); err == nil {
		t.Fatal("accepted markers in the wrong order")
	}
}

func TestMarkers(t *testing.T) {
	begin, end := Markers("BENCHPLOT")
	if begin != "<!-- BENCHPLOT:BEGIN -->" || end != "<!-- BENCHPLOT:END -->" {
		t.Errorf("Markers = %q / %q", begin, end)
	}
}
