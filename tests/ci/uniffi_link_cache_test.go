package ci_test

import (
	"regexp"
	"strings"
	"testing"
)

// `make test-uniffi-link` links the umbrella archive that CGO_LDFLAGS points
// at, and the Go build cache does not hash that archive: internal/ffi/gen.go
// hashes only target/{release,debug}/libgusset.a. Linked to a fixed output
// (`go test -c -o`), a rebuilt umbrella was reused unchanged, because nothing
// the cache keys on had moved (demonstrated in the card's evidence: the linker
// did not run after a byte of the archive changed). -ldflags is part of the
// link's cache key, so the recipe hashes the archive and passes the digest
// there. The rule is a pure function over the recipe's lines.

const (
	uniffiRecipeTarget = "test-uniffi-link:"
	umbrellaArchive    = "examples/uniffi_bindgen_go/rust/target/debug/libgusset.a"
)

var (
	// NAME=$$( ... sha256sum|shasum -a 256 ... $(UNIFFI_UMBRELLA) ... )
	umbrellaHashAssign = regexp.MustCompile(`(\w+)=\$\$\([^;]*(?:sha256sum|shasum -a 256)[^;]*\$\(UNIFFI_UMBRELLA\)`)
	ldflagsFlag        = regexp.MustCompile(`-ldflags\s+"([^"]*)"`)
)

// recipeLines returns the recipe of one Makefile target, backslash
// continuations joined: a recipe line is one shell, so a variable set on one
// line does not exist on the next.
func recipeLines(mk []string, target string) []string {
	var out []string
	in := false
	for i := 0; i < len(mk); i++ {
		l := mk[i]
		if !in {
			in = strings.HasPrefix(l, target)
			continue
		}
		if !strings.HasPrefix(l, "\t") {
			if strings.TrimSpace(l) == "" || strings.HasPrefix(l, "#") {
				continue
			}
			break
		}
		text := strings.TrimSpace(l)
		for strings.HasSuffix(text, `\`) && i+1 < len(mk) {
			i++
			text = strings.TrimSuffix(text, `\`) + " " + strings.TrimSpace(mk[i])
		}
		out = append(out, text)
	}
	return out
}

// staleUmbrellaViolations reports why the recipe could link a stale umbrella
// archive: the link command must carry the archive's own digest in -ldflags,
// computed in the same shell line, from the archive that CGO_LDFLAGS links.
func staleUmbrellaViolations(recipe []string, vars map[string][]string) []string {
	var out []string
	if got := strings.Join(vars["UNIFFI_UMBRELLA"], " "); got != umbrellaArchive {
		out = append(out, "UNIFFI_UMBRELLA is "+got+", want "+umbrellaArchive+" (the archive CGO_LDFLAGS -L resolves -lgusset to)")
	}
	found := false
	for _, l := range recipe {
		if !strings.Contains(l, "go test") || !strings.Contains(l, "gusset_uniffi_engine") {
			continue
		}
		found = true
		if !strings.Contains(l, "-L$(CURDIR)/examples/uniffi_bindgen_go/rust/target/debug") {
			out = append(out, "the go test line does not link from the umbrella's target/debug: "+l)
		}
		m := ldflagsFlag.FindStringSubmatch(l)
		if m == nil || !strings.Contains(m[1], "-X ") {
			out = append(out, "the go test line passes no -ldflags \"-X ...\", so a changed umbrella archive does not change the link's cache key: "+l)
			continue
		}
		h := umbrellaHashAssign.FindStringSubmatch(l)
		if h == nil {
			out = append(out, "the archive digest is not computed (sha256sum or shasum -a 256 of $(UNIFFI_UMBRELLA)) in the same shell line as go test: "+l)
			continue
		}
		if !strings.Contains(m[1], "$$"+h[1]) {
			out = append(out, "-ldflags does not carry $$"+h[1]+", the archive digest: "+l)
		}
	}
	if !found {
		out = append(out, "no recipe line runs go test with -tags gusset_uniffi_engine; the check examined nothing")
	}
	return out
}

func TestUniffiLinkRecipeKeysTheLinkOnTheUmbrellaArchive(t *testing.T) {
	mk := readLines(t, "../../Makefile")
	recipe := recipeLines(mk, uniffiRecipeTarget)
	if len(recipe) == 0 {
		t.Fatalf("Makefile has no %s recipe", uniffiRecipeTarget)
	}
	for _, v := range staleUmbrellaViolations(recipe, makefileVars(mk)) {
		t.Error(v)
	}
}

func TestPlantedStaleUmbrellaRecipesAreCaught(t *testing.T) {
	vars := map[string][]string{"UNIFFI_UMBRELLA": {umbrellaArchive}}
	const link = `CGO_LDFLAGS="-L$(CURDIR)/examples/uniffi_bindgen_go/rust/target/debug" go test -count=1 -tags gusset_uniffi_engine `
	good := []string{
		`umbrella_sha=$$( { sha256sum $(UNIFFI_UMBRELLA) 2>/dev/null || shasum -a 256 $(UNIFFI_UMBRELLA); } | cut -d' ' -f1 ); ` + link + `-ldflags "-X x.y=$$umbrella_sha" -v ./...`,
	}
	if v := staleUmbrellaViolations(good, vars); len(v) != 0 {
		t.Fatalf("the compliant recipe was flagged: %v", v)
	}
	cases := []struct {
		name   string
		recipe []string
		vars   map[string][]string
	}{
		{"no -ldflags (the old recipe)", []string{link + `-v ./...`}, vars},
		{"-ldflags without a -X", []string{`h=$$(sha256sum $(UNIFFI_UMBRELLA)); ` + link + `-ldflags "-s" ./...`}, vars},
		{"digest computed on another line", []string{`h=$$(sha256sum $(UNIFFI_UMBRELLA))`, link + `-ldflags "-X x.y=$$h" ./...`}, vars},
		{"digest of a different file", []string{`h=$$(sha256sum target/debug/libgusset.a); ` + link + `-ldflags "-X x.y=$$h" ./...`}, vars},
		{"digest computed but not passed", []string{`h=$$(sha256sum $(UNIFFI_UMBRELLA)); ` + link + `-ldflags "-X x.y=const" ./...`}, vars},
		{"umbrella variable points at the engine archive", good, map[string][]string{"UNIFFI_UMBRELLA": {"target/debug/libgusset.a"}}},
		{"no go test line", []string{"true"}, vars},
	}
	for _, c := range cases {
		if len(staleUmbrellaViolations(c.recipe, c.vars)) == 0 {
			t.Errorf("%s: the planted stale-umbrella recipe was not caught", c.name)
		}
	}
}

func TestRecipeLinesJoinContinuationsAndStopAtTheNextTarget(t *testing.T) {
	got := recipeLines([]string{"x:", "\tone \\", "\t  two", "# c", "\tthree", "", "next:", "\tfour"}, "x:")
	if strings.Join(got, "|") != "one  two|three" {
		t.Errorf("recipeLines = %q", got)
	}
}
