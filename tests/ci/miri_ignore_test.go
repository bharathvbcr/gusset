package ci_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
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
// target. A test usually reaches the OS through a helper, not by naming libc
// itself, so a function that calls an OS function is an OS function too, to a
// fixed point, and a test that calls one must be ignored under Miri. A call is
// seen as `name(`, `Type::name(`, `module::path::name(`, `name::<T>(`,
// `.name(` and a function passed as a value (`map(name)`); a qualified call
// counts when its qualifier is a type or module the crate defines (or Self,
// self, super, crate), so std's `File::open(` is not a call of a crate
// function called open. A method call is matched by name alone, which can only
// over-report: a test that trips on it takes one #[cfg_attr(miri, ignore)].
//
// Comments, string and char literals are blanked before anything is read, so a
// brace or a call named inside one moves nothing; a multi-line attribute is
// read whole; `ignore` is matched as a word, so `ignored` is not it.
//
// socketpair(2) and UnixStream::pair count as the OS. Evidence, not a Miri run
// (no Miri is installed where this was written): every test in this tree that
// calls socketpair is already ignored under Miri, and each reaches fcntl or
// setsockopt next (tiny_socketpair sets SO_SNDBUF and calls set_nonblocking;
// the other opens a Handle on the socket, which is the fcntl that failed in the
// run named above). The guard's reason is the host calls Miri does not model,
// and these tests make them; if a socketpair test that makes none is ever
// wanted under Miri, narrow this one line.

// needsOS names the calls that reach pipe(2), a socket or a real Handle directly.
var needsOS = regexp.MustCompile(`\bmake_pipe\(|\bHandle::open\(|\bHandle::open_classified\(|\bgusset_handle_open\(|\blibc::pipe2?\(|\blibc::socketpair\(|\bUnixStream::pair\(`)

// rsFnDecl matches a function declaration, with any visibility and qualifier a
// test module is likely to use.
var rsFnDecl = regexp.MustCompile(`^\s*(?:pub(?:\([a-z]+\))?\s+)?(?:(?:unsafe|async|const|extern\s+"C")\s+)*fn\s+(\w+)\s*[(<]`)

// rsFn is one Rust function: its name, whether it is a #[test] and carries
// #[cfg_attr(miri, ignore)], and its body with comments and literals blanked.
type rsFn struct {
	name    string
	test    bool
	ignored bool
	body    string
}

// rustCode returns src with comments (line, and nested block), string
// literals (plain, byte, raw) and char literals replaced by spaces, newlines
// kept, so line structure and brace balance describe code only.
func rustCode(src string) string {
	b := []byte(src)
	out := append([]byte(nil), b...)
	blank := func(from, to int) {
		if to > len(b) {
			to = len(b)
		}
		for k := from; k < to; k++ {
			if out[k] != '\n' {
				out[k] = ' '
			}
		}
	}
	isIdent := func(c byte) bool {
		return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	i := 0
	for i < len(b) {
		switch {
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '/':
			j := i
			for j < len(b) && b[j] != '\n' {
				j++
			}
			blank(i, j)
			i = j
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '*':
			j, depth := i+2, 1
			for j < len(b) && depth > 0 {
				switch {
				case j+1 < len(b) && b[j] == '/' && b[j+1] == '*':
					depth++
					j += 2
				case j+1 < len(b) && b[j] == '*' && b[j+1] == '/':
					depth--
					j += 2
				default:
					j++
				}
			}
			blank(i, j)
			i = j
		case b[i] == 'r' && (i == 0 || !isIdent(b[i-1]) || (b[i-1] == 'b' && (i == 1 || !isIdent(b[i-2])))) && rawStringEnd(b, i) > 0:
			end := rawStringEnd(b, i)
			blank(i, end)
			i = end
		case b[i] == '"':
			j := i + 1
			for j < len(b) && b[j] != '"' {
				if b[j] == '\\' {
					j++
				}
				j++
			}
			blank(i, j+1)
			i = j + 1
		case b[i] == '\'':
			switch {
			case i+1 < len(b) && b[i+1] == '\\':
				j := i + 3 // past the escaped character, which may itself be a quote
				for j < len(b) && b[j] != '\'' {
					j++
				}
				blank(i, j+1)
				i = j + 1
			default:
				_, size := utf8.DecodeRune(b[min(i+1, len(b)):])
				if i+1+size < len(b) && b[i+1+size] == '\'' && b[i+1] != '\'' {
					blank(i, i+2+size) // a char literal
					i += 2 + size
				} else {
					i++ // a lifetime
				}
			}
		default:
			i++
		}
	}
	return string(out)
}

// rawStringEnd returns the offset just past the raw string literal that starts
// at b[i] ('r', then any number of #, then a quote), or 0 if none starts there.
func rawStringEnd(b []byte, i int) int {
	j := i + 1
	for j < len(b) && b[j] == '#' {
		j++
	}
	if j >= len(b) || b[j] != '"' {
		return 0
	}
	closer := "\"" + strings.Repeat("#", j-i-1)
	k := strings.Index(string(b[j+1:]), closer)
	if k < 0 {
		return len(b)
	}
	return j + 1 + k + len(closer)
}

var (
	// attrCfgAttrIgnore is a squashed cfg_attr(miri, ignore) or an unconditional
	// #[ignore], with `ignore` as a whole word.
	attrIgnored = regexp.MustCompile(`^#\[(?:cfg_attr\(miri,)?ignore(?:[^A-Za-z0-9_]|$)`)
	spaces      = regexp.MustCompile(`\s+`)
)

// attrEnd returns the offset in s just past the attribute that starts at its
// beginning (`#[` ... balanced `]`), and whether it closes within s.
func attrEnd(s string) (end int, closed bool) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return len(s), false
}

