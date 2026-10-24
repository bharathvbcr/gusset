package pitfalls_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bharathvbcr/gusset"
)

// TestPitfall_OpenNilOptionIsRefusedNotPanicked pins the nil-Option refusal.
//
// Open takes ...Option and Option is a func type, so Open(nil) compiles.
// handle.go called every Option unchecked, and a nil one panicked with a nil
// dereference before anything was allocated. It now returns an error matching
// ErrBadArg: not a skip, because an Option the caller meant to apply (a failed
// constructor's zero value) would then be dropped without a word.
func TestPitfall_OpenNilOptionIsRefusedNotPanicked(t *testing.T) {
	var zero gusset.Option
	cases := []struct {
		name string
		opts []gusset.Option
	}{
		{"Open(nil)", []gusset.Option{nil}},
		{"Open(WithPoolSize(2), nil)", []gusset.Option{gusset.WithPoolSize(2), nil}},
		{"Open(nil, WithPoolSize(2))", []gusset.Option{nil, gusset.WithPoolSize(2)}},
		{"a zero Option variable", []gusset.Option{zero}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				h   *gusset.Handle
				err error
			)
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("Open panicked on a nil Option: %v", r)
					}
				}()
				h, err = gusset.Open(tc.opts...)
			}()
			if err == nil {
				_ = h.Close()
				t.Fatal("Open accepted a nil Option")
			}
			if h != nil {
				t.Fatalf("a refused Open returned a handle alongside %v", err)
			}
			if !errors.Is(err, gusset.ErrBadArg) {
				t.Fatalf("a nil Option must match ErrBadArg, got %v", err)
			}
			if !strings.Contains(err.Error(), "nil Option") {
				t.Fatalf("the error does not name the problem: %v", err)
			}
		})
	}

	// The refusal is about the nil, not about Open: the same options without
	// it open.
	h, err := gusset.Open(gusset.WithPoolSize(2), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open without a nil Option: %v", err)
	}
	_ = h.Close()
}
