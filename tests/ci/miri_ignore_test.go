package ci_test

import (
	"io/fs"
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
//
// The scan covers every .rs file under crates/gusset/src, the whole --lib
// target, not a list of directories: the guard once read only pool/, so a test
// in ffi/mod.rs that opened a pipe was never looked at. And a test usually
// reaches the OS through a helper in its module (ffi/mod.rs's `pipe()` and
// `open()`), not by naming libc::pipe itself, so a function that calls an OS
// function is an OS function too, to a fixed point, and a test that calls one
// must be ignored under Miri.

// needsOS names the calls that reach pipe(2) or open a real Handle directly.
// Wrappers are found by osFunctions, not listed here.
var needsOS = regexp.MustCompile(`\bmake_pipe\(|\bHandle::open\(|\bHandle::open_classified\(|\bgusset_handle_open\(|\blibc::pipe2?\(`)

// rsFnDecl matches a function declaration, with any visibility and qualifier a
// test module is likely to use.
var rsFnDecl = regexp.MustCompile(`^\s*(?:pub(?:\([a-z]+\))?\s+)?(?:(?:unsafe|async|const|extern\s+"C")\s+)*fn\s+(\w+)\s*[(<]`)

// rsFn is one Rust function: its name, whether it is a #[test] and carries
// #[cfg_attr(miri, ignore)], and its body with line comments removed.
type rsFn struct {
	name    string
	test    bool
	ignored bool
	body    string
}

// codeOnly drops a trailing line comment, so a call named in a comment or a
// brace in one counts for nothing.
func codeOnly(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}

// parseRsFns returns every function declared in src, nested ones included. The
// attribute block is the run of attribute, doc and comment lines directly above
// the declaration, an attribute that spans lines (`#[cfg(any(` ... `))]`)
// included; the body runs to the brace that closes the first one.
func parseRsFns(src string) []rsFn {
	lines := strings.Split(src, "\n")
	var fns []rsFn
	var isTest, isIgnored bool
	attrDepth := 0 // unclosed '[' of a multi-line attribute
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if attrDepth > 0 {
			attrDepth += strings.Count(t, "[") - strings.Count(t, "]")
			continue
		}
		if strings.HasPrefix(t, "#[") {
			if t == "#[test]" {
				isTest = true
			}
			if strings.HasPrefix(t, "#[cfg_attr(miri, ignore") {
				isIgnored = true
			}
			attrDepth = strings.Count(t, "[") - strings.Count(t, "]")
			continue
		}
		if strings.HasPrefix(t, "//") {
			continue
		}
		m := rsFnDecl.FindStringSubmatch(line)
		test, ignored := isTest, isIgnored
		isTest, isIgnored = false, false
		if m == nil {
			continue
		}
		f := rsFn{name: m[1], test: test, ignored: ignored}
		depth, started := 0, false
		var body strings.Builder
		for k := i; k < len(lines); k++ {
			code := codeOnly(lines[k])
			body.WriteString(code)
			body.WriteByte('\n')
			opens := strings.Count(code, "{")
			depth += opens - strings.Count(code, "}")
			if opens > 0 {
				started = true
			}
			if (started && depth <= 0) || (!started && strings.HasSuffix(strings.TrimSpace(code), ";")) {
				break
			}
		}
		f.body = body.String()
		fns = append(fns, f)
	}
	return fns
}

// callsBare reports whether body calls the free function name: not a method
// (`.name(`), not a path (`Type::name(`, `libc::name(`), which are other things.
func callsBare(body, name string) bool {
	re := regexp.MustCompile(`(?:^|[^\w.:])` + regexp.QuoteMeta(name) + `\(`)
	return re.MatchString(body)
}

// osFunctions is the set of non-test function names whose bodies reach the OS
// directly or through another such function, to a fixed point.
func osFunctions(fns []rsFn) map[string]bool {
	found := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, f := range fns {
			if f.test || found[f.name] {
				continue
			}
			uses := needsOS.MatchString(f.body)
			for name := range found {
				if uses {
					break
				}
				uses = callsBare(f.body, name)
			}
			if uses {
				found[f.name] = true
				changed = true
			}
		}
	}
	return found
}

