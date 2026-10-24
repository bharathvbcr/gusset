// Package hostprobe is the host-load skip used by tests that probe a live job.
//
// A probe that landed after the job ended proves nothing. Skipping it is right
// on a developer machine and wrong in CI: a check that did not run must not
// look like a pass. GUSSET_REQUIRE_PROBES=1 turns that skip into a failure.
// Every workflow that runs go test sets it once, in its top-level env:, so it
// reaches every job (tests/ci/workflow_supplychain_test.go,
// TestWorkflowLevelProbeEnv). The helper is not part of the public gusset API.
package hostprobe

import (
	"fmt"
	"os"
	"testing"
)

// Skip skips t with reason, unless GUSSET_REQUIRE_PROBES=1, in which case it
// fails with the same text. format is fmt.Sprintf'd so call sites that used
// t.Skipf keep one message for both outcomes.
//
// The contract: Skip is only for a condition a quiet CI runner never meets,
// which is real host load or noise (a goroutine stalled past the end of the
// job it was probing, a timer woken late by a busy machine). Because CI sets
// GUSSET_REQUIRE_PROBES=1, a Skip that a quiet runner can reach fails every
// run there. Skip is never for a deterministic platform or runtime property:
// one that holds on every run of some host (an OS, an architecture, a build
// tag, a runtime constant) needs a fix to the test or a platform gate
// (t.Skip on runtime.GOOS, a build constraint), not this helper.
//
// TestSerialCall_LeavesAPForOtherGoroutines broke the contract. Its idle
// baseline timed 200 us naps in a process with every P idle, where Go's Linux
// netpoller rounds a sub-millisecond timer wait up to epoll_wait(1)
// (runtime/netpoll_epoll.go). The baseline read ~860 us on every Linux host,
// so the noise threshold fired on every Linux runner and the switch made it a
// failure (Matrix 39270656716). The fix moved the baseline off that floor; it
// did not loosen the threshold.
func Skip(t testing.TB, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv("GUSSET_REQUIRE_PROBES") == "1" {
		t.Fatalf("%s", msg)
	}
	t.Skip(msg)
}
