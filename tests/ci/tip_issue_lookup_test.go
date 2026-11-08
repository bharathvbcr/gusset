package ci_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// tipIssueLookupProblems names what is wrong with the toolchain-issue script in
// tip.yml. The first version took existing.data[0] from one unpaged
// listForRepo({labels:'toolchain'}) call: the issues API returns pull requests
// too, so a labelled PR or any other toolchain issue received the comment, and
// a tracking issue past page one was never found.
func tipIssueLookupProblems(tip string) []string {
	var out []string
	for _, c := range []struct{ want, why string }{
		{"!i.pull_request", "skip pull requests: the issues API lists them"},
		{"i.title === title", "match the tracking issue by exact title, not the first labelled result"},
		{"github.paginate.iterator", "page through the listing instead of reading page one only"},
		{"MAX_PAGES", "bound the listing"},
	} {
		if !strings.Contains(tip, c.want) {
			out = append(out, "tip.yml issue lookup lacks `"+c.want+"`: "+c.why)
		}
	}
	if strings.Contains(tip, "existing.data[0]") {
		out = append(out, "tip.yml still comments on existing.data[0], whatever it is")
	}
	if strings.Contains(tip, "`/sched/threads:threads`") {
		out = append(out, "tip.yml issue body names /sched/threads:threads; the code reads /sched/threads/total:threads (handle_internal_test.go, gusset.go)")
	}
	if !strings.Contains(tip, "`/sched/threads/total:threads`") {
		out = append(out, "tip.yml issue body does not name /sched/threads/total:threads")
	}
	return out
}

func TestTipIssueLookupIsExactAndBounded(t *testing.T) {
	tip := string(mustRead(t, filepath.Join(moduleRoot(t), ".github", "workflows", "tip.yml")))
	for _, p := range tipIssueLookupProblems(tip) {
		t.Error(p)
	}
}

func TestPlantedLooseTipIssueLookupIsCaught(t *testing.T) {
	loose := "const existing = await github.rest.issues.listForRepo({labels: 'toolchain'});\n" +
		"issue_number: existing.data[0].number,\n" +
		"'the `/sched/threads:threads` metric'\n"
	if got := tipIssueLookupProblems(loose); len(got) != 7 {
		t.Errorf("loose lookup: got %d problems %v, want 7", len(got), got)
	}
}