// unignoredOSTests returns the names of the #[test] functions that reach the
// OS, directly or through a function in osFns, and lack #[cfg_attr(miri, ignore)].
func unignoredOSTests(fns []rsFn, osFns map[string]bool) []string {
	var bad []string
	for _, f := range fns {
		if !f.test || f.ignored {
			continue
		}
		uses := needsOS.MatchString(f.body)
		for name := range osFns {
			if uses {
				break
			}
			uses = callsBare(f.body, name)
		}
		if uses {
			bad = append(bad, f.name)
		}
	}
	return bad
}

// libSources reads every .rs file under crates/gusset/src: the sources of the
// target `cargo miri test --lib` builds.
func libSources(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join("..", "..", "crates", "gusset", "src")
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".rs" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLibTestsThatNeedTheOSAreIgnoredUnderMiri(t *testing.T) {
	sources := libSources(t)
	// The crate has 23 source files; a path change that leaves a handful would
	// scan almost nothing and pass.
	if len(sources) < 15 {
		t.Fatalf("found %d crate source files; the path no longer matches the crate layout", len(sources))
	}
	var all []rsFn
	perFile := map[string][]rsFn{}
	tests, attrs := 0, 0
	for path, src := range sources {
		fns := parseRsFns(src)
		perFile[path] = fns
		all = append(all, fns...)
		for _, f := range fns {
			if f.test {
				tests++
			}
		}
		for _, line := range strings.Split(src, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#[test]") {
				attrs++
			}
		}
	}
	// Every #[test] attribute line must have produced a test function. A parser
	// that loses one (a declaration form or attribute layout it does not read)
	// would leave that test unexamined and the guard green.
	if attrs == 0 || tests != attrs {
		t.Fatalf("%d #[test] attributes in %d files but %d test functions parsed; the parser no longer reads the crate's tests", attrs, len(sources), tests)
	}
	osFns := osFunctions(all)
	if !osFns["make_pipe"] && !osFns["pipe"] {
		t.Fatalf("no pipe helper recognised as an OS function (%d found); the call-through rule scans nothing", len(osFns))
	}
	for path, fns := range perFile {
		for _, name := range unignoredOSTests(fns, osFns) {
			t.Errorf("%s: test %s opens a pipe or a Handle (directly or through a helper) but has no #[cfg_attr(miri, ignore)]; the nightly Miri job fails on it", path, name)
		}
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
    // make_pipe() in a comment is not a call
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

fn pipe() -> (i32, i32) {
    let mut fds = [0i32; 2];
    assert_eq!(unsafe { libc::pipe(fds.as_mut_ptr()) }, 0);
    (fds[0], fds[1])
}

pub(super) fn open_via_pipe() -> i32 {
    let (r, w) = pipe();
    w
}

fn pure_helper() -> i32 {
    1
}

#[test]
fn planted_through_a_helper() {
    let (r, w) = pipe();
}

#[test]
fn planted_through_a_helper_of_a_helper() {
    let w = open_via_pipe();
}

#[test]
#[cfg_attr(miri, ignore)]
fn ignored_through_a_helper() {
    let (r, w) = pipe();
}

#[test]
fn pure_with_a_pure_helper_and_a_method() {
    assert_eq!(pure_helper(), 1);
    let v = x.pipe();
    let f = std::fs::File::open("x");
}

#[test]
fn planted_through_the_c_export() {
    let rc = unsafe { gusset_handle_open(1, fd, &mut h, &mut st) };
}
`
	fns := parseRsFns(fixture)
	got := strings.Join(unignoredOSTests(fns, osFunctions(fns)), ",")
	const want = "planted_pipe_test,planted_after_another_attribute,planted_through_a_helper,planted_through_a_helper_of_a_helper,planted_through_the_c_export"
	if got != want {
		t.Fatalf("flagged %q; want exactly %q", got, want)
	}
}
