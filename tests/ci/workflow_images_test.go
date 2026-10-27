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
	return append(out, commandImages(lines)...)
}

// commandImages returns the image operand of every docker, podman or nerdctl
// run, pull or create in lines (`container run` and `image pull` forms, and
// `$(DOCKER)` or `${DOCKER}` for the binary included), which may be a
// workflow's run bodies or a plain shell script or Makefile. It is the whole of
// the image rule for the latter: `container:` and `image:` are YAML keys, and
// in a Makefile `image:` is a target. Compose files and `docker buildx` are not
// read: the repository uses neither.
//
// The operand is the first argument that is not an option or an option's value,
// so `-e BASE=golang:1@sha256:<hex> alpine:3.24` names alpine:3.24, not the
// digest that sits in an environment variable.
func commandImages(lines []string) []imageUse {
	var out []imageUse
	for _, ll := range logicalLines(lines) {
		for _, c := range shellCommands(flattenSubst(ll.text, engineWords)) {
			if ref, ok := imageOperand(c.words); ok {
				if ref == "" {
					ref = strings.Join(c.words, " ")
				}
				out = append(out, imageUse{ll.n, unquote(ref), "docker/podman run in a run body"})
			}
		}
	}
	return out
}

// containerImageViolations reports an image that is not pinned by digest.
func containerImageViolations(name string, lines []string) []string {
	return imageViolations(name, containerImages(lines))
}

// scriptImageViolations is containerImageViolations for a build script or
// Makefile: only the images its docker/podman commands name.
func scriptImageViolations(name string, lines []string) []string {
	return imageViolations(name, commandImages(lines))
}

func imageViolations(name string, uses []imageUse) []string {
	var out []string
	for _, u := range uses {
		if p := imageRefProblem(u.ref, u.comment); p != "" {
			out = append(out, name+":"+strconv.Itoa(u.n)+": image "+strconv.Quote(u.ref)+" "+p)
		}
	}
	return out
}

