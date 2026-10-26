package ci_test

import (
	"regexp"
	"strconv"
	"strings"
)

// Shell reading shared by the rustup-init rule and the image rule. Both are
// pure functions over lines, so the same code runs over the real workflows and
// build scripts and over a planted violation (workflow_images_test.go). The
// reading is deliberately small: it knows command separators, a command's word
// after its wrappers, and quoting only enough to trim it. Where it cannot read
// something the rules fail closed or the limit is written down beside the rule.

// shellCmd is one simple command of a shell body: where it starts in the text,
// its words, and the separator that ended it.
type shellCmd struct {
	start int
	words []string
	sep   string
}

// shellSplit separates commands: ; && || | a closing paren, a newline, and the
// openers of a command substitution or subshell, so the command inside
// `out=$(rustup-init -y)` is a command of its own. Quoting is not understood:
// the rules that use this look at command words and whole-word paths, which a
// separator inside a quoted string does not move.
var shellSplit = regexp.MustCompile("\\$\\(|;|&&|\\|\\||\\||\\)|\\n|`")

func shellCommands(text string) []shellCmd {
	var out []shellCmd
	prev := 0
	for _, m := range shellSplit.FindAllStringIndex(text, -1) {
		out = append(out, shellCmd{prev, strings.Fields(text[prev:m[0]]), text[m[0]:m[1]]})
		prev = m[1]
	}
	return append(out, shellCmd{prev, strings.Fields(text[prev:]), ""})
}

// shellSubst is a non-nested `$(...)`. flattenSubst replaces the ones that do
// not hold a command the rule is looking for, so `-v $(shell pwd):/src` and
// `$(CURDIR)/x` stay one word and the command around them is not cut at the
// paren. keep matches the commands that must stay visible.
var shellSubst = regexp.MustCompile(`\$\(([^()]*)\)`)

// engineWords matches a container engine named inside a substitution.
var engineWords = regexp.MustCompile(`(?:^|[\s/])(?:docker|podman|nerdctl)\b`)

var simpleVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func flattenSubst(s string, keep *regexp.Regexp) string {
	for i := 0; i < 8; i++ {
		n := shellSubst.ReplaceAllStringFunc(s, func(m string) string {
			inner := m[2 : len(m)-1]
			if keep.MatchString(inner) {
				return m
			}
			if simpleVarName.MatchString(inner) {
				return "$" + inner
			}
			return "$SUBST"
		})
		if n == s {
			return s
		}
		s = n
	}
	return s
}

// shellText is lines with comments removed, each backslash continuation joined
// into one line, and substitutions that hide no keep-command flattened. Offsets
// the rules use are into this text.
func shellText(lines []string, keep *regexp.Regexp) string {
	body := make([]string, 0, len(lines))
	for _, l := range lines {
		body = append(body, stripShellComment(l))
	}
	return flattenSubst(strings.ReplaceAll(strings.Join(body, "\n"), "\\\n", " "), keep)
}

var shellAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// lineLead names the words that open a command without being its name.
var lineLead = map[string]bool{
	"then": true, "do": true, "if": true, "elif": true, "else": true,
	"while": true, "until": true, "!": true,
}

// wrapperSpec describes a command that runs another: which of its options take
// a value, and how many positional arguments precede the command it runs.
type wrapperSpec struct {
	valueOpts  map[string]bool
	positional int
}

