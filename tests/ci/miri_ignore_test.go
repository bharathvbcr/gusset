package ci_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The nightly Miri job runs `cargo miri test -p gusset --lib`, and Miri cannot
// run pipe(2), the worker threads or the pipe fcntls a real Handle needs. Every
// lib test that opens a pipe or a Handle therefore carries
// #[cfg_attr(miri, ignore)] (nightly.yml says so beside the command). A test
// added without it fails only in Miri, and only once every earlier Miri step
// passes: open_refuses_a_descriptor_that_is_not_a_pipe_write_end and three
// respawn tests waited behind a cancelled job for days and then failed with
// "unsupported operation: fcntl" (run 40065501109). This makes that a per-push
// failure instead.

// needsOS names the calls that reach pipe(2) or open a real Handle.
var needsOS = regexp.MustCompile(`\bmake_pipe\(|\bHandle::open\(|libc::pipe\(`)

var testFn = regexp.MustCompile(`^\s*(?:pub\s+)?fn\s+(\w+)\s*\(`)

// unignoredOSTests returns the #[test] functions in src whose body calls one of
// needsOS but whose attribute block lacks #[cfg_attr(miri, ignore)].
func unignoredOSTests(src string) []string {
	lines := strings.Split(src, "\n")
	var bad []string
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "#[test]" {
			continue
		}
		ignored := false
		j := i + 1
		for ; j < len(lines); j++ {
			t := strings.TrimSpace(lines[j])
			if strings.HasPrefix(t, "#[cfg_attr(miri, ignore)]") {
				ignored = true
				continue
			}
			if strings.HasPrefix(t, "#[") || strings.HasPrefix(t, "//") {
				continue
			}
			break
		}
		if j >= len(lines) {
			break
		}
		m := testFn.FindStringSubmatch(lines[j])
		if m == nil {
			continue
		}
		depth, started, uses := 0, false, false
		for k := j; k < len(lines); k++ {
			if needsOS.MatchString(lines[k]) {
				uses = true
			}
			opens := strings.Count(lines[k], "{")
			depth += opens - strings.Count(lines[k], "}")
			if opens > 0 {
				started = true
			}
			if started && depth <= 0 {
				break
			}
		}
		if uses && !ignored {
			bad = append(bad, m[1])
		}
		i = j
	}
	return bad
}

func TestLibTestsThatNeedTheOSAreIgnoredUnderMiri(t *testing.T) {
	files, err := filepath.Glob("../../crates/gusset/src/pool/tests/*.rs")
	if err != nil {
		t.Fatal(err)
	}
	more, err := filepath.Glob("../../crates/gusset/src/pool/*.rs")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, more...)
	if len(files) < 5 {
		t.Fatalf("found %d pool source files; the glob no longer matches the crate layout", len(files))
	}
	scanned := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "#[test]") {
			continue
		}
		scanned++
		for _, name := range unignoredOSTests(string(b)) {
			t.Errorf("%s: test %s opens a pipe or a Handle but has no #[cfg_attr(miri, ignore)]; the nightly Miri job fails on it", f, name)
		}
	}
	if scanned == 0 {
		t.Fatal("no pool file holds a #[test]; this check scans nothing")
	}
}

func TestMiriIgnoreRuleOnFixtures(t *testing.T) {
	const fixture = `
#[test]
#[cfg_attr(miri, ignore)]
fn ignored_pipe_test() {
    let (r, w) = make_pipe();
}

#[test]
fn pure_test() {
    assert_eq!(1, 1);
}

#[test]
fn planted_pipe_test() {
    let handle = Handle::open(1, w).must("open");
}

#[test]
#[should_panic]
fn planted_after_another_attribute() {
    let (r, w) = make_pipe();
}
`
	got := strings.Join(unignoredOSTests(fixture), ",")
	if got != "planted_pipe_test,planted_after_another_attribute" {
		t.Fatalf("flagged %q; want exactly the two planted tests", got)
	}
}
