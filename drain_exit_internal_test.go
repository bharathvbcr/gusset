package gusset

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// killDrain makes drainPipe give up on an open handle, as a read error or a
// corrupt record not caused by Close would.
func killDrain(t *testing.T, s *handleState) {
	t.Helper()
	_ = s.pipe.SetReadDeadline(time.Now())
	select {
	case <-s.drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("drainPipe did not exit on a read error")
	}
	if !s.drainExited.Load() {
		t.Fatal("drainPipe exited without setting drainExited")
	}
}

// Once drainPipe has stopped reading, no completion can ever be delivered, so a
// submitter already parked on the pool semaphore must be refused rather than
// left parked, and must never be admitted to run a job nobody can collect.
//
// The drainExited refusal covered only callers arriving after the exit. One
// parked on a full pool stayed parked until Close: forever, with a context
// that has no deadline. And one woken by a permit coming back — a waiter told
// "handle closed" returns its permit — passed the closed and poisoned checks
// and was handed a ticket for work whose completion nobody would read (I4).
func TestDrainExit_ParkedSubmitterIsRefusedNotParkedOrAdmitted(t *testing.T) {
	for _, withWaiter := range []bool{false, true} {
		name := "permit-held"
		if withWaiter {
			name = "permit-returned-by-waiter"
		}
		t.Run(name, func(t *testing.T) {
			h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			s := h.state
			ctx := context.Background()

			// The pool's only permit, held by a 300 ms job.
			busy, err := h.Submit(ctx, []byte{9, 30})
			if err != nil {
				t.Fatal(err)
			}
			waited := make(chan error, 1)
			if withWaiter {
				go func() { _, err := h.Wait(ctx, busy); waited <- err }()
				eventually(t, "the waiter to register", func() bool {
					s.mu.Lock()
					defer s.mu.Unlock()
					_, ok := s.pending[busy]
					return ok
				})
			}

			type result struct {
				ticket uint64
				err    error
			}
			parked := make(chan result, 1)
			go func() {
				tk, err := h.Submit(ctx, []byte{0, 1})
				parked <- result{tk, err}
			}()
			time.Sleep(20 * time.Millisecond) // let it park on the full pool
			select {
			case r := <-parked:
				t.Fatalf("second submit on a full pool of one did not park: %+v", r)
			default:
			}

			killDrain(t, s)
			if withWaiter {
				if err := <-waited; err == nil {
					t.Fatal("waiter got a result after the reader gave up")
				}
			}

			select {
			case r := <-parked:
				if r.err == nil {
					t.Fatalf("parked submitter was admitted with ticket %d after the reader "+
						"gave up: its completion can never be delivered", r.ticket)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("parked submitter still parked 2 s after the reader gave up")
			}

			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			if n := len(s.sem); n != 0 {
				t.Fatalf("%d permits held after Close (I4)", n)
			}
		})
	}
}

// When drainPipe stops on its own — a corrupt record, an errno nobody asked
// for — the reason has to survive. It used to be dropped: every waiter got
// "gusset: handle closed", every later call ErrClosed, and nothing was logged,
// so a handle that had stopped working could not say why. The cause is now
// logged, kept on the handle, and carried by the waiter's error and every
// later refusal, which still match ErrClosed.
func TestDrainExit_CauseIsRetainedAndStillErrClosed(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state
	ctx := context.Background()

	busy, err := h.Submit(ctx, []byte{9, 30}) // 300 ms job
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { _, err := h.Wait(ctx, busy); waited <- err }()
	eventually(t, "the waiter to register", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, ok := s.pending[busy]
		return ok
	})

	// killDrain ends the reader with a read-deadline error on an open handle.
	killDrain(t, s)

	isCause := func(what string, err error, prefix string) {
		t.Helper()
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("%s: %v does not match ErrClosed", what, err)
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("%s: %v lost the reader's exit cause", what, err)
		}
		if !strings.HasPrefix(err.Error(), prefix) {
			t.Fatalf("%s: %q dropped the historical text %q", what, err, prefix)
		}
	}

	cause := s.drainErr.Load()
	if cause == nil {
		t.Fatal("drainPipe exited on an open handle without recording why")
	}
	if !errors.Is(cause.cause, os.ErrDeadlineExceeded) {
		t.Fatalf("recorded cause %v is not the read error", cause.cause)
	}
	isCause("waiter", <-waited, "gusset: handle closed")
	_, err = h.Submit(ctx, []byte{0, 1})
	isCause("later Submit", err, "gusset: handle is closed")
	_, err = h.Call(ctx, []byte{0, 1})
	isCause("later Call", err, "gusset: handle is closed")
	if !strings.Contains(logs.String(), "completion reader stopped") {
		t.Fatalf("the reader's exit was not logged; log:\n%s", logs.String())
	}

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// A reader stopped by Close is not a failure: no cause, no warning.
	logs.Reset()
	h2, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	if err := h2.Close(); err != nil {
		t.Fatal(err)
	}
	if c := h2.state.drainErr.Load(); c != nil {
		t.Fatalf("Close recorded a reader failure: %v", c)
	}
	if strings.Contains(logs.String(), "completion reader stopped") {
		t.Fatalf("Close logged a reader failure:\n%s", logs.String())
	}
}
