// Package docblock replaces a delimited, generated region inside a Markdown
// document.
//
// R15 says every performance number in the docs is generated from committed raw
// data. Two tools need the same mechanism to honour it — tools/benchdoc for the
// README's benchstat table, tools/benchplot for the adoption guide's tables —
// and before this package existed only one of them had it. The other document's
// figures were typed in by hand, went stale across three recordings without
// anything noticing, and were still being read as measurements.
package docblock

import (
	"fmt"
	"strings"
)

// Markers returns the comment pair that delimits the named generated block.
func Markers(name string) (begin, end string) {
	return "<!-- " + name + ":BEGIN -->", "<!-- " + name + ":END -->"
}

// Replace swaps the contents of the named block in doc for body, keeping the
// markers themselves. path is used only for error messages.
//
// It is an error for the markers to be missing rather than something to paper
// over by appending: a document that has lost its markers has been edited in a
// way the generator cannot reason about, and writing the block somewhere
// arbitrary would leave two copies of the numbers, one of them stale. That is
// the failure this package exists to prevent, so it fails instead.
//
// A marker appearing more than once is refused for the same reason: only the
// first block would be rewritten, and the second would keep its old numbers.
func Replace(doc, path, name, body string) (string, error) {
	begin, end := Markers(name)
	for _, m := range []string{begin, end} {
		if n := strings.Count(doc, m); n > 1 {
			return "", fmt.Errorf("%s has %s more than once (%d times); keep exactly one "+
				"generated block so no stale copy survives regeneration", path, m, n)
		}
	}
	start := strings.Index(doc, begin)
	stop := strings.Index(doc, end)
	switch {
	case start < 0 || stop < 0:
		return "", fmt.Errorf("%s has no %s / %s block; add one around the generated "+
			"section so this tool has somewhere to write", path, begin, end)
	case stop < start:
		return "", fmt.Errorf("%s has %s after %s", path, begin, end)
	}
	return doc[:start] + begin + "\n" + strings.TrimRight(body, "\n") + "\n" + doc[stop:], nil
}