func opts(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// wrappers are the common ways a command is run through another. `command` is
// left out: `command -v x` asks about x and does not run it.
var wrappers = map[string]wrapperSpec{
	"sudo":    {opts("-u", "-g", "-C", "-h", "-p", "-r", "-t", "-U", "-D", "-R", "-T", "--user", "--group", "--chdir"), 0},
	"env":     {opts("-u", "-C", "-S", "--unset", "--chdir", "--split-string"), 0},
	"timeout": {opts("-s", "-k", "--signal", "--kill-after"), 1},
	"nice":    {opts("-n", "--adjustment"), 0},
	"ionice":  {opts("-c", "-n", "-p", "--class", "--classdata", "--pid"), 0},
	"stdbuf":  {opts("-i", "-o", "-e", "--input", "--output", "--error"), 0},
	"time":    {nil, 0}, "exec": {nil, 0}, "nohup": {nil, 0}, "setsid": {nil, 0},
	"sh": {nil, 0}, "bash": {nil, 0}, ".": {nil, 0}, "source": {nil, 0},
}

// commandWord returns the index and text of the word that names the command a
// word list runs: the first word that is not a leading keyword (`then`, `!`), an
// environment assignment, an opening paren or brace, a make recipe prefix, or
// part of a wrapper (sudo -E, timeout 300, env -i X=1, nice -n 10, `. path`).
func commandWord(words []string) (int, string) {
	var wrap *wrapperSpec
	skip, positional := 0, 0
	for i, w := range words {
		w = strings.Trim(strings.TrimLeft(w, "({"), `"'`)
		if skip > 0 {
			skip--
			continue
		}
		if w == "" {
			continue
		}
		if wrap == nil {
			// A recipe line may start @ + or -; no command starts with a dash.
			w = strings.TrimLeft(w, "@+-")
			if w == "" || lineLead[w] || shellAssignment.MatchString(w) {
				continue
			}
			if spec, ok := wrappers[w]; ok {
				wrap, positional = &spec, spec.positional
				continue
			}
			return i, w
		}
		switch {
		case strings.HasPrefix(w, "-") && len(w) > 1:
			if wrap.valueOpts[w] {
				skip = 1
			}
		case shellAssignment.MatchString(w):
		case positional > 0:
			positional--
		default:
			if spec, ok := wrappers[w]; ok {
				wrap, positional = &spec, spec.positional
				continue
			}
			return i, w
		}
	}
	return -1, ""
}

// ---- container images ----------------------------------------------------

// engineWord matches the container engine a command runs: docker, podman or
// nerdctl at any path, or a variable standing for one.
var engineWord = regexp.MustCompile(`^(?:\S*/)?(?:docker|podman|nerdctl)$|^\$\{?(?:DOCKER|PODMAN|NERDCTL|CONTAINER_ENGINE|CONTAINER_RUNTIME|ENGINE)\}?$`)

// imageVerbs are the subcommands that name an image to pull or start.
var imageVerbs = map[string]bool{"run": true, "pull": true, "create": true}

// engineGlobalOpts take a value before the subcommand (`docker --context x run`).
var engineGlobalOpts = opts("-H", "--host", "-c", "--context", "--config", "-l", "--log-level",
	"--connection", "--url", "--identity", "--remote", "--root", "--runroot", "--storage-driver",
	"--cgroup-manager", "--namespace", "-n", "--address", "--tlscacert", "--tlscert", "--tlskey", "--hooks-dir")

// verbOpts take a value after run, pull or create. An option not listed is
// read as a flag, so its value is taken for the image and the line is
// reported: the rule can only err toward a false report.
var verbOpts = opts("-a", "--attach", "-c", "--cpu-shares", "-e", "--env", "-h", "--hostname", "-l", "--label",
	"-m", "--memory", "-p", "--publish", "-u", "--user", "-v", "--volume", "-w", "--workdir",
	"--add-host", "--annotation", "--authfile", "--blkio-weight", "--cap-add", "--cap-drop", "--cert-dir",
	"--cgroup-parent", "--cgroupns", "--cidfile", "--cpus", "--cpuset-cpus", "--creds", "--device", "--dns",
	"--entrypoint", "--env-file", "--expose", "--gpus", "--group-add", "--health-cmd", "--ip", "--ipc",
	"--label-file", "--link", "--log-driver", "--log-opt", "--mac-address", "--mount", "--name", "--net",
	"--network", "--pid", "--pids-limit", "--platform", "--pull", "--restart", "--runtime", "--security-opt",
	"--shm-size", "--stop-signal", "--stop-timeout", "--tmpfs", "--ulimit", "--userns", "--uts",
	"--volumes-from", "--arch", "--os", "--variant", "--retry", "--retry-delay")

// imageOperand reads one command's words. ok is false when the command is not
// a container run, pull or create. ref is the image operand, "" when the
// command names none.
func imageOperand(words []string) (ref string, ok bool) {
	e := -1
	for i, w := range words {
		if engineWord.MatchString(strings.TrimLeft(strings.Trim(w, `"'`), "@+-({")) {
			e = i
			break
		}
	}
	if e < 0 {
		return "", false
	}
	j := e + 1
	for j < len(words) && strings.HasPrefix(words[j], "-") {
		j++
		if engineGlobalOpts[words[j-1]] {
			j++
		}
	}
	if j < len(words) && (words[j] == "container" || words[j] == "image") {
		j++
	}
	if j >= len(words) || !imageVerbs[words[j]] {
		return "", false
	}
	for j++; j < len(words); j++ {
		w := words[j]
		if w == "--" {
			j++
			break
		}
		if !strings.HasPrefix(w, "-") {
			break
		}
		if verbOpts[w] {
			j++
		}
	}
	if j < len(words) {
		return words[j], true
	}
	return "", true
}

// ---- rustup-init ---------------------------------------------------------

// rustupInitViolations reports an unverified rustup-init in a workflow. A step
// that runs rustup-init must source tools/ci/versions.env, download the pinned
// $RUSTUP_VERSION, and, before each run, check the very path it runs against
// $RUSTUP_INIT_SHA256_X86_64_MUSL with a `sha256sum -c` whose failure stops the
// step, with nothing fetched or copied over that path between the check and the
// run. `apk add rustup` is refused too: it installs a rustup-init this repo
// never checked, and the unverified-download shortcut (sh.rustup.rs) is refused
// for the same reason. rustupScriptViolations is the same rule over a build
// script or Makefile, which has no steps: the whole file is the body.
//
// What counts as a run: any command whose command word is rustup-init, at any
// path, with any arguments or none, behind any of the wrappers in commandWord
// (sudo, env, timeout N, nice, `. path`, sh/bash) or inside `$(...)`. An
// unnamed step (`- run: |`), a single-line run, any block scalar header and any
// step indentation are read (stepRunBody and TestEveryRunKeyIsInAParsedStep,
// which fails on a run key the step parser did not reach).
//
// What counts as a check: a line that runs `sha256sum -c` (or --check) over the
// path with the pinned checksum variable, and whose failure stops the step: no
// `||` handler at all, or `|| exit N` (N nonzero) or `|| false` as the whole
// handler, ending the list or followed by `&&`. So `|| true`, `|| :`, `|| echo
// "would exit 1"`, `|| exit 1 || true`, `|| { echo x; exit 1; }` (a group is
// refused, not parsed), a check put in the background with `&`, one whose line
// opens with `... ||` (the check may never run), and a body that runs `set +e`
// are all refused.
//
// Known limits, written down rather than left to be found: a check or run
// inside an `if`/`case` branch, a function or a script the step calls is read
// as the text it is, so a check that sits in a branch that need not run counts;
// a run or a path reached through a shell variable ($RI -y, a path built from
// parts) is not seen; a downloader the rule does not know (aria2c is, others are
// not) can rewrite the file unseen; `x && check` counts as a check, since
// download-then-check on one line is the usual shape.
func rustupInitViolations(name string, lines []string) []string {
	out := rustupFetchViolations(name, lines)
	for _, st := range workflowSteps(lines) {
		step := name + ": step " + strconv.Quote(stepTitle(st)) + " runs rustup-init "
		out = append(out, rustupBodyViolations(step, rustupStepText(st))...)
	}
	return out
}

// rustupScriptViolations is rustupInitViolations for a build script or Makefile.
func rustupScriptViolations(name string, lines []string) []string {
	out := rustupFetchViolations(name, lines)
	return append(out, rustupBodyViolations(name+" runs rustup-init ", shellText(lines, rustupWords))...)
}

// rustupFetchViolations reports the two ways of getting a rustup-init this
// repository never checked: sh.rustup.rs and `apk add rustup`.
func rustupFetchViolations(name string, lines []string) []string {
	var out []string
	for _, ll := range logicalLines(lines) {
		at := name + ":" + strconv.Itoa(ll.n) + ": "
		if rustupPipe.MatchString(ll.text) {
			out = append(out, at+"rustup is fetched from rustup.rs with no pinned version or checksum: "+strings.TrimSpace(ll.text))
		}
		for _, seg := range commandSegments(ll.text) {
			if apkAddsRustup(strings.Fields(seg)) {
				out = append(out, at+"apk add rustup installs an unverified rustup-init; download the pinned one: "+strings.TrimSpace(seg))
			}
		}
	}
	return out
}

// rustupWords is what flattenSubst must leave visible for this rule.
var rustupWords = regexp.MustCompile(`rustup-init`)

// rustupStepText is a step's run body in shellText form.
func rustupStepText(st []string) string {
	return shellText(strings.Split(stepRunBody(st), "\n"), rustupWords)
}

// setPlusE matches a body turning errexit off, in either spelling: `set +e`
// (alone or among other short flags) and `set +o errexit`.
var setPlusE = regexp.MustCompile(`(?m)(?:^|[;&|(]\s*|\s)set\s+(?:-\S+\s+)*(?:\+[a-zA-Z]*e|\+o\s+errexit\b)`)

// rustupBodyViolations applies the rule to one shell body, a step's run body or
// a whole script. who is the prefix of each message.
func rustupBodyViolations(who, text string) []string {
	runs := rustupRuns(text)
	if len(runs) == 0 {
		return nil
	}
	var out []string
	if !strings.Contains(text, "tools/ci/versions.env") {
		out = append(out, who+"without sourcing tools/ci/versions.env")
	}
	for _, v := range []string{"RUSTUP_VERSION", "RUSTUP_INIT_SHA256_X86_64_MUSL"} {
		if !strings.Contains(text, "$"+v) && !strings.Contains(text, "${"+v) {
			out = append(out, who+"without using $"+v)
		}
	}
	if setPlusE.MatchString(text) {
		out = append(out, who+"in a body that runs `set +e`, so a failed check does not stop it")
	}
	// Each run of rustup-init needs an earlier `sha256sum -c` line that names
	// that same path and the pinned checksum: verifying some other file, or any
	// hash, proves nothing about what gets executed.
	for _, run := range runs {
		path := strings.Trim(run.words[0], `"'`)
		end, ok := checksumBefore(text[:run.start], path)
		if !ok {
			out = append(out, who+"("+strconv.Quote(path)+") without an earlier `sha256sum -c` line that checks that path against $RUSTUP_INIT_SHA256_X86_64_MUSL and whose failure stops the step")
			continue
		}
		if w := rewritesPath(text[end:run.start], path); w != "" {
			out = append(out, who+"("+strconv.Quote(path)+") fetches or copies over the path after it was checked: "+w)
		}
	}
	return out
}

// rustupRuns returns the commands of text that run rustup-init, each reduced to
// its command word (the path it runs).
func rustupRuns(text string) []shellCmd {
	var runs []shellCmd
	for _, c := range shellCommands(text) {
		if i, w := commandWord(c.words); i >= 0 && (w == "rustup-init" || strings.HasSuffix(w, "/rustup-init")) {
			c.words = []string{w}
			runs = append(runs, c)
		}
	}
	return runs
}

var sha256Check = regexp.MustCompile(`sha256sum\s+(?:-\w+\s+)*(?:-c\b|--check\b)`)

// checkHandler is the whole of an accepted `||` handler: a nonzero exit, or
// false, and what ends it. A false followed by `;` is refused: without set -e
// the list goes on.
var checkHandler = regexp.MustCompile(`^\|\|\s*(exit\s+[1-9][0-9]*|false)\s*(;|&&|$)`)

// backgrounded matches a lone & that backgrounds a command, not && or >&.
var backgrounded = regexp.MustCompile(`[^&>]&(?:[^&>]|$)`)

// checksumBefore finds the latest line of before that runs `sha256sum -c`
// over path using the pinned checksum variable and whose failure stops the
// step (checkStopsStep), and returns the offset in before just after that
// `sha256sum -c`. The path must appear as a whole word ("./x" and "x" are the
// same file), so /tmp/rustup-init.new and /tmp/other do not count for
// /tmp/rustup-init.
func checksumBefore(before, path string) (end int, ok bool) {
	want := regexp.MustCompile(`(?:^|[\s"'=])(?:\./)?` + regexp.QuoteMeta(strings.TrimPrefix(path, "./")) + `(?:$|[\s"'])`)
	off := 0
	for _, l := range strings.SplitAfter(before, "\n") {
		m := sha256Check.FindStringIndex(l)
		if m != nil &&
			(strings.Contains(l, "$RUSTUP_INIT_SHA256_X86_64_MUSL") || strings.Contains(l, "${RUSTUP_INIT_SHA256_X86_64_MUSL")) &&
			want.MatchString(l) &&
			checkStopsStep(l[:m[0]], l[m[1]:]) {
			end, ok = off+m[1], true
		}
		off += len(l)
	}
	return end, ok
}

// checkStopsStep reports whether a check line, split at `sha256sum -c` into
// what leads it and what follows, can fail the step: it is not behind a `||`,
// not backgrounded, and any `||` after it hands off to a failing handler.
func checkStopsStep(lead, rest string) bool {
	if i := strings.LastIndex(lead, ";"); strings.Contains(lead[i+1:], "||") {
		return false // `true || check`: the check may never run
	}
	if backgrounded.MatchString(rest) {
		return false
	}
	i := strings.Index(rest, "||")
	if i < 0 {
		return true
	}
	m := checkHandler.FindStringSubmatch(rest[i:])
	if m == nil || (m[1] == "false" && m[2] == ";") {
		return false
	}
	return !strings.Contains(rest[i+len(m[0]):], "||")
}

// pathWriters are the commands that put a file at a path.
var pathWriters = map[string]bool{
	"wget": true, "curl": true, "cp": true, "mv": true, "install": true,
	"tee": true, "dd": true, "ln": true, "rsync": true, "aria2c": true,
}

// rewritesPath returns the first command of between, the text from a passed
// check to the run it guards, that may replace the checked file: a downloader
// or copier that names the path or the file's name, or a redirect onto the
// path. A check proves what was on disk when it ran, not what is there later.
// A URL that merely contains the file's name counts: it fails closed.
func rewritesPath(between, path string) string {
	base := path[strings.LastIndex(path, "/")+1:]
	redirect := regexp.MustCompile(`>>?\s*["']?(?:\./)?` + regexp.QuoteMeta(strings.TrimPrefix(path, "./")) + `(?:$|[\s"';&|)])`)
	for _, c := range shellCommands(between) {
		i, w := commandWord(c.words)
		if i < 0 {
			continue
		}
		cmd := strings.Join(c.words, " ")
		if redirect.MatchString(cmd) || (pathWriters[w] && strings.Contains(cmd, base)) {
			return strings.TrimSpace(cmd)
		}
	}
	return ""
}
