package ci_test

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Container-image and rustup-init pinning (ft-415ddf1f criterion 7).
//
// A `container:` image named by tag is a mutable third-party artefact: the
// registry can move the tag under a required check. The rules here, like the
// ones in workflow_supplychain_test.go, are pure functions over lines, so the
// same code runs over the real workflows and over a planted violation
// (TestPlantedImageViolationsAreCaught): a rule that cannot fail is not a rule.
//
// What is deliberately not pinned, and why, lives next to the musl jobs in
// matrix.yml: Alpine's apk index keeps only the newest build of each package,
// so an exact `apk add pkg=ver` stops resolving the day Alpine rebuilds it.

var (
	// repo:tag@sha256:<64 hex>. The tag is kept beside the digest so a human can
	// read what the digest is and a bump tool can find the line.
	digestRef      = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
	imageKeyLine   = regexp.MustCompile(`^\s*(?:- )?(container|image):\s*(.*)$`)
	dockerUsesLine = regexp.MustCompile(`^\s*(?:- )?uses:\s*docker://(\S+)`)
	containerRun   = regexp.MustCompile(`\b(?:docker|podman)\s+(?:run|pull|create)\b`)
	rustupExec     = regexp.MustCompile(`(?:^|[\s;&|(])((?:\S*/)?rustup-init)\s+(?:-y|--)`)
	rustupPipe     = regexp.MustCompile(`rustup\.rs\b`)
	versionsVar    = regexp.MustCompile(`^([A-Z][A-Z0-9_]*)=(.*)$`)
)

// splitComment separates a trailing ` # comment` from a YAML value.
func splitComment(s string) (value, comment string) {
	if i := strings.Index(s, " #"); i >= 0 {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+2:])
	}
	return strings.TrimSpace(s), ""
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// imageRefProblem returns why ref (with its line's trailing comment) is not an
// acceptable pinned image, or "".
func imageRefProblem(ref, comment string) string {
	switch {
	case !digestRef.MatchString(ref):
		return "is not pinned by a full sha256 digest (want repo:tag@sha256:<64 hex>)"
	case !strings.Contains(refName(ref), ":"):
		return "has a digest but no readable tag before it (want repo:tag@sha256:...)"
	case comment == "":
		return "has no trailing # comment naming what the digest resolved to"
	}
	return ""
}

// refName is the repo:tag part of a digest reference, without its registry path.
func refName(ref string) string {
	name := ref[:strings.Index(ref, "@")]
	return name[strings.LastIndex(name, "/")+1:]
}

// ancestors lists the keys enclosing line i, nearest first.
func ancestors(lines []string, i int) []string {
	var out []string
	n := indentOf(lines[i])
	for j := i - 1; j >= 0 && n > 0; j-- {
		if strings.TrimSpace(lines[j]) == "" || isComment(lines[j]) {
			continue
		}
		if k := indentOf(lines[j]); k < n {
			out = append(out, strings.TrimSuffix(strings.TrimSpace(lines[j]), ":"))
			n = k
		}
	}
	return out
}

func inImageBlock(lines []string, i int) bool {
	for _, a := range ancestors(lines, i) {
		if a == "container" || a == "services" {
			return true
		}
	}
	return false
}

// containerImages returns every image reference a workflow pulls: a job's
// `container:` (scalar or `image:` mapping), a `services:` image, a
// `docker://` action, and the image argument of docker/podman run|pull|create
// in a run body. Each is reported with its 1-based line and trailing comment.
type imageUse struct {
	n       int
	ref     string
	comment string
}

func containerImages(lines []string) []imageUse {
	var out []imageUse
	for i, l := range lines {
		if isComment(l) {
			continue
		}
		if m := dockerUsesLine.FindStringSubmatch(l); m != nil {
			_, c := splitComment(l)
			out = append(out, imageUse{i + 1, m[1], c})
			continue
		}
		m := imageKeyLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if m[1] == "image" && !inImageBlock(lines, i) {
			continue
		}
		v, c := splitComment(m[2])
		if v == "" { // `container:` mapping form; its image: line is visited on its own
			continue
		}
		out = append(out, imageUse{i + 1, unquote(v), c})
	}
	for _, ll := range logicalLines(lines) {
		for _, seg := range commandSegments(ll.text) {
			if !containerRun.MatchString(seg) {
				continue
			}
			ref := strings.TrimSpace(seg)
			for _, f := range strings.Fields(seg) {
				if strings.Contains(f, "@sha256:") {
					ref = f
				}
			}
			out = append(out, imageUse{ll.n, ref, "docker/podman run in a run body"})
		}
	}
	return out
}

