package gusset

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A closed handle's errors were distinct errors.New values with two different
// texts ("gusset: handle is closed" at the entry points, "gusset: handle
// closed" for a waiter released by the completion reader), so the only way to
// recognise one was to match strings. Every closed-handle error must now
// satisfy errors.Is(err, ErrClosed), and keep the text it always had, so the
// substring matchers adopters already wrote keep working.

// wantClosed fails unless err is ErrClosed and still carries legacyText.
func wantClosed(t *testing.T, what string, err error, legacyText string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: nil error, want ErrClosed", what)
		return
	}
	if !errors.Is(err, ErrClosed) {
		t.Errorf("%s: %q (%T) does not match errors.Is(err, ErrClosed)", what, err, err)
	}
	if !strings.Contains(err.Error(), legacyText) {
		t.Errorf("%s: %q lost the historical text %q", what, err, legacyText)
	}
}

// Every entry point that returns an error, called after Close.
func TestErrClosed_EveryEntryPointAfterClose(t *testing.T) {
	h, err := Open(WithPoolSize(2), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// A ticket still outstanding at Close, never waited on before it.
	outstanding, err := h.Submit(ctx, []byte{0, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	buf, err := h.NewBuffer(8 << 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	const is = "gusset: handle is closed"
	_, err = h.Submit(ctx, []byte{0})
	wantClosed(t, "Submit([]byte)", err, is)
	_, err = h.Submit(ctx, buf)
	wantClosed(t, "Submit(*Buffer)", err, is)
	_, err = h.Submit(ctx, nil)
	wantClosed(t, "Submit(nil)", err, is)
	_, err = h.Call(ctx, []byte{0})
	wantClosed(t, "Call", err, is)
	_, err = h.CallBuffer(ctx, buf)
	wantClosed(t, "CallBuffer", err, is)
	_, err = h.Wait(ctx, outstanding)
	wantClosed(t, "Wait(outstanding ticket)", err, is)
	_, err = h.WaitBuffer(ctx, outstanding)
	wantClosed(t, "WaitBuffer(outstanding ticket)", err, is)
	_, err = h.Wait(ctx, 1<<62)
	wantClosed(t, "Wait(unknown ticket)", err, is)
	_, err = h.NewBuffer(64)
	wantClosed(t, "NewBuffer", err, is)
	wantClosed(t, "Discard(outstanding ticket)", h.Discard(outstanding), is)
	wantClosed(t, "Discard(unknown ticket)", h.Discard(1<<62), is)

	// Buffer operations after Close are contract, not errors.
	if b := buf.Bytes(); b != nil {
		t.Errorf("Bytes after Close returned %d bytes, want nil", len(b))
	}
	if err := buf.Free(); err != nil {
		t.Errorf("Free after Close: %v, want nil", err)
	}
	// A second Close reports the first Close's outcome, not ErrClosed.
	if err := h.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// A waiter parked when the completion reader gives up is released with
// "gusset: handle closed" (drainPipe's exit), and a submitter parked on the
// full pool at that moment is refused. Both are ErrClosed.
func TestErrClosed_WaiterAndSubmitterReleasedByDrainExit(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s := h.state
	ctx := context.Background()

	busy, err := h.Submit(ctx, []byte{9, 30}) // holds the only permit for 300 ms
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { _, err := h.Wait(ctx, busy); waited <- err }()
	eventually(t, "the waiter to register", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.pending[busy] != nil
	})
	parked := make(chan error, 1)
	go func() { _, err := h.Submit(ctx, []byte{0}); parked <- err }()
	time.Sleep(20 * time.Millisecond) // let it park on the full pool

	killDrain(t, s)

	select {
	case err := <-waited:
		wantClosed(t, "Wait released by drainPipe exit", err, "gusset: handle closed")
	case <-time.After(5 * time.Second):
		t.Fatal("waiter not released by drainPipe exit")
	}
	select {
	case err := <-parked:
		wantClosed(t, "Submit parked across drainPipe exit", err, "gusset: handle is closed")
	case <-time.After(5 * time.Second):
		t.Fatal("parked submitter not released by drainPipe exit")
	}
	// And every later call, with the handle not yet closed.
	_, err = h.Call(ctx, []byte{0})
	wantClosed(t, "Call after drainPipe exit", err, "gusset: handle is closed")
	_, err = h.Wait(ctx, busy)
	wantClosed(t, "Wait after drainPipe exit", err, "gusset: handle is closed")
}

// bigInput is an echo (mode 0) input too long for an inline completion
// record, so its completion is a bare ticket collected with gusset_take.
func bigInput(n int) []byte {
	in := bytes.Repeat([]byte{7}, n)
	in[0] = 0
	return in
}

// A bare-ticket completion read while the handle is closing is answered
// "handle closed" by drainPipe itself (it cannot enter cgo to take it).
//
// White-box: closed is set by hand, the state close holds while it joins the
// workers, so the completion is read in exactly that window.
func TestErrClosed_CompletionReadDuringClose(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	s := h.state
	ctx := context.Background()

	in := append([]byte{9, 5, 9}, bytes.Repeat([]byte{1}, 200)...) // 50 ms, then echo 203 bytes
	tk, err := h.Submit(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { _, err := h.Wait(ctx, tk); waited <- err }()
	eventually(t, "the waiter to register", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.pending[tk] != nil
	})
	s.closed.Store(true)
	select {
	case err = <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter not released")
	}
	s.closed.Store(false) // let the real Close run
	wantClosed(t, "Wait whose completion was read during close", err, "gusset: handle closed")
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

// A large result already completed, collected after close has begun: Wait
// cannot copy it out and WaitBuffer cannot wrap it.
func TestErrClosed_CompletedTakeResultAfterCloseBegins(t *testing.T) {
	for _, viaBuffer := range []bool{false, true} {
		name := "Wait"
		if viaBuffer {
			name = "WaitBuffer"
		}
		t.Run(name, func(t *testing.T) {
			h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
			if err != nil {
				t.Fatal(err)
			}
			s := h.state
			ctx := context.Background()
			in, err := h.NewBuffer(16 << 10)
			if err != nil {
				t.Fatal(err)
			}
			copy(in.Bytes(), bigInput(16<<10))
			tk, err := h.Submit(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			eventually(t, "the result to complete", func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				_, ok := s.completed[tk]
				return ok && s.takeIDs[tk] != 0
			})
			s.closed.Store(true)
			if viaBuffer {
				_, err = h.WaitBuffer(ctx, tk)
			} else {
				_, err = h.Wait(ctx, tk)
			}
			s.closed.Store(false)
			wantClosed(t, name+" of a take result after close began", err, "gusset: handle is closed")
			_ = in.Free()
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Submit's buffer refusal is ErrClosed only when the handle being called is
// the one closing (a race with Close past the handle check, which the hammer
// drives). A freed buffer, or one belonging to another handle that is closed,
// leaves the called handle open: that must not read as ErrClosed.
func TestErrClosed_BufferRefusalsOfAnOpenHandleAreNot(t *testing.T) {
	h, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()

	freed, err := h.NewBuffer(64)
	if err != nil {
		t.Fatal(err)
	}
	_ = freed.Free()
	_, err = h.Submit(ctx, freed)
	if err == nil || errors.Is(err, ErrClosed) {
		t.Errorf("Submit of a freed buffer on an open handle: %v, want a non-ErrClosed refusal", err)
	}

	other, err := Open(WithPoolSize(1), WithDiagnosticEngine())
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := other.NewBuffer(64)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = h.Submit(ctx, foreign)
	if err == nil || errors.Is(err, ErrClosed) {
		t.Errorf("Submit of another closed handle's buffer on an open handle: %v, want a non-ErrClosed refusal", err)
	}
	if errBufferClosed.Error() != "gusset: buffer is freed or closed" || !errors.Is(errBufferClosed, ErrClosed) {
		t.Errorf("errBufferClosed = %q, want the historical text and ErrClosed", errBufferClosed)
	}
}
