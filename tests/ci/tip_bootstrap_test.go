package ci_test

// The weekly tip job's bootstrap contract.
//
// Run 35594759554 failed in "Set up Go Tip" before any Gusset test ran:
//
//	Building Go cmd/dist using /opt/hostedtoolcache/go/1.24.13/x64. (go1.24.13 linux/amd64)
//	found packages main (build.go) and building_Go_requires_Go_1_26_0_or_later (notgo126.go) in /home/runner/gotip/src/cmd/dist
//
// Go tip (1.28) requires a bootstrap toolchain >= Go 1.26.0. That floor is
// src/make.bash's bootgo=1.26.0 and src/cmd/dist/notgo126.go's
// //go:build !go1.26. make.bash uses whatever `go` is on PATH when
// GOROOT_BOOTSTRAP is unset, and ubuntu-latest still ships Go 1.24 in the
// hosted toolcache. This test fails on a workflow that builds tip that way.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestTipWorkflowBootstrapsFromGo1_26OrNewer(t *testing.T) {
	root := moduleRoot(t)
	tip := string(mustRead(t, filepath.Join(root, ".github", "workflows", "tip.yml")))
	matrix := string(mustRead(t, filepath.Join(root, ".github", "workflows", "matrix.yml")))

	makeAt := strings.Index(tip, "./make.bash")
	if makeAt < 0 {
		t.Fatal("tip.yml no longer builds Go tip with ./make.bash")
	}
	before := tip[:makeAt]
	setupAt := strings.LastIndex(before, "actions/setup-go@")
	if setupAt < 0 {
		t.Fatal("tip.yml runs ./make.bash with no actions/setup-go before it. " +
			"Run 35594759554 bootstrapped from the runner image: " +
			"Building Go cmd/dist using /opt/hostedtoolcache/go/1.24.13/x64. (go1.24.13 linux/amd64); " +
			"found packages main (build.go) and building_Go_requires_Go_1_26_0_or_later (notgo126.go)")
	}

	matrixVer := firstGoVersion(t, matrix)
	tipVer := firstGoVersion(t, before[setupAt:])
	if tipVer != matrixVer {
		t.Fatalf("tip bootstrap go-version %q must match the matrix pin %q", tipVer, matrixVer)
	}
	minor, ok := goMinor(tipVer)
	if !ok || minor < 26 {
		t.Fatalf("bootstrap %q is below Go 1.26.0, which src/cmd/dist/notgo126.go refuses", tipVer)
	}
	if !strings.Contains(before[setupAt:], "GOROOT_BOOTSTRAP=") {
		t.Fatal("make.bash must be invoked with GOROOT_BOOTSTRAP set to the setup-go tree; " +
			"unset, make.bash takes the first go on PATH, and ubuntu-latest's toolcache is still Go 1.24")
	}
	// The shell refuses a resolved toolchain older than the floor before it
	// spends minutes cloning tip. A setup-go range that silently resolves to
	// 1.24 would otherwise die inside cmd/dist with the package-name error.
	if !strings.Contains(before, "-lt 26") {
		t.Fatal("the bootstrap step must reject a toolchain older than Go 1.26 before ./make.bash")
	}
	if !strings.Contains(before, "notgo126.go") {
		t.Fatal("the bootstrap step must name notgo126.go; that is the file that rejected Go 1.24.13")
	}
}

func TestGoMinorParsesSetupGoVersionSpecs(t *testing.T) {
	cases := []struct {
		spec string
		want int
		ok   bool
	}{
		{spec: "1.27.x", want: 27, ok: true},
		{spec: "1.26.0", want: 26, ok: true},
		{spec: "1.24.13", want: 24, ok: true},
		{spec: ">=1.26.0", want: 26, ok: true},
		{spec: "stable", ok: false},
		{spec: "1", ok: false},
	}
	for _, tc := range cases {
		got, ok := goMinor(tc.spec)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("goMinor(%q) = (%d, %v), want (%d, %v)", tc.spec, got, ok, tc.want, tc.ok)
		}
	}
}

// goMinor reads the minor from a setup-go version spec. A leading range
// operator is stripped so ">=1.26.0" and "1.27.x" share one parser. Specs
// that do not name a 1.N release ("stable", a bare major) are refused: the
// tip floor is a number, and an alias would hide a drop below it.
func goMinor(spec string) (int, bool) {
	spec = strings.TrimSpace(spec)
	for _, prefix := range []string{">=", "<=", "^", "~"} {
		spec = strings.TrimPrefix(spec, prefix)
	}
	parts := strings.Split(spec, ".")
	if len(parts) < 2 || parts[0] != "1" {
		return 0, false
	}
	minor, err := strconv.Atoi(strings.TrimRight(parts[1], "xX"))
	if err != nil {
		return 0, false
	}
	return minor, true
}

func firstGoVersion(t *testing.T, s string) string {
	t.Helper()
	const key = "go-version:"
	i := strings.Index(s, key)
	if i < 0 {
		t.Fatalf("no go-version in the workflow fragment")
	}
	rest := strings.TrimSpace(s[i+len(key):])
	if rest == "" {
		t.Fatal("empty go-version")
	}
	quote := rest[0]
	if quote != '\'' && quote != '"' {
		t.Fatalf("go-version must be quoted so YAML does not trim it, got %q", rest)
	}
	end := strings.IndexByte(rest[1:], byte(quote))
	if end < 0 {
		t.Fatal("unterminated go-version")
	}
	return rest[1 : 1+end]
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test working directory")
		}
		dir = parent
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
