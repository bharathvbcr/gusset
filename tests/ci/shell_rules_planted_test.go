package ci_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Planted violations for the rules in shell_rules_test.go. Every case is a pair:
// a form the rule must report and, where one exists, the compliant form beside
// it that it must not. A rule that cannot fail is not a rule, so each bypass
// found in review is a row here.

const plantedDigest = "sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c"

// unnamedStep wraps a run body in a step with no name, whose first line is
// `- run: |`.
func unnamedStep(body string) []string {
	return strings.Split("      - run: |\n          "+body, "\n")
}

// withHeader wraps a run body in a named step whose run key carries header.
func withHeader(header, body string) []string {
	return strings.Split("      - name: Install toolchain\n        run: "+header+"\n          "+body, "\n")
}

var runKeyLine = regexp.MustCompile(`^\s*(?:- )?run:`)

// unparsedRunKeys reports a workflow whose run keys are not all inside a step
// workflowSteps found. The step parser reads steps at one indentation; a step
// written at another would hold a run body no rule looks at, and the rules
// would pass over it. Counting keys against parsed steps fails loud instead.
func unparsedRunKeys(name string, lines []string) []string {
	want := 0
	for _, l := range lines {
		if !isComment(l) && runKeyLine.MatchString(l) {
			want++
		}
	}
	got := 0
	for _, st := range workflowSteps(lines) {
		for _, l := range st {
			if !isComment(l) && runKeyLine.MatchString(l) {
				got++
			}
		}
	}
	if got == want {
		return nil
	}
	return []string{name + ": " + strconv.Itoa(want) + " run keys but only " + strconv.Itoa(got) + " inside steps the parser read; a step at another indentation, or a layout it does not know, hides its run body from every rule"}
}

func TestEveryRunKeyIsInAParsedStep(t *testing.T) {
	runs := 0
	for name, lines := range workflows(t) {
		for _, v := range unparsedRunKeys(name, lines) {
			t.Error(v)
		}
		for _, st := range workflowSteps(lines) {
			if stepRunBody(st) != "" {
				runs++
			}
		}
	}
	if runs == 0 {
		t.Fatal("no run bodies found in the workflows; the step parser reads nothing")
	}
	// The planted layout the parser cannot read must be reported, and the one
	// it reads must not.
	fourSpace := strings.Split("    - name: Install\n      run: |\n        /tmp/rustup-init -y", "\n")
	if len(unparsedRunKeys("x", fourSpace)) == 0 {
		t.Error("a step indented four spaces was not reported")
	}
	if v := unparsedRunKeys("x", step("echo hi")); len(v) != 0 {
		t.Errorf("the usual step layout was reported: %v", v)
	}
	if v := unparsedRunKeys("x", unnamedStep("echo hi")); len(v) != 0 {
		t.Errorf("an unnamed step was reported: %v", v)
	}
}

func TestWorkflowFilesReadsBothExtensions(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.yml", "b.yaml", "c.txt", "d.yml.bak"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("on: push\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := workflowFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("workflowFiles found %v, want exactly a.yml and b.yaml", got)
	}
}

