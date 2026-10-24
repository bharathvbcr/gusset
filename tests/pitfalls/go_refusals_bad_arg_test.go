package pitfalls_test

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/bharathvbcr/gusset"
)

// TestPitfall_GoSideArgumentRefusalsAreBadArg pins the class of the refusals Go
// makes itself.
//
// The same caller mistake used to have two classes. A pool size over the
// ceiling or a buffer length of zero reached through the C ABI returned
// FFI_BAD_ARG, and the identical value passed through WithPoolSize or NewBuffer
// was refused by Go with a bare fmt.Errorf that matched nothing: a caller who
// wrote errors.Is(err, gusset.ErrBadArg) met the refusal only if the mistake was
// a nil Option. Each of the five Go-side refusals now matches ErrBadArg, is not
// also ErrGeneric, and keeps the text that names the problem (the substring
// tests TestPitfall_PoolSizeRefusalNamesTheActualProblem and
// TestPitfall_NegativeBufferBudgetIsRefused pin the wording).
func TestPitfall_GoSideArgumentRefusalsAreBadArg(t *testing.T) {
	check := func(t *testing.T, what string, err error, msg string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: accepted", what)
		}
		if !errors.Is(err, gusset.ErrBadArg) {
			t.Fatalf("%s: want errors.Is(err, ErrBadArg), got %T %v", what, err, err)
		}
		if errors.Is(err, gusset.ErrGeneric) {
			t.Fatalf("%s: matches ErrGeneric as well as ErrBadArg: %v", what, err)
		}
		var ge *gusset.Error
		if !errors.As(err, &ge) {
			t.Fatalf("%s: want an *Error, got %T", what, err)
		}
		if want := fmt.Sprintf("gusset error [%d]: ", gusset.ErrBadArg.Code); !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("%s: %q does not start with %q", what, err, want)
		}
		if !strings.Contains(err.Error(), msg) {
			t.Fatalf("%s: %q does not say %q", what, err, msg)
		}
	}

	t.Run("WithPoolSize below one", func(t *testing.T) {
		for _, n := range []int{0, -1} {
			h, err := gusset.Open(gusset.WithPoolSize(n), gusset.WithDiagnosticEngine())
			if h != nil {
				_ = h.Close()
			}
			check(t, fmt.Sprintf("WithPoolSize(%d)", n), err, "pool_size must be at least 1")
		}
	})

	t.Run("WithPoolSize over the ceiling", func(t *testing.T) {
		for _, n := range []int{gusset.MaxPoolSize + 1, 1 << 40} {
			h, err := gusset.Open(gusset.WithPoolSize(n), gusset.WithDiagnosticEngine())
			if h != nil {
				_ = h.Close()
			}
			check(t, fmt.Sprintf("WithPoolSize(%d)", n), err, "exceeds maximum")
		}
	})

	t.Run("WithBufferBudget negative", func(t *testing.T) {
		for _, n := range []int64{-1, math.MinInt64} {
			h, err := gusset.Open(gusset.WithBufferBudget(n), gusset.WithDiagnosticEngine())
			if h != nil {
				_ = h.Close()
			}
			check(t, fmt.Sprintf("WithBufferBudget(%d)", n), err, "buffer budget must not be negative")
		}
	})

	h, err := gusset.Open(gusset.WithPoolSize(1), gusset.WithDiagnosticEngine())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	t.Run("NewBuffer of zero or less", func(t *testing.T) {
		for _, n := range []int{0, -1} {
			b, err := h.NewBuffer(n)
			if b != nil {
				_ = b.Free()
			}
			check(t, fmt.Sprintf("NewBuffer(%d)", n), err, "buffer size must be greater than zero")
		}
	})

	t.Run("NewBuffer over the ceiling", func(t *testing.T) {
		b, err := h.NewBuffer(gusset.MaxBufferBytes + 1)
		if b != nil {
			_ = b.Free()
		}
		check(t, "NewBuffer(MaxBufferBytes+1)", err, "exceeds maximum")
	})

	// The refusal is about the argument, not the handle: it neither poisons nor
	// closes it, and the next valid allocation works.
	b, err := h.NewBuffer(64)
	if err != nil {
		t.Fatalf("a valid NewBuffer after the refusals: %v", err)
	}
	_ = b.Free()
}