// parseRsFns returns every function declared in code (already passed through
// rustCode), nested ones included. The attributes are those directly above the
// declaration, one that spans lines included; the body runs to the brace that
// closes the first one. Anything after an attribute's closing bracket on its
// own line is dropped, so a one-line `#[test] fn x() {}` yields no function,
// and checkTestsParsed reports it.
func parseRsFns(code string) []rsFn {
	lines := strings.Split(code, "\n")
	var fns []rsFn
	var isTest, isIgnored bool
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "#[") || strings.HasPrefix(t, "#![") {
			attr := t
			for _, closed := attrEnd(attr); !closed && i+1 < len(lines); _, closed = attrEnd(attr) {
				i++
				attr += " " + strings.TrimSpace(lines[i])
			}
			end, _ := attrEnd(attr)
			attr = spaces.ReplaceAllString(attr[:end], "")
			if attr == "#[test]" {
				isTest = true
			}
			if attrIgnored.MatchString(attr) {
				isIgnored = true
			}
			continue
		}
		if t == "" {
			continue // a blank line, or a comment blanked by rustCode, between attributes and the item
		}
		m := rsFnDecl.FindStringSubmatch(lines[i])
		test, ignored := isTest, isIgnored
		isTest, isIgnored = false, false
		if m == nil {
			continue
		}
		f := rsFn{name: m[1], test: test, ignored: ignored}
		depth, started := 0, false
		var body strings.Builder
		for k := i; k < len(lines); k++ {
			body.WriteString(lines[k])
			body.WriteByte('\n')
			opens := strings.Count(lines[k], "{")
			depth += opens - strings.Count(lines[k], "}")
			if opens > 0 {
				started = true
			}
			if (started && depth <= 0) || (!started && strings.HasSuffix(strings.TrimSpace(lines[k]), ";")) {
				break
			}
		}
		f.body = body.String()
		fns = append(fns, f)
	}
	return fns
}

// crateTypeNames is every type, trait, module or impl target code names, plus
// the path words that stand for the crate's own.
var crateTypeName = regexp.MustCompile(`\b(?:struct|enum|trait|mod|type|union)\s+(\w+)|\bimpl(?:<[^>]*>)?\s+(?:[\w:<>]+\s+for\s+)?(\w+)`)

func crateNames(code string, into map[string]bool) {
	for _, m := range crateTypeName.FindAllStringSubmatch(code, -1) {
		for _, n := range m[1:] {
			if n != "" {
				into[n] = true
			}
		}
	}
	for _, n := range []string{"Self", "self", "super", "crate"} {
		into[n] = true
	}
}

// pathUse matches an identifier path followed by `(` (a call) or by `,` `)`
// `]` `;` (a function used as a value): qualifiers, the name, an optional
// turbofish, the follower.
var pathUse = regexp.MustCompile(`((?:[A-Za-z_]\w*(?:::<[^<>]*>)?::)*)([A-Za-z_]\w*)\s*(?:::<[^<>]*>)?\s*([(,)\];])`)

var turbofish = regexp.MustCompile(`::<[^<>]*>`)

// callsAny reports whether body calls, or takes as a value, any function in
// names (see the doc comment at the top of this file for what counts).
func callsAny(body string, names, types map[string]bool) bool {
	if len(names) == 0 {
		return false
	}
	for _, m := range pathUse.FindAllStringSubmatchIndex(body, -1) {
		name := body[m[4]:m[5]]
		if !names[name] {
			continue
		}
		if body[m[6]:m[7]] != "(" {
			// A value: only inside an argument list or array, never `let x = name;`.
			k := m[0] - 1
			for k >= 0 && (body[k] == ' ' || body[k] == '\n' || body[k] == '\t') {
				k--
			}
			if k < 0 || (body[k] != '(' && body[k] != ',' && body[k] != '[') {
				continue
			}
		}
		if quals := strings.TrimSuffix(turbofish.ReplaceAllString(body[m[2]:m[3]], ""), "::"); quals != "" {
			last := quals[strings.LastIndex(quals, "::")+1:]
			last = strings.TrimPrefix(last, ":")
			if !types[last] {
				continue
			}
		}
		return true
	}
	return false
}