func TestPlantedRustupBypassesAreCaught(t *testing.T) {
	run := "/tmp/rustup-init -y"
	type c struct {
		name string
		bad  []string
		good []string
	}
	var cases []c

	// Step layouts: an unnamed step, a one-line run, every block scalar header.
	cases = append(cases,
		c{"unnamed step, block scalar", rustupInitViolations("x", unnamedStep(pinned("", "", run))), rustupInitViolations("x", unnamedStep(pinned(okCheck, "", run)))},
		c{"unnamed step, single-line run", rustupInitViolations("x", []string{"      - run: /tmp/rustup-init -y"}), rustupInitViolations("x", []string{"      - run: . tools/ci/versions.env; wget -q -O /tmp/rustup-init \"https://example.invalid/$RUSTUP_VERSION/rustup-init\"; " + okCheck + " && /tmp/rustup-init -y"})},
		c{"named step, single-line run", rustupInitViolations("x", []string{"      - name: Install", "        run: /tmp/rustup-init -y"}), nil},
	)
	for _, h := range []string{"|", "|+", "|-", ">", ">-", ">+", "|2", "|2-", "|-2", "|-  # why", "|  "} {
		cases = append(cases, c{"block scalar header " + strconv.Quote(h), rustupInitViolations("x", withHeader(h, pinned("", "", run))), rustupInitViolations("x", withHeader(h, pinned(okCheck, "", run)))})
	}

	// Wrappers: each runs rustup-init and must be held to the rule.
	for _, w := range []string{"timeout 300 ", "timeout -s KILL 5s ", "timeout --kill-after=5 60 ", "nice ", "nice -n 10 ", "ionice -c 3 ", "sudo ", "sudo -E ", "sudo -u root ", "env ", "env -i HOME=/x ", "env -u FOO X=1 ", ". ", "source ", "exec ", "sh ", "bash ", "time ", "nohup ", "stdbuf -o0 ", "if ", "! ", "FOO=1 ", "sudo timeout 60 nice -n 5 "} {
		cases = append(cases, c{"run behind " + strconv.Quote(strings.TrimSpace(w)), rustupInitViolations("x", step(pinned("", "", w+"/tmp/rustup-init -q -y"))), rustupInitViolations("x", step(pinned(okCheck, "", w+"/tmp/rustup-init -q -y")))})
	}
	cases = append(cases,
		c{"run in a subshell", rustupInitViolations("x", step(pinned("", "", "(/tmp/rustup-init -y)"))), nil},
		c{"run in a command substitution", rustupInitViolations("x", step(pinned("", "", "out=$(/tmp/rustup-init -y)"))), nil},
		c{"run in backticks", rustupInitViolations("x", step(pinned("", "", "out=`/tmp/rustup-init -y`"))), nil},
		c{"run through sh -c", rustupInitViolations("x", step(pinned("", "", `sh -c "/tmp/rustup-init -y"`))), nil},
	)

	// `command -v rustup-init` asks about the file and does not run it.
	if v := rustupInitViolations("x", step(". tools/ci/versions.env\n          command -v rustup-init")); len(v) != 0 {
		t.Errorf("command -v is a question, not a run, but was reported: %v", v)
	}

	// A check that does not stop the step is not a check.
	for name, check := range map[string]string{
		"|| echo naming exit":        okCheck + ` || echo "would exit 1"`,
		"|| exit 1 || true":          okCheck + " || exit 1 || true",
		"|| true after exit 1 list":  okCheck + " || true || exit 1",
		"|| false then a semicolon":  okCheck + " || false; true",
		"|| a group handler":         okCheck + " || { echo mismatch; exit 1; }",
		"|| a subshell handler":      okCheck + " || (echo mismatch; exit 1)",
		"|| exit with a variable":    okCheck + " || exit $?",
		"|| exit 0":                  okCheck + " || exit 0",
		"|| true":                    okCheck + " || true",
		"backgrounded":               okCheck + " &",
		"backgrounded then more":     okCheck + " & sleep 1",
		"true || check":              "true || " + okCheck,
		"false || true || check":     "false || true || " + okCheck,
		"set +e before the check":    "set +e\n          " + okCheck,
		"set +eu before the check":   "set +eu\n          " + okCheck,
		"set -x +e before the check": "set -x +e\n          " + okCheck,
	} {
		good := ""
		switch name {
		case "set +e before the check", "set +eu before the check", "set -x +e before the check":
			good = "set -e\n          " + okCheck
		default:
			good = okCheck + " || exit 1"
		}
		cases = append(cases, c{name, rustupInitViolations("x", step(pinned(check, "", run))), rustupInitViolations("x", step(pinned(good, "", run)))})
	}

	// Handlers that do stop the step.
	for _, h := range []string{" || exit 1", " || exit 2", " || exit 127", " || false", " || exit 1 && true", " || false && true", " || exit 1; true", " 2>&1 || exit 1", " >/dev/null 2>&1 || exit 1", " &> /dev/null || exit 1"} {
		if v := rustupInitViolations("x", step(pinned(okCheck+h, "", run))); len(v) != 0 {
			t.Errorf("handler %q: the compliant form was flagged: %v", h, v)
		}
	}

	// Build scripts: the rule applies to every buildFiles entry.
	script := strings.Split(strings.ReplaceAll(pinned("", "", run), "\n          ", "\n"), "\n")
	goodScript := strings.Split(strings.ReplaceAll(pinned(okCheck, "", run), "\n          ", "\n"), "\n")
	cases = append(cases,
		c{"script, unchecked run", rustupScriptViolations("build.sh", script), rustupScriptViolations("build.sh", goodScript)},
		c{"script, check ending || true", rustupScriptViolations("build.sh", strings.Split(strings.ReplaceAll(pinned(okCheck+" || true", "", run), "\n          ", "\n"), "\n")), nil},
		c{"Makefile recipe, unchecked run", rustupScriptViolations("Makefile", []string{"toolchain:", "\t@. tools/ci/versions.env && wget -q -O $(TMP)/rustup-init \"$$RUSTUP_VERSION\" && $(TMP)/rustup-init -y"}), nil},
		c{"script run behind sudo", rustupScriptViolations("build.sh", []string{". tools/ci/versions.env", "wget -q -O /tmp/rustup-init \"$RUSTUP_VERSION\"", "sudo /tmp/rustup-init -y"}), nil},
		c{"script sh.rustup.rs", rustupScriptViolations("build.sh", []string{"curl https://sh.rustup.rs -sSf | sh -s -- -y"}), nil},
		c{"script apk add rustup", rustupScriptViolations("build.sh", []string{"apk add --no-cache rustup"}), rustupScriptViolations("build.sh", []string{"apk add --no-cache musl-dev"})},
	)

	for _, tc := range cases {
		if len(tc.bad) == 0 {
			t.Errorf("%s: the planted violation was not caught", tc.name)
		}
		if len(tc.good) != 0 {
			t.Errorf("%s: the compliant form was flagged: %v", tc.name, tc.good)
		}
	}
}

