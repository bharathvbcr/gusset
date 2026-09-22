package pitfalls_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bharathvbcr/gusset"
)

// A job that was already running when a sibling panicked still delivers its
// result, whatever that result's size.
//
// Poison refuses *new* work (I2). gusset_take used to allocate the egress
// buffer for a result of 4 KiB or less through the public, poison-checked
// buf_alloc, so a job that finished successfully after a sibling's panic lost
// its result and surfaced a generic FFI_ERR "handle is poisoned" — neither the
// result nor ErrPoisoned. A result over 4 KiB was already promoted to a buffer
// on the worker and came back intact, so the outcome depended on output size.
func TestPitfall_InFlightResultSurvivesSiblingPanic(t *testing.T) {
	large := make([]byte, 5000)
	large[0], large[1], large[2] = 9, 30, 0 // sleep 300 ms, then echo large[2:]
	for i := 3; i < len(large); i++ {
		large[i] = byte(i)
	}

	cases := []struct {
		name  string
		input []byte
		want  []byte
	}{
		{"small", []byte{9, 30, 0, 1, 2, 3}, []byte{0, 1, 2, 3}},
		{"large", large, large[2:]},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
			if err != nil {
				t.Fatalf("Open failed: %v", err)
			}
			defer h.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			var in any = tc.input
			if len(tc.input) > 4096 {
				buf, err := h.NewBuffer(len(tc.input))
				if err != nil {
					t.Fatalf("NewBuffer: %v", err)
				}
				defer buf.Free()
				copy(buf.Bytes(), tc.input)
				in = buf
			}

			slow, err := h.Submit(ctx, in)
			if err != nil {
				t.Fatalf("Submit slow job: %v", err)
			}
			boom, err := h.Submit(ctx, []byte{9, 5, 1}) // panic after 50 ms
			if err != nil {
				t.Fatalf("Submit panicking job: %v", err)
			}

			if _, err := h.Wait(ctx, boom); !errors.Is(err, gusset.ErrPanic) {
				t.Fatalf("sibling: expected ErrPanic, got %v", err)
			}

			got, err := h.Wait(ctx, slow)
			if err != nil {
				t.Fatalf("in-flight job finished after the sibling's panic and lost its result: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("result corrupted: got %d bytes, want %d", len(got), len(tc.want))
			}

			// Poison still refuses new work (I2).
			if _, err := h.Call(ctx, []byte{0}); !errors.Is(err, gusset.ErrPoisoned) {
				t.Fatalf("new call after panic: expected ErrPoisoned, got %v", err)
			}
		})
	}
}