// osFunctions is the set of non-test function names whose bodies reach the OS
// directly or through another such function, to a fixed point.
func osFunctions(fns []rsFn, types map[string]bool) map[string]bool {
	found := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, f := range fns {
			if f.test || found[f.name] {
				continue
			}
			if needsOS.MatchString(f.body) || callsAny(f.body, found, types) {
				found[f.name] = true
				changed = true
			}
		}
	}
	return found
}

// unignoredOSTests returns the names of the #[test] functions that reach the
// OS, directly or through a function in osFns, and lack an ignore under Miri.
func unignoredOSTests(fns []rsFn, osFns, types map[string]bool) []string {
	var bad []string
	for _, f := range fns {
		if f.test && !f.ignored && (needsOS.MatchString(f.body) || callsAny(f.body, osFns, types)) {
			bad = append(bad, f.name)
		}
	}
	return bad
}

// testAttrLines counts the lines of code that open with #[test].
func testAttrLines(code string) int {
	n := 0
	for _, line := range strings.Split(code, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#[test]") {
			n++
		}
	}
	return n
}

// checkTestsParsed is the guard's own guard: every #[test] attribute line must
// have produced a test function. A parser that loses one (a declaration form
// or attribute layout it does not read) would leave that test unexamined and
// the scan green.
func checkTestsParsed(code string, fns []rsFn) (attrs, parsed int) {
	for _, f := range fns {
		if f.test {
			parsed++
		}
	}
	return testAttrLines(code), parsed
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
	types := map[string]bool{}
	attrs, tests := 0, 0
	for path, src := range sources {
		code := rustCode(src)
		fns := parseRsFns(code)
		perFile[path] = fns
		all = append(all, fns...)
		crateNames(code, types)
		a, p := checkTestsParsed(code, fns)
		attrs, tests = attrs+a, tests+p
	}
	if attrs == 0 || tests != attrs {
		t.Fatalf("%d #[test] attributes in %d files but %d test functions parsed; the parser no longer reads the crate's tests", attrs, len(sources), tests)
	}
	osFns := osFunctions(all, types)
	if !osFns["make_pipe"] && !osFns["pipe"] {
		t.Fatalf("no pipe helper recognised as an OS function (%d found); the call-through rule scans nothing", len(osFns))
	}
	var paths []string
	for p := range perFile {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		for _, name := range unignoredOSTests(perFile[path], osFns, types) {
			t.Errorf("%s: test %s opens a pipe or a Handle (directly or through a helper) but has no #[cfg_attr(miri, ignore)]; the nightly Miri job fails on it", path, name)
		}
	}
}