// containerImageViolations reports an image that is not pinned by digest.
func containerImageViolations(name string, lines []string) []string {
	var out []string
	for _, u := range containerImages(lines) {
		if p := imageRefProblem(u.ref, u.comment); p != "" {
			out = append(out, name+":"+strconv.Itoa(u.n)+": image "+strconv.Quote(u.ref)+" "+p)
		}
	}
	return out
}

// imageDriftViolations reports one repo:tag pinned to two different digests,
// across or within workflows. The two musl jobs must test the same image.
func imageDriftViolations(all map[string][]string) []string {
	seen := map[string]map[string]string{} // repo:tag -> digest -> first place
	var names []string
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, u := range containerImages(all[name]) {
			at := strings.Index(u.ref, "@sha256:")
			if at < 0 {
				continue
			}
			tag, digest := u.ref[:at], u.ref[at+1:]
			if seen[tag] == nil {
				seen[tag] = map[string]string{}
			}
			if _, ok := seen[tag][digest]; !ok {
				seen[tag][digest] = name + ":" + strconv.Itoa(u.n)
			}
		}
	}
	var out []string
	var tags []string
	for tag := range seen {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	for _, tag := range tags {
		if len(seen[tag]) > 1 {
			var where []string
			for d, at := range seen[tag] {
				where = append(where, at+" "+d)
			}
			sort.Strings(where)
			out = append(out, tag+" is pinned to different digests: "+strings.Join(where, "; "))
		}
	}
	return out
}

// rustupInitViolations reports an unverified rustup-init. A step that runs
// rustup-init must source tools/ci/versions.env, download the pinned
// $RUSTUP_VERSION, and, before each run, check the very path it runs against
// $RUSTUP_INIT_SHA256_X86_64_MUSL with `sha256sum -c` (a check of some other
// file does not count). `apk add rustup` is refused too: it installs a
// rustup-init this repo never checked, and the unverified-download shortcut
// (sh.rustup.rs) is refused for the same reason.
func rustupInitViolations(name string, lines []string) []string {
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
	for _, st := range workflowSteps(lines) {
		var body []string
		for _, l := range strings.Split(stepRunBody(st), "\n") {
			body = append(body, stripShellComment(l))
		}
		text := strings.Join(body, "\n")
		if !rustupExec.MatchString(text) {
			continue
		}
		step := name + ": step " + strconv.Quote(stepTitle(st)) + " runs rustup-init "
		if !strings.Contains(text, "tools/ci/versions.env") {
			out = append(out, step+"without sourcing tools/ci/versions.env")
		}
		for _, v := range []string{"RUSTUP_VERSION", "RUSTUP_INIT_SHA256_X86_64_MUSL"} {
			if !strings.Contains(text, "$"+v) && !strings.Contains(text, "${"+v) {
				out = append(out, step+"without using $"+v)
			}
		}
		// Each run of rustup-init needs an earlier `sha256sum -c` line that
		// names that same path and the pinned checksum: verifying some other
		// file, or any hash, proves nothing about what gets executed.
		for _, m := range rustupExec.FindAllStringSubmatchIndex(text, -1) {
			path := strings.Trim(text[m[2]:m[3]], `"'`)
			if !checksumVerifies(text[:m[0]], path) {
				out = append(out, step+"("+strconv.Quote(path)+") without an earlier `sha256sum -c` line that checks that path against $RUSTUP_INIT_SHA256_X86_64_MUSL")
			}
		}
	}
	return out
}

var sha256Check = regexp.MustCompile(`sha256sum\s+-c\b`)

// checksumVerifies reports whether a line of before runs `sha256sum -c` over
// path using the pinned checksum variable. The path must appear as a whole
// word ("./x" and "x" are the same file), so /tmp/rustup-init.new and
// /tmp/other do not count for /tmp/rustup-init.
func checksumVerifies(before, path string) bool {
	want := regexp.MustCompile(`(?:^|[\s"'=])(?:\./)?` + regexp.QuoteMeta(strings.TrimPrefix(path, "./")) + `(?:$|[\s"'])`)
	for _, l := range strings.Split(before, "\n") {
		if sha256Check.MatchString(l) &&
			(strings.Contains(l, "$RUSTUP_INIT_SHA256_X86_64_MUSL") || strings.Contains(l, "${RUSTUP_INIT_SHA256_X86_64_MUSL")) &&
			want.MatchString(l) {
			return true
		}
	}
	return false
}

