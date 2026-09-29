package gusset

// End-to-end differential fuzzing of a real handle: random sequences of
// Submit / Wait / WaitBuffer / cancel (a Wait whose context is already done)
// / Close against the diagnostic engine's echo (mode 0) and generated-output
// (mode 16) work units, compared with a model written from the documented
// contract rather than from the implementation.
//
// Invariants checked on every sequence:
//   - I4: permits are never leaked. Once every live ticket is collected and
//     every abandoned job has landed, the semaphore is empty; a Submit the
//     model says must be admitted is admitted within a bounded time.
//   - I1: every take() buffer is freed. Rust live bytes return to the
//     pre-sequence baseline after Close.
//   - Results are exact and reach the ticket that asked for them, on the
//     inline path (<= InlineResultMax), the Go-copy path (<= 4 KiB) and the
//     Rust-buffer path, through the ring and the pipe.
//
// Run with `go test -run '^$' -fuzz '^FuzzHandleOpSequence$' -fuzztime 5m .`

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

type modelTicket struct {
	want    []byte // expected result bytes
	wantErr string // expected error substring instead of bytes
}

// echoLens and allocLens are the payload sizes worth hitting: both sides of
// the inline record limit, of the 4 KiB copy limit, and of mode 16's 64 KiB
// chunk.
var (
	echoLens  = []int{0, 1, 2, 47, 48, 49, 100, 4095, 4096, 4097}
	allocLens = []int{-1, 0, 1, 48, 49, 4096, 4097, 65535, 65536, 65537, 1 << 20}
)

func mode16Input(n int, seed byte) []byte {
	if n < 0 {
		return []byte{16, 1} // too short for the length word: an engine error
	}
	in := []byte{16, 0, 0, 0, 0, seed}
	binary.LittleEndian.PutUint32(in[1:5], uint32(n))
	return in
}

func mode16Output(n int, seed byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i) ^ seed
	}
	return out
}

const opBound = 10 * time.Second