// flagged runs the whole guard over one source text the way the test does.
func flagged(src string) (bad string, attrs, parsed int) {
	code := rustCode(src)
	fns := parseRsFns(code)
	types := map[string]bool{}
	crateNames(code, types)
	attrs, parsed = checkTestsParsed(code, fns)
	return strings.Join(unignoredOSTests(fns, osFunctions(fns, types), types), ","), attrs, parsed
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
    let s = "make_pipe() in a string is not a call";
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
fn pure_with_a_pure_helper_and_std_paths() {
    assert_eq!(pure_helper(), 1);
    let f = std::fs::File::open("x");
    let g = File::open("x");
}

#[test]
fn planted_through_the_c_export() {
    let rc = unsafe { gusset_handle_open(1, fd, &mut h, &mut st) };
}
`
	if got, a, p := flagged(fixture); got != "planted_pipe_test,planted_after_another_attribute,planted_through_a_helper,planted_through_a_helper_of_a_helper,planted_through_the_c_export" || a != p {
		t.Fatalf("flagged %q (attrs %d, parsed %d); want exactly the five planted tests", got, a, p)
	}

	// Each row is a test that reaches the OS through a form the guard once could
	// not see, or an ignore it once could not read. want is the flagged names.
	rows := []struct {
		name, src, want string
	}{
		{"a method on a rig", `
struct Rig { fd: i32 }
impl Rig {
    fn start() -> Rig { let (r, w) = make_pipe(); Rig { fd: w } }
    fn fd(&self) -> i32 { self.fd }
}
#[test]
fn via_rig() { let rig = Rig::start(); }
#[test]
fn via_rig_method() { let n = rig.start(); }
`, "via_rig,via_rig_method"},
		{"a module path", `
mod m {
    pub fn make_rig() { make_pipe(); }
}
#[test]
fn via_super() { super::m::make_rig(); }
#[test]
fn via_self_path() { self::m::make_rig(); }
#[test]
fn via_mod_path() { m::make_rig(); }
`, "via_super,via_self_path,via_mod_path"},
		{"a turbofish and a function value", `
fn helper<T>() { make_pipe(); }
fn spawn_with(f: fn()) { f(); }
#[test]
fn via_turbofish() { helper::<u8>(); }
#[test]
fn via_value() { spawn_with(helper::<u8>); }
#[test]
fn via_map() { let v: Vec<()> = (0..2).map(helper_plain).collect(); }
fn helper_plain(_: i32) { make_pipe(); }
#[test]
fn via_array() { let fs = [helper_plain, helper_plain]; }
#[test]
fn a_binding_named_like_it_is_not_a_value() { let x = helper_plain; }
`, "via_turbofish,via_value,via_map,via_array"},
		{"a brace in a string", `
fn helper() {
    let s = "}";
    make_pipe();
}
#[test]
fn closing_brace_in_a_string() { helper(); }
fn other() {
    let s = "{";
}
#[test]
fn pure_after_an_open_brace_string() { let x = 1; }
#[test]
fn braces_in_chars() { let a = '{'; let b = '}'; make_pipe(); }
#[test]
fn braces_in_a_raw_string() { let a = r#"{ } " "#; make_pipe(); }
#[test]
fn braces_in_a_comment() { /* { { */ let a = 1; // }
}
`, "closing_brace_in_a_string,braces_in_chars,braces_in_a_raw_string"},
		{"a lifetime is not a char", `
fn first<'a>(x: &'a str) -> &'a str { x }
#[test]
fn with_lifetimes() { let s = first("a"); make_pipe(); }
`, "with_lifetimes"},
		{"a multi-line ignore", `
#[test]
#[cfg_attr(
    miri,
    ignore
)]
fn ignored_across_lines() { make_pipe(); }
#[test]
#[cfg_attr(miri, ignore = "needs fcntl")]
fn ignored_with_a_reason() { make_pipe(); }
#[test]
#[ignore]
fn ignored_always() { make_pipe(); }
`, ""},
		{"ignored is not ignore", `
#[test]
#[cfg_attr(miri, ignored)]
fn not_ignored() { make_pipe(); }
#[test]
#[cfg_attr(miri, ignore_this)]
fn not_ignored_either() { make_pipe(); }
#[test]
#[cfg_attr(not(miri), ignore)]
fn ignored_off_miri_only() { make_pipe(); }
`, "not_ignored,not_ignored_either,ignored_off_miri_only"},
		{"a multi-line cfg before the test", `
#[test]
#[cfg(any(
    target_os = "macos",
    target_os = "linux"
))]
fn cfg_across_lines() { make_pipe(); }
#[test]
#[cfg(any(
    target_os = "macos"
))]
#[cfg_attr(miri, ignore)]
fn cfg_across_lines_ignored() { make_pipe(); }
`, "cfg_across_lines"},
		{"std paths are not crate functions", `
fn open() { make_pipe(); }
#[test]
fn std_open() { let f = std::fs::File::open("x"); let g = File::open("y"); }
#[test]
fn crate_open() { open(); }
#[test]
fn crate_open_by_path() { super::open(); }
`, "crate_open,crate_open_by_path"},
		{"socketpair", `
#[test]
fn via_socketpair() { let rc = unsafe { libc::socketpair(libc::AF_UNIX, libc::SOCK_STREAM, 0, p) }; }
#[test]
fn via_unix_stream() { let (a, b) = UnixStream::pair().unwrap(); }
fn tiny() -> (i32, i32) { unsafe { libc::socketpair(1, 1, 0, p) }; (0, 0) }
#[test]
fn via_socketpair_helper() { let (w, r) = tiny(); }
`, "via_socketpair,via_unix_stream,via_socketpair_helper"},
	}
	for _, r := range rows {
		got, a, p := flagged(r.src)
		if got != r.want || a != p {
			t.Errorf("%s: flagged %q (attrs %d, parsed %d); want %q", r.name, got, a, p, r.want)
		}
	}

	// A one-line `#[test] fn` is not read; the count must say so.
	if _, a, p := flagged("#[test] fn one_liner() { make_pipe(); }\n"); a == p {
		t.Errorf("a one-line #[test] fn was parsed (attrs %d, parsed %d); the parse check cannot see a test the parser lost", a, p)
	}
}