// apkAddsRustup reports an `apk add` command line that names the rustup package.
func apkAddsRustup(f []string) bool {
	add := false
	for i, w := range f {
		if w == "apk" && i+1 < len(f) && f[i+1] == "add" {
			add = true
			continue
		}
		if add && strings.Trim(w, `"'`) == "rustup" {
			return true
		}
	}
	return false
}

func stepTitle(step []string) string {
	for _, l := range step {
		t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "- "))
		if strings.HasPrefix(t, "name:") {
			return strings.TrimSpace(strings.TrimPrefix(t, "name:"))
		}
	}
	return strings.TrimSpace(step[0])
}

func TestEveryContainerImageIsPinnedByDigest(t *testing.T) {
	all := workflows(t)
	for name, lines := range all {
		for _, v := range containerImageViolations(name, lines) {
			t.Error(v)
		}
	}
	for _, v := range imageDriftViolations(all) {
		t.Error(v)
	}
}

func TestRustupInitIsPinnedAndVerified(t *testing.T) {
	for name, lines := range workflows(t) {
		for _, v := range rustupInitViolations(name, lines) {
			t.Error(v)
		}
	}
	for _, p := range buildFiles {
		for _, v := range rustupInitViolations(p, readLines(t, p)) {
			t.Error(v)
		}
	}
}

// The pins the workflows read. A malformed value would make `sha256sum -c`
// fail late, in CI, instead of here.
func TestRustupPinsAreWellFormed(t *testing.T) {
	vars := map[string]string{}
	for _, l := range readLines(t, "../../tools/ci/versions.env") {
		if m := versionsVar.FindStringSubmatch(l); m != nil {
			vars[m[1]] = m[2]
		}
	}
	if v := vars["RUSTUP_VERSION"]; !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(v) {
		t.Errorf("tools/ci/versions.env RUSTUP_VERSION=%q, want an exact x.y.z", v)
	}
	if v := vars["RUSTUP_INIT_SHA256_X86_64_MUSL"]; !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(v) {
		t.Errorf("tools/ci/versions.env RUSTUP_INIT_SHA256_X86_64_MUSL=%q, want 64 lowercase hex", v)
	}
}

