package hostprobe

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRequireProbesTurnsALateProbeIntoAFailure is the injected-delay proof for
// Skip. The delay lives here, not in the production probes: those still skip
// on a slow host unless the CI env is set. The child is a real go test, so a
// Skip and a Fatal are told apart by the test harness rather than by calling
// the helper in this process (which would end this test).
func TestRequireProbesTurnsALateProbeIntoAFailure(t *testing.T) {
	if os.Getenv("GUSSET_HOSTPROBE_CHILD") == "1" {
		time.Sleep(time.Millisecond) // the probe is already late
		Skip(t, "injected sleep: the probe ran past the job's end: it proves nothing on this host")
		return
	}

	for _, require := range []bool{false, true} {
		cmd := exec.Command("go", "test", "-count=1", "-timeout", "60s", "-v",
			"-run", "^TestRequireProbesTurnsALateProbeIntoAFailure$")
		cmd.Env = append(envWithout("GUSSET_REQUIRE_PROBES", "GUSSET_HOSTPROBE_CHILD"), "GUSSET_HOSTPROBE_CHILD=1")
		if require {
			cmd.Env = append(cmd.Env, "GUSSET_REQUIRE_PROBES=1")
		}
		out, err := cmd.CombinedOutput()
		text := string(out)
		const reason = "injected sleep: the probe ran past the job's end"
		if !strings.Contains(text, reason) {
			t.Fatalf("require=%v: child output does not name the late probe\n%v\n%s", require, err, text)
		}
		if require {
			if err == nil || !strings.Contains(text, "FAIL") {
				t.Fatalf("require=1: late probe skipped or passed, want a failure\n%v\n%s", err, text)
			}
			continue
		}
		if err != nil || !strings.Contains(text, "--- SKIP:") {
			t.Fatalf("require unset: late probe did not skip\n%v\n%s", err, text)
		}
	}
}

func envWithout(keys ...string) []string {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	var out []string
	for _, e := range os.Environ() {
		name, _, _ := strings.Cut(e, "=")
		if drop[name] {
			continue
		}
		out = append(out, e)
	}
	return out
}
