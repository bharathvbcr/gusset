package pitfalls_test

import (
	"os/exec"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// Close must not depend on the lifetime of a child process.
//
// Open duplicated the completion pipe's write end with syscall.Dup, which does
// not set close-on-exec. Every subprocess the service started while a handle
// was open inherited that write end, so when Rust closed its copy the pipe
// still had a writer, drainPipe never saw EOF, and Close blocked until the
// child exited — for a long-lived child, forever. It also leaked the
// descriptor into processes that have no business holding it.
func TestPitfall_CloseDoesNotWaitForChildProcesses(t *testing.T) {
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		_ = h.Close()
		t.Skipf("cannot start a child process here: %v", err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()

	closed := make(chan error, 1)
	go func() { closed <- h.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked on a child process that inherited the completion pipe's write end")
	}
}
