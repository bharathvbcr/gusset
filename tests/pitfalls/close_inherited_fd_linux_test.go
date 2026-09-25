//go:build linux

package pitfalls_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// writePipeFDs lists this process's descriptors that are write ends of pipes.
func writePipeFDs(t *testing.T) map[int]bool {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	out := map[int]bool{}
	for _, e := range ents {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		link, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil || !strings.HasPrefix(link, "pipe:") {
			continue
		}
		info, err := os.ReadFile(filepath.Join("/proc/self/fdinfo", e.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(info), "\n") {
			if f, ok := strings.CutPrefix(line, "flags:"); ok {
				flags, err := strconv.ParseInt(strings.TrimSpace(f), 8, 64)
				if err == nil && flags&syscall.O_ACCMODE == syscall.O_WRONLY {
					out[fd] = true
				}
			}
		}
	}
	return out
}

// Close waited for EOF on the completion pipe, which needs every copy of the
// write end closed. A copy that outlives Rust's — a child forked outside Go's
// ForkLock by Rust or C code, simulated here with a dup — left Close, every
// concurrent Close, and every Submit parked on a permit blocked forever.
func TestPitfall_CloseDoesNotWaitForAnInheritedWriteEnd(t *testing.T) {
	before := writePipeFDs(t)
	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	var dups []int
	for fd := range writePipeFDs(t) {
		if before[fd] {
			continue
		}
		d, err := syscall.Dup(fd)
		if err != nil {
			t.Fatal(err)
		}
		dups = append(dups, d)
	}
	defer func() {
		for _, d := range dups {
			_ = syscall.Close(d)
		}
	}()
	if len(dups) == 0 {
		t.Skip("could not locate the completion pipe's write end")
	}

	done := make(chan error, 1)
	go func() { done <- h.Close() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked waiting for EOF while another copy of the write end was open")
	}
}