// imageDriftViolations reports one repo:tag pinned to two different digests,
// across or within workflows. The two musl jobs must test the same image.
func imageDriftViolations(all map[string][]imageUse) []string {
	seen := map[string]map[string]string{} // repo:tag -> digest -> first place
	var names []string
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, u := range all[name] {
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
	uses := map[string][]imageUse{}
	for name, lines := range workflows(t) {
		for _, v := range containerImageViolations(name, lines) {
			t.Error(v)
		}
		uses[name] = containerImages(lines)
	}
	// The build scripts CI trusts are held to the same rule: a docker or podman
	// command in one of them is an image pulled outside every workflow.
	for _, p := range buildFiles {
		lines := readLines(t, p)
		for _, v := range scriptImageViolations(p, lines) {
			t.Error(v)
		}
		uses[p] = commandImages(lines)
	}
	for _, v := range imageDriftViolations(uses) {
		t.Error(v)
	}
}

func TestRustupInitIsPinnedAndVerified(t *testing.T) {
	runs := 0
	for name, lines := range workflows(t) {
		for _, v := range rustupInitViolations(name, lines) {
			t.Error(v)
		}
		for _, st := range workflowSteps(lines) {
			runs += len(rustupRuns(rustupStepText(st)))
		}
	}
	// The musl jobs in matrix.yml run rustup-init twice. A detector that finds
	// none would pass every workflow, however it was written.
	if runs < 2 {
		t.Errorf("found %d rustup-init runs in the workflows, want the 2 the musl jobs make; the rule is not looking at them", runs)
	}
	for _, p := range buildFiles {
		for _, v := range rustupScriptViolations(p, readLines(t, p)) {
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
		// Bypasses of the run detection: it used to match only `rustup-init -y`
		// and `rustup-init --`, so each of these skipped the whole rule.
		{"flags before -y, unchecked", rustupInitViolations("x", step(pinned("", "", "/tmp/rustup-init -q -y"))), rustupInitViolations("x", step(pinned(okCheck, "", "/tmp/rustup-init -q -y")))},
		{"bare invocation, unchecked", rustupInitViolations("x", step(pinned("", "", "/tmp/rustup-init"))), rustupInitViolations("x", step(pinned(okCheck, "", "/tmp/rustup-init")))},
		{"a long option first, unchecked", rustupInitViolations("x", step(pinned("", "", "/tmp/rustup-init --profile minimal -y"))), rustupInitViolations("x", step(pinned(okCheck, "", "/tmp/rustup-init --profile minimal -y")))},
		{"run behind sudo, unchecked", rustupInitViolations("x", step(pinned("", "", "sudo /tmp/rustup-init -q -y"))), rustupInitViolations("x", step(pinned(okCheck, "", "sudo /tmp/rustup-init -q -y")))},
		{"run through sh, unchecked", rustupInitViolations("x", step(pinned("", "", "sh /tmp/rustup-init -q -y"))), nil},
		{"run after && on a continued line, unchecked", rustupInitViolations("x", step(pinned("", "", "chmod +x /tmp/rustup-init && \\\n            /tmp/rustup-init \\\n            -q -y"))), nil},
		// A check that cannot stop the step is not a check.
		{"check followed by || true", rustupInitViolations("x", step(pinned(okCheck+" || true", "", "/tmp/rustup-init -y"))), rustupInitViolations("x", step(pinned(okCheck+" || exit 1", "", "/tmp/rustup-init -y")))},
		{"check followed by || :", rustupInitViolations("x", step(pinned(okCheck+" || :", "", "/tmp/rustup-init -y"))), nil},
		{"check followed by || echo", rustupInitViolations("x", step(pinned(okCheck+" || echo mismatch", "", "/tmp/rustup-init -y"))), nil},
		{"check followed by || bare exit", rustupInitViolations("x", step(pinned(okCheck+" || exit", "", "/tmp/rustup-init -y"))), nil},
		{"check followed by || exit 0", rustupInitViolations("x", step(pinned(okCheck+" || exit 0", "", "/tmp/rustup-init -y"))), nil},
		{"check with an unstoppable handler, run on the same line", rustupInitViolations("x", step(pinned("", "", okCheck+" || true && /tmp/rustup-init -y"))), nil},
		{"check by --check", rustupInitViolations("x", step(pinned(strings.Replace(okCheck, "sha256sum -c -", "sha256sum --check - || true", 1), "", "/tmp/rustup-init -y"))), rustupInitViolations("x", step(pinned(strings.Replace(okCheck, "sha256sum -c -", "sha256sum --check -", 1), "", "/tmp/rustup-init -y")))},
		// Replacing the file after it was checked.
		{"wget re-download after the check", rustupInitViolations("x", step(pinned(okCheck, `wget -q -O /tmp/rustup-init "https://static.rust-lang.org/rustup/dist/x86_64-unknown-linux-musl/rustup-init"`, "/tmp/rustup-init -y"))), nil},
		{"curl re-download after the check", rustupInitViolations("x", step(pinned(okCheck, `curl -sSfo /tmp/rustup-init "https://static.rust-lang.org/rustup/dist/x86_64-unknown-linux-musl/rustup-init"`, "/tmp/rustup-init -y"))), nil},
		{"curl -O into the working directory after the check", rustupInitViolations("x", step(pinned(strings.ReplaceAll(okCheck, "/tmp/rustup-init", "rustup-init"), `curl -sSfO "https://static.rust-lang.org/rustup/dist/x86_64-unknown-linux-musl/rustup-init"`, "./rustup-init -y"))), nil},
		{"cp over the checked file", rustupInitViolations("x", step(pinned(okCheck, "cp /tmp/other-binary /tmp/rustup-init", "/tmp/rustup-init -y"))), nil},
		{"redirect over the checked file", rustupInitViolations("x", step(pinned(okCheck, "cat /tmp/other-binary > /tmp/rustup-init", "/tmp/rustup-init -y"))), nil},
		{"re-download on the line of the check", rustupInitViolations("x", step(pinned(okCheck+` && wget -q -O /tmp/rustup-init "https://static.rust-lang.org/rustup/dist/x86_64-unknown-linux-musl/rustup-init"`, "", "/tmp/rustup-init -y"))), nil},
		{"apk add rustup", rustupInitViolations("x", []string{"          apk add --no-cache build-base rustup"}), rustupInitViolations("x", []string{"          apk add --no-cache build-base musl-dev"})},
		{"curl rustup.rs piped to sh", rustupInitViolations("x", []string{"          curl https://sh.rustup.rs -sSf | sh -s -- -y"}), nil},
		// The image rule used to read workflows only; the build scripts CI
		// trusts (buildFiles) are Makefile and shell, where only a
		// docker/podman command can name an image.
		{"unpinned image in a Makefile recipe", scriptImageViolations("Makefile", []string{"cross-musl:", "\t@docker run --rm -v $(CURDIR):/src alpine:3.24 sh -c 'cargo build'"}), scriptImageViolations("Makefile", []string{"cross-musl:", "\t@docker run --rm -v $(CURDIR):/src alpine:3.24@" + digest + " sh -c 'cargo build'"})},
		{"unpinned image in a shell script", scriptImageViolations("run.sh", []string{"#!/bin/sh", "set -eu", "podman pull golang:1.27-alpine"}), scriptImageViolations("run.sh", []string{"#!/bin/sh", "podman pull golang:1.27-alpine@" + digest})},
		{"unpinned image after a sudo and a pipe in a script", scriptImageViolations("run.sh", []string{"echo go | sudo docker create golang:1.27-alpine"}), nil},
		{"image behind a line continuation in a script", scriptImageViolations("run.sh", []string{"docker run --rm \\", "  alpine:3.24 \\", "  true"}), nil},
	}
	for _, c := range cases {
		if len(c.got) == 0 {
			t.Errorf("%s: the planted violation was not caught", c.name)
		}
		if len(c.good) != 0 {
			t.Errorf("%s: the compliant form was flagged: %v", c.name, c.good)
		}
	}
	// Forms the rustup rule must accept: it is only worth tightening if the
	// real workflow's shape still passes.
	for name, body := range map[string]string{
		"the real shape":                   verified,
		"the run on the line of its check": pinned("", "", okCheck+" && /tmp/rustup-init -q -y"),
		"a failing handler with false":     pinned(okCheck+" || false", "", "/tmp/rustup-init -y"),
		"chmod between check and run":      pinned(okCheck, "chmod +x /tmp/rustup-init", "/tmp/rustup-init -q -y"),
		"a second check after a rewrite":   pinned(okCheck, "cp /tmp/x /tmp/rustup-init\n          "+okCheck, "/tmp/rustup-init -y"),
		"a redirect onto a longer name":    pinned(okCheck, "cat /tmp/x > /tmp/rustup-init.new", "/tmp/rustup-init -y"),
		"a download of something else":     pinned(okCheck, `wget -q -O /tmp/other "https://example.invalid/other"`, "/tmp/rustup-init -y"),
		"long options after -y":            pinned(okCheck, "", "/tmp/rustup-init -y --default-toolchain stable"),
	} {
		if v := rustupInitViolations("x", step(body)); len(v) != 0 {
			t.Errorf("%s: the compliant form was flagged: %v", name, v)
		}
	}
	// YAML keys are not read in a script: a Makefile target named image, or a
	// word container after a colon, is not an image reference.
	if v := scriptImageViolations("Makefile", []string{"image:", "\t@echo build", "container: image"}); len(v) != 0 {
		t.Errorf("a Makefile target named image was read as an image: %v", v)
	}
	if d := imageDriftViolations(map[string][]imageUse{
		"a.yml": containerImages([]string{"    container: golang:1.27-alpine@" + digest + " # c"}),
		"b.yml": containerImages([]string{"    container: golang:1.27-alpine@" + other + " # c"}),
	}); len(d) == 0 {
		t.Error("two digests for one tag: the planted drift was not caught")
	}
	if d := imageDriftViolations(map[string][]imageUse{
		"a.yml": containerImages([]string{"    container: golang:1.27-alpine@" + digest + " # c", "    container: golang:1.27-alpine@" + digest + " # c"}),
	}); len(d) != 0 {
		t.Errorf("one digest used twice was flagged as drift: %v", d)
	}
}

const verified = `. tools/ci/versions.env
          wget -q -O /tmp/rustup-init "https://static.rust-lang.org/rustup/archive/$RUSTUP_VERSION/x86_64-unknown-linux-musl/rustup-init"
          echo "$RUSTUP_INIT_SHA256_X86_64_MUSL  /tmp/rustup-init" | sha256sum -c -
          chmod +x /tmp/rustup-init
          /tmp/rustup-init -y --default-toolchain stable --profile minimal`

// okCheck is the check line the real workflows use.
const okCheck = `echo "$RUSTUP_INIT_SHA256_X86_64_MUSL  /tmp/rustup-init" | sha256sum -c -`

// pinned builds a run body shaped like the real one: the pinned download, then
// check, then between, then run. An empty check or between leaves a blank line.
func pinned(check, between, run string) string {
	return ". tools/ci/versions.env\n" +
		"          wget -q -O /tmp/rustup-init \"https://static.rust-lang.org/rustup/archive/$RUSTUP_VERSION/x86_64-unknown-linux-musl/rustup-init\"\n" +
		"          " + check + "\n" +
		"          " + between + "\n" +
		"          " + run
}

// step wraps a run body in the one-step job shape the real workflows use.
func step(body string) []string {
	return strings.Split("      - name: Install toolchain\n        run: |\n          "+body, "\n")
}