func TestPlantedImageViolationsAreCaught(t *testing.T) {
	const digest = "sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c"
	const other = "sha256:52efed73b4accb3fe90c2d53e297c917c578f80c995da88a4f0770d65cf49de1"
	ok := "    container: golang:1.27-alpine@" + digest + " # golang:1.27-alpine, Go 1.27.3"
	cases := []struct {
		name string
		got  []string
		good []string
	}{
		{"mutable tag", containerImageViolations("x", []string{"    container: golang:1.27-alpine"}), containerImageViolations("x", []string{ok})},
		{"untagged image", containerImageViolations("x", []string{"    container: golang"}), nil},
		{"quoted mutable tag", containerImageViolations("x", []string{`    container: "golang:1.27-alpine"`}), nil},
		{"63-hex digest", containerImageViolations("x", []string{"    container: golang:1.27-alpine@" + digest[:len(digest)-1] + " # c"}), nil},
		{"uppercase digest", containerImageViolations("x", []string{"    container: golang:1.27-alpine@sha256:" + strings.ToUpper(digest[7:]) + " # c"}), nil},
		{"sha512 digest", containerImageViolations("x", []string{"    container: golang:1.27-alpine@sha512:" + digest[7:] + " # c"}), nil},
		{"digest without a tag", containerImageViolations("x", []string{"    container: golang@" + digest + " # c"}), nil},
		{"digest without a comment", containerImageViolations("x", []string{"    container: golang:1.27-alpine@" + digest}), nil},
		{"matrix expression", containerImageViolations("x", []string{"    container: ${{ matrix.image }}"}), nil},
		{"mapping form", containerImageViolations("x", []string{"    container:", "      image: golang:1.27-alpine", "      options: --cpus 2"}), containerImageViolations("x", []string{"    container:", "      image: golang:1.27-alpine@" + digest + " # c"})},
		{"services image", containerImageViolations("x", []string{"    services:", "      db:", "        image: postgres:17"}), nil},
		{"docker:// action", containerImageViolations("x", []string{"      - uses: docker://alpine:3.24"}), nil},
		{"docker run in a run body", containerImageViolations("x", []string{"        run: docker run --rm alpine:3.24 true"}), nil},
		{"podman pull behind a pipe", containerImageViolations("x", []string{"        run: echo go | podman pull golang:1.27-alpine"}), nil},
		{"digest in a comment only", containerImageViolations("x", []string{"    container: golang:1.27-alpine # @" + digest}), nil},
		{"rustup-init with no checksum", rustupInitViolations("x", step("          rustup-init -y --default-toolchain stable")), rustupInitViolations("x", step(verified))},
		{"checksum after the run", rustupInitViolations("x", step(". tools/ci/versions.env\n          wget -O /tmp/rustup-init \"$RUSTUP_VERSION\"\n          /tmp/rustup-init -y\n          echo \"$RUSTUP_INIT_SHA256_X86_64_MUSL  /tmp/rustup-init\" | sha256sum -c -")), nil},
		{"checksum verifies a different file than the one run", rustupInitViolations("x", step(". tools/ci/versions.env\n          wget -q -O /tmp/rustup-init \"https://static.rust-lang.org/rustup/archive/$RUSTUP_VERSION/x86_64-unknown-linux-musl/rustup-init\"\n          echo \"$RUSTUP_INIT_SHA256_X86_64_MUSL  /tmp/other\" | sha256sum -c -\n          chmod +x /tmp/rustup-init\n          /tmp/rustup-init -y")), nil},
		{"checksum line does not use the pinned checksum", rustupInitViolations("x", step(". tools/ci/versions.env\n          wget -q -O /tmp/rustup-init \"https://static.rust-lang.org/rustup/archive/$RUSTUP_VERSION/x86_64-unknown-linux-musl/rustup-init\"\n          echo \"$RUSTUP_VERSION  /tmp/rustup-init\" | sha256sum -c -\n          /tmp/rustup-init -y")), nil},
		{"checksum on a bare name, run by path", rustupInitViolations("x", step(". tools/ci/versions.env\n          echo \"$RUSTUP_VERSION $RUSTUP_INIT_SHA256_X86_64_MUSL  rustup-init\" | sha256sum -c -\n          /tmp/rustup-init -y")), nil},
		{"checksum target only shares a prefix with the run path", rustupInitViolations("x", step(". tools/ci/versions.env\n          echo \"$RUSTUP_VERSION $RUSTUP_INIT_SHA256_X86_64_MUSL  /tmp/rustup-init.new\" | sha256sum -c -\n          /tmp/rustup-init -y")), nil},
		{"unpinned download", rustupInitViolations("x", step(". tools/ci/versions.env\n          wget -O /tmp/rustup-init https://static.rust-lang.org/rustup/dist/x86_64-unknown-linux-musl/rustup-init\n          echo \"$RUSTUP_INIT_SHA256_X86_64_MUSL  /tmp/rustup-init\" | sha256sum -c -\n          /tmp/rustup-init -y")), nil},
		{"checksum named in a comment", rustupInitViolations("x", step(". tools/ci/versions.env\n          # sha256sum -c $RUSTUP_VERSION $RUSTUP_INIT_SHA256_X86_64_MUSL\n          rustup-init -y")), nil},
		{"apk add rustup", rustupInitViolations("x", []string{"          apk add --no-cache build-base rustup"}), rustupInitViolations("x", []string{"          apk add --no-cache build-base musl-dev"})},
		{"curl rustup.rs piped to sh", rustupInitViolations("x", []string{"          curl https://sh.rustup.rs -sSf | sh -s -- -y"}), nil},
	}
	for _, c := range cases {
		if len(c.got) == 0 {
			t.Errorf("%s: the planted violation was not caught", c.name)
		}
		if len(c.good) != 0 {
			t.Errorf("%s: the compliant form was flagged: %v", c.name, c.good)
		}
	}
	if d := imageDriftViolations(map[string][]string{
		"a.yml": {"    container: golang:1.27-alpine@" + digest + " # c"},
		"b.yml": {"    container: golang:1.27-alpine@" + other + " # c"},
	}); len(d) == 0 {
		t.Error("two digests for one tag: the planted drift was not caught")
	}
	if d := imageDriftViolations(map[string][]string{
		"a.yml": {"    container: golang:1.27-alpine@" + digest + " # c", "    container: golang:1.27-alpine@" + digest + " # c"},
	}); len(d) != 0 {
		t.Errorf("one digest used twice was flagged as drift: %v", d)
	}
}

const verified = `. tools/ci/versions.env
          wget -q -O /tmp/rustup-init "https://static.rust-lang.org/rustup/archive/$RUSTUP_VERSION/x86_64-unknown-linux-musl/rustup-init"
          echo "$RUSTUP_INIT_SHA256_X86_64_MUSL  /tmp/rustup-init" | sha256sum -c -
          chmod +x /tmp/rustup-init
          /tmp/rustup-init -y --default-toolchain stable --profile minimal`

// step wraps a run body in the one-step job shape the real workflows use.
func step(body string) []string {
	return strings.Split("      - name: Install toolchain\n        run: |\n          "+body, "\n")
}