func TestPlantedImageOperandBypassesAreCaught(t *testing.T) {
	const pin = "alpine:3.24@" + plantedDigest
	type c struct {
		name string
		bad  []string
		good []string
	}
	img := func(cmd string) []string { return scriptImageViolations("run.sh", []string{cmd}) }
	mk := func(cmd string) []string { return scriptImageViolations("Makefile", []string{"t:", "\t" + cmd}) }
	cases := []c{
		{"a digest in an env value is not the image", img("docker run -e BASE=golang:1.27@" + plantedDigest + " alpine:3.24 true"), img("docker run -e BASE=golang:1.27 " + pin + " true")},
		{"a digest in a label value", img("docker run --label base=golang:1.27@" + plantedDigest + " alpine:3.24 true"), img("docker run --label base=x " + pin + " true")},
		{"a digest in an argument after the image", img("docker run alpine:3.24 echo golang:1.27@" + plantedDigest), img("docker run " + pin + " echo hi")},
		{"options with values before the image", img("docker run --rm --name x -p 80:80 -v /a:/b -w /src alpine:3.24 true"), img("docker run --rm --name x -p 80:80 -v /a:/b -w /src " + pin + " true")},
		{"an option=value form", img("docker run --network=host --entrypoint=/bin/sh alpine:3.24 -c true"), img("docker run --network=host --entrypoint=/bin/sh " + pin + " -c true")},
		{"container run", img("docker container run --rm alpine:3.24 true"), img("docker container run --rm " + pin + " true")},
		{"image pull", img("docker image pull alpine:3.24"), img("docker image pull " + pin)},
		{"container create", img("docker container create alpine:3.24"), img("docker container create " + pin)},
		{"podman container run", img("podman container run alpine:3.24 true"), nil},
		{"nerdctl run", img("nerdctl run --rm alpine:3.24 true"), img("nerdctl run --rm " + pin + " true")},
		{"nerdctl image pull", img("nerdctl image pull alpine:3.24"), nil},
		{"a global option with a value", img("docker --context remote run alpine:3.24 true"), img("docker --context remote run " + pin + " true")},
		{"a global option with =", img("docker --host=tcp://x:2375 pull alpine:3.24"), nil},
		{"a path to the binary", img("/usr/bin/docker run alpine:3.24 true"), nil},
		{"sudo and a path", img("sudo /usr/local/bin/podman run alpine:3.24 true"), nil},
		{"$(DOCKER) in a Makefile", mk("$(DOCKER) run --rm alpine:3.24 true"), mk("$(DOCKER) run --rm " + pin + " true")},
		{"${DOCKER} in a Makefile", mk("${DOCKER} run --rm alpine:3.24 true"), mk("${DOCKER} run --rm " + pin + " true")},
		{"$DOCKER in a script", img("$DOCKER run --rm alpine:3.24 true"), nil},
		{"$(PODMAN) container run", mk("$(PODMAN) container run alpine:3.24 true"), nil},
		{"a recipe prefix", mk("@docker run alpine:3.24 true"), mk("@docker run " + pin + " true")},
		{"an ignore-errors recipe prefix", mk("-docker run alpine:3.24 true"), nil},
		{"$(shell pwd) hides nothing", mk("docker run --rm -v $(shell pwd):/src alpine:3.24 true"), mk("docker run --rm -v $(shell pwd):/src " + pin + " true")},
		{"$(CURDIR) hides nothing", mk("docker run --rm -v $(CURDIR):/src alpine:3.24 true"), mk("docker run --rm -v $(CURDIR):/src " + pin + " true")},
		{"$(pwd) in a script", img("docker run -v $(pwd):/src alpine:3.24 true"), img("docker run -v $(pwd):/src " + pin + " true")},
		{"nested substitutions", mk("docker run -v $(abspath $(dir $(lastword $(MAKEFILE_LIST)))):/src alpine:3.24 true"), mk("docker run -v $(abspath $(dir $(lastword $(MAKEFILE_LIST)))):/src " + pin + " true")},
		{"a docker command inside a substitution", img("out=$(docker run alpine:3.24 true)"), img("out=$(docker run " + pin + " true)")},
		{"a variable image", img("docker run --rm $IMAGE true"), nil},
		{"no image at all", img("docker run --rm"), nil},
	}
	for _, tc := range cases {
		if len(tc.bad) == 0 {
			t.Errorf("%s: the planted violation was not caught", tc.name)
		}
		if len(tc.good) != 0 {
			t.Errorf("%s: the compliant form was flagged: %v", tc.name, tc.good)
		}
	}
	for _, cmd := range []string{
		"docker ps -a",
		"docker images",
		"docker image ls",
		"docker rm -f x",
		"which docker",
		"docker --version",
		"echo run docker",
	} {
		if v := img(cmd); len(v) != 0 {
			t.Errorf("%q names no image, but was reported: %v", cmd, v)
		}
	}
}
