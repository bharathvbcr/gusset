//go:build linux

package gusset

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// pipeQueued reports the bytes the kernel still holds unread in the pipe
// (FIONREAD), which tells a record stranded in the pipe from one never written.
func pipeQueued(f *os.File) string {
	rc, err := f.SyscallConn()
	if err != nil {
		return fmt.Sprintf("SyscallConn: %v", err)
	}
	var n int32
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCINQ, uintptr(unsafe.Pointer(&n)))
	}); err != nil {
		return fmt.Sprintf("Control: %v", err)
	}
	if errno != 0 {
		return fmt.Sprintf("FIONREAD: %v", errno)
	}
	return fmt.Sprintf("%d bytes queued unread", n)
}

// An empty pipe and a pipe holding a record must read differently, or the
// report cannot say whether a stalled completion was written at all.
func TestPipeQueued_CountsUnreadBytes(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if got := pipeQueued(r); got != "0 bytes queued unread" {
		t.Errorf("empty pipe reported %q", got)
	}
	if _, err := w.Write(make([]byte, 24)); err != nil {
		t.Fatal(err)
	}
	if got := pipeQueued(r); got != "24 bytes queued unread" {
		t.Errorf("pipe holding 24 bytes reported %q", got)
	}
}
