// Package hostprobe is the host-load skip used by tests that probe a live job.
//
// A probe that landed after the job ended proves nothing. Skipping it is right
// on a developer machine and wrong in CI: a check that did not run must not
// look like a pass. GUSSET_REQUIRE_PROBES=1, which the workflow go test steps
// set, turns that skip into a failure. The helper is not part of the public
// gusset API.
package hostprobe

import (
	"fmt"
	"os"
	"testing"
)

// Skip skips t with reason, unless GUSSET_REQUIRE_PROBES=1, in which case it
// fails with the same text. format is fmt.Sprintf'd so call sites that used
// t.Skipf keep one message for both outcomes.
func Skip(t testing.TB, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv("GUSSET_REQUIRE_PROBES") == "1" {
		t.Fatalf("%s", msg)
	}
	t.Skip(msg)
}
