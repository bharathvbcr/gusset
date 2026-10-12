package ci_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestMakefileParsesUnderTheHostMake expands every variable the install path
// uses, under whichever make is on PATH. macOS ships GNU Make 3.81, which reads
// an unescaped '#' inside $(shell …) as the start of a comment and stops with
// "unterminated call to function 'shell'" — `make build` never expands
// GUSSET_VERSION, so only `make install` (and install.sh, which calls it) broke,
// and only on a Mac. A dry run of install is the cheapest way to make the
// parser walk that line.
func TestMakefileParsesUnderTheHostMake(t *testing.T) {
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("no make on PATH")
	}
	cmd := exec.Command(makePath, "-n", "-C", "../..", "install", "PREFIX="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n install failed under %s: %v\n%s", makePath, err, out)
	}
	if strings.Contains(string(out), "unterminated call") {
		t.Fatalf("make reported a parse error:\n%s", out)
	}
}