func FuzzHandleOpSequence(f *testing.F) {
	f.Add(uint8(0), []byte{0, 3, 2, 0})
	f.Add(uint8(1), []byte{1, 4, 0, 5, 2, 0, 3, 1})
	f.Add(uint8(5), []byte{0, 9, 1, 10, 4, 0, 5, 0, 6, 0, 0, 1, 7, 0, 2, 0, 5, 3})
	f.Add(uint8(3), []byte{1, 6, 1, 7, 1, 8, 4, 0, 4, 1, 4, 2, 0, 0})
	f.Add(uint8(6), []byte{0, 1, 0, 2, 0, 3, 0, 4, 4, 0, 4, 0, 4, 0, 4, 0, 0, 5, 2, 0})
	f.Add(uint8(2), []byte{7, 0, 0, 1, 2, 0, 7, 0})

	f.Fuzz(func(t *testing.T, cfg uint8, ops []byte) {
		if len(ops) > 96 {
			ops = ops[:96]
		}
		pool := int(cfg%4) + 1
		opts := []Option{WithPoolSize(pool), WithDiagnosticEngine()}
		if cfg&4 != 0 {
			opts = append(opts, withPipeOnly())
		}
		baseline := Stats().LiveBytes

		h, err := Open(opts...)
		if err != nil {
			t.Fatal(err)
		}
		s := h.state
		closed := false
		var closeErr error
		defer func() {
			if !closed {
				_ = h.Close()
			}
		}()

		live := map[uint64]modelTicket{}
		var order []uint64 // live tickets in submission order, for picking
		var spent []uint64 // collected or abandoned: must answer ErrUnknownTicket
		abandoned := 0

		pick := func(arg byte) (uint64, bool) {
			if len(order) == 0 {
				return 0, false
			}
			return order[int(arg)%len(order)], true
		}
		retire := func(tk uint64) {
			delete(live, tk)
			order = slices.DeleteFunc(order, func(x uint64) bool { return x == tk })
			spent = append(spent, tk)
		}
		bounded := func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), opBound)
		}
		done := func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}
		checkResult := func(what string, tk uint64, mt modelTicket, got []byte, err error) {
			t.Helper()
			if mt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), mt.wantErr) {
					t.Fatalf("%s ticket %d: err %v, want %q", what, tk, err, mt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s ticket %d: %v", what, tk, err)
			}
			if !bytes.Equal(got, mt.want) {
				t.Fatalf("%s ticket %d: %d bytes, want %d (first diff at %d)", what, tk, len(got), len(mt.want), firstDiff(got, mt.want))
			}
		}
		closedErr := func(what string, err error) {
			t.Helper()
			if err == nil || !strings.Contains(err.Error(), "handle is closed") {
				t.Fatalf("%s after Close: %v, want handle is closed", what, err)
			}
		}
		submit := func(in []byte, mt modelTicket, refuse string) {
			t.Helper()
			if closed {
				_, err := h.Submit(context.Background(), in)
				closedErr("Submit", err)
				return
			}
			if refuse != "" {
				_, err := h.Submit(context.Background(), in)
				if err == nil || !strings.Contains(err.Error(), refuse) {
					t.Fatalf("Submit %d bytes: %v, want %q", len(in), err, refuse)
				}
				return
			}
			if len(live) >= pool {
				return // every permit held by a live ticket: Submit would park by design
			}
			ctx, cancel := bounded()
			defer cancel()
			tk, err := h.Submit(ctx, in)
			if err != nil {
				// The model says a permit is free or will be once an
				// abandoned job lands: a timeout here is a leaked permit.
				t.Fatalf("Submit refused with %d live, %d abandoned, pool %d: %v (I4 permit leak?)",
					len(live), abandoned, pool, err)
			}
			if _, dup := live[tk]; dup || slices.Contains(spent, tk) {
				t.Fatalf("ticket %d issued twice", tk)
			}
			live[tk] = mt
			order = append(order, tk)
		}

		for i := 0; i+1 < len(ops); i += 2 {
			op, arg := ops[i]%8, ops[i+1]
			switch op {
			case 0: // Submit echo
				n := echoLens[int(arg)%len(echoLens)]
				in := make([]byte, n)
				for j := range in {
					in[j] = byte(j*7) + arg
				}
				if n > 0 {
					in[0] = 0
				}
				refuse := ""
				if n > 4096 {
					refuse = "copy limit"
				}
				submit(in, modelTicket{want: in}, refuse)
			case 1: // Submit mode 16
				n := allocLens[int(arg)%len(allocLens)]
				mt := modelTicket{want: mode16Output(max(n, 0), arg)}
				if n < 0 {
					mt = modelTicket{wantErr: "mode 16 needs a u32 LE length"}
				}
				submit(mode16Input(n, arg), mt, "")
			case 2, 3: // Wait / WaitBuffer on a live ticket
				tk, ok := pick(arg)
				if !ok {
					continue
				}
				ctx, cancel := bounded()
				if closed {
					var err error
					if op == 2 {
						_, err = h.Wait(ctx, tk)
					} else {
						_, err = h.WaitBuffer(ctx, tk)
					}
					cancel()
					closedErr("Wait", err)
					retire(tk)
					continue
				}
				var got []byte
				var err error
				if op == 2 {
					got, err = h.Wait(ctx, tk)
				} else {
					var b *Buffer
					b, err = h.WaitBuffer(ctx, tk)
					if err == nil {
						got = slices.Clone(b.Bytes())
						if ferr := b.Free(); ferr != nil {
							t.Fatalf("Free: %v", ferr)
						}
					}
				}
				timedOut := ctx.Err() != nil
				cancel()
				if timedOut {
					t.Fatalf("Wait on ticket %d did not return within %v", tk, opBound)
				}
				checkResult("Wait", tk, live[tk], got, err)
				retire(tk)
			case 4: // cancel: Wait with a context that is already done
				tk, ok := pick(arg)
				if !ok {
					continue
				}
				got, err := h.Wait(done(), tk)
				if closed {
					closedErr("cancel", err)
				} else if errors.Is(err, context.Canceled) {
					abandoned++
				} else {
					// The completion was already in: a real answer wins.
					checkResult("cancel", tk, live[tk], got, err)
				}
				retire(tk)
			case 5: // Wait on a spent or never-issued ticket
				var tk uint64
				if len(spent) > 0 && arg&1 == 0 {
					tk = spent[int(arg>>1)%len(spent)]
				} else {
					tk = 1<<40 + uint64(arg)
				}
				ctx, cancel := bounded()
				_, err := h.Wait(ctx, tk)
				cancel()
				if closed {
					closedErr("stale Wait", err)
				} else if !errors.Is(err, ErrUnknownTicket) {
					t.Fatalf("Wait on spent/unknown ticket %d: %v, want ErrUnknownTicket", tk, err)
				}
			case 6: // Submit with a done context
				_, err := h.Submit(done(), []byte{0, arg})
				if closed {
					closedErr("Submit(done)", err)
				} else if !errors.Is(err, context.Canceled) {
					t.Fatalf("Submit with a cancelled context: %v", err)
				}
			case 7: // Close (again)
				err := h.Close()
				if closed && !sameErr(err, closeErr) {
					t.Fatalf("second Close: %v, first %v", err, closeErr)
				}
				if !closed && err != nil {
					t.Fatalf("Close: %v", err)
				}
				closed, closeErr = true, err
			}
		}

		// Collect everything still live.
		for _, tk := range slices.Clone(order) {
			ctx, cancel := bounded()
			got, err := h.Wait(ctx, tk)
			cancel()
			if closed {
				closedErr("final Wait", err)
			} else {
				checkResult("final Wait", tk, live[tk], got, err)
			}
			retire(tk)
		}

		// I4: abandoned jobs return their permits when they land.
		if !closed {
			deadline := time.Now().Add(opBound)
			for {
				s.mu.Lock()
				held, semHeld := len(s.semTickets), len(s.sem)
				s.mu.Unlock()
				if held == 0 && semHeld == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("I4: %d tickets / %d permits still held with nothing live (%d abandoned)", held, semHeld, abandoned)
				}
				time.Sleep(100 * time.Microsecond)
			}
			if err := h.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			closed = true
		}

		s.mu.Lock()
		leftovers := len(s.sem) + len(s.semTickets) + len(s.completed) + len(s.takeIDs) + len(s.abandoned)
		for _, ch := range s.pending {
			if ch != nil {
				leftovers++
			}
		}
		s.mu.Unlock()
		if leftovers != 0 {
			t.Fatalf("after Close: sem %d semTickets %d completed %d takeIDs %d abandoned %d pending %v",
				len(s.sem), len(s.semTickets), len(s.completed), len(s.takeIDs), len(s.abandoned), s.pending)
		}
		// I1: every Rust buffer this sequence created has been freed.
		if live := Stats().LiveBytes; live > baseline {
			t.Fatalf("I1: Rust live bytes %d after Close, %d before Open", live, baseline)
		}
	})
}

func sameErr(a, b error) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Error() == b.Error()
}
