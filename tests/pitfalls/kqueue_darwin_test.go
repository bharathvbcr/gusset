//go:build darwin

package pitfalls_test

import (
	"bytes"
	"context"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// TestDarwin_CompletionReaderParksInKqueueUnderRawConnRead asserts that on macOS (darwin),
// the Gusset completion reader parks in the Darwin kqueue event loop via RawConn.Read
// when idle, and is woken via kqueue EVFILT_READ notification on completion.
//
// Addresses BUILD_REPORT.md:115 (ft-83151a08) and docs/integrations.md:242.
func TestDarwin_CompletionReaderParksInKqueueUnderRawConnRead(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("kqueue reader test is macOS-specific")
	}

	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	// Give the reader goroutine time to drain initial queue and park
	deadline := time.Now().Add(3 * time.Second)
	parked := false
	for time.Now().Before(deadline) {
		buf := make([]byte, 64*1024)
		n := runtime.Stack(buf, true)
		trace := string(buf[:n])

		// Look for drainPipe / ticketReader goroutine parked in netpoll / IO wait
		if strings.Contains(trace, "drainPipe") && strings.Contains(trace, "[IO wait]") {
			if strings.Contains(trace, "rawConn.Read") || strings.Contains(trace, "internal/poll") {
				parked = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !parked {
		t.Fatalf("drainPipe reader goroutine did not park in kqueue under RawConn.Read [IO wait]")
	}

	// Verify that submitting work wakes the parked kqueue reader and completes cleanly
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	payload := []byte{0, 42, 43, 44}
	res, err := h.Call(ctx, payload)
	if err != nil {
		t.Fatalf("Call on handle with kqueue reader: %v", err)
	}
	if !bytes.Equal(res, payload) {
		t.Fatalf("got %v, want %v", res, payload)
	}
}

// TestDarwin_RawConnPipeKqueueParkAndWake explicitly exercises the exact RawConn.Read
// non-blocking EAGAIN -> kqueue EVFILT_READ registration -> park -> wake cycle used by
// ticketReader on macOS.
func TestDarwin_RawConnPipeKqueueParkAndWake(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("kqueue test is macOS-specific")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	rc, err := r.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}

	// Set non-blocking on read end
	if err := syscall.SetNonblock(int(r.Fd()), true); err != nil {
		t.Fatalf("SetNonblock: %v", err)
	}

	readDone := make(chan []byte, 1)
	readErr := make(chan error, 1)

	// Launch reader goroutine that parks in kqueue when EAGAIN is encountered
	go func() {
		buf := make([]byte, 8)
		var n int
		var rerr error
		err := rc.Read(func(fd uintptr) bool {
			m, e := syscall.Read(int(fd), buf)
			if m > 0 {
				n = m
				return true
			}
			if e == syscall.EAGAIN {
				// Returning false instructs the Go runtime netpoller to register
				// the file descriptor with kqueue (EVFILT_READ) and park the goroutine.
				return false
			}
			rerr = e
			return true
		})
		if err != nil {
			readErr <- err
			return
		}
		if rerr != nil {
			readErr <- rerr
			return
		}
		readDone <- buf[:n]
	}()

	// Verify reader goroutine is parked in [IO wait] on kqueue
	parked := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		buf := make([]byte, 64*1024)
		sz := runtime.Stack(buf, true)
		trace := string(buf[:sz])
		if strings.Contains(trace, "TestDarwin_RawConnPipeKqueueParkAndWake") && strings.Contains(trace, "[IO wait]") {
			parked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !parked {
		t.Fatalf("reader goroutine did not park in kqueue under RawConn.Read [IO wait]")
	}

	// Write wake payload to pipe - this activates EVFILT_READ in kqueue, waking netpoller
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	if _, err := w.Write(want); err != nil {
		t.Fatalf("write to pipe: %v", err)
	}

	select {
	case got := <-readDone:
		if !bytes.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	case err := <-readErr:
		t.Fatalf("read failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatalf("kqueue did not wake reader goroutine within 3s")
	}
}
