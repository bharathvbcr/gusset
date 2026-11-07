package uniffi_bindgen_go

import (
	"context"
	"errors"
	"math"
	"strconv"
	"testing"
	"time"
)

// A 2 GiB buffer is never allocated here: the length check is a function of an
// int, so the boundary is tested on the number.
func TestLengthPrefix_Boundary(t *testing.T) {
	for _, n := range []int{0, 1, 1 << 20, math.MaxInt32} {
		got, err := lengthPrefix(n)
		if err != nil || int(got) != n {
			t.Fatalf("lengthPrefix(%d) = %d, %v; want %d, nil", n, got, err, n)
		}
	}
	if strconv.IntSize < 64 {
		t.Skip("an int cannot exceed MaxInt32 on this platform")
	}
	for _, n := range []int{math.MaxInt32 + 1, math.MaxInt32 + 2, 1 << 40} {
		if _, err := lengthPrefix(n); !errors.Is(err, ErrStringTooLong) {
			t.Fatalf("lengthPrefix(%d) error = %v; want ErrStringTooLong", n, err)
		}
	}
}

func TestWriter_SmallStringAndBytesUseThePrefixPath(t *testing.T) {
	w := NewWriter(16)
	if err := w.WriteString("hi"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := w.WriteBytes([]byte{9}); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	want := []byte{0, 0, 0, 2, 'h', 'i', 0, 0, 0, 1, 9}
	if string(w.Bytes()) != string(want) {
		t.Fatalf("bytes = %v, want %v", w.Bytes(), want)
	}
}

func TestDelayMillis_CannotWrap(t *testing.T) {
	maxDelay := time.Duration(math.MaxInt32) * time.Millisecond
	ok := map[time.Duration]int32{
		0:                       0,
		-5 * time.Second:        0, // the engine treats a negative delay as none
		999 * time.Microsecond:  0,
		1500 * time.Millisecond: 1500,
		maxDelay:                math.MaxInt32,
	}
	for d, want := range ok {
		if got, err := delayMillis(d); err != nil || got != want {
			t.Fatalf("delayMillis(%v) = %d, %v; want %d, nil", d, got, err, want)
		}
	}
	// 2^31 ms wrapped to math.MinInt32 under int32(d.Milliseconds()).
	for _, d := range []time.Duration{maxDelay + time.Millisecond, math.MaxInt64} {
		if got, err := delayMillis(d); !errors.Is(err, ErrDelayOutOfRange) {
			t.Fatalf("delayMillis(%v) = %d, %v; want ErrDelayOutOfRange", d, got, err)
		}
	}
}

type neverCalledTransport struct{ calls int }

func (n *neverCalledTransport) Call(context.Context, []byte) ([]byte, error) {
	n.calls++
	return nil, errors.New("must not be called")
}
func (n *neverCalledTransport) Submit(context.Context, []byte) (uint64, error) { return 0, nil }
func (n *neverCalledTransport) Wait(context.Context, uint64) ([]byte, error)   { return nil, nil }
func (n *neverCalledTransport) Close() error                                   { return nil }

func TestExecuteSlowJob_RefusesAWrappingDelayBeforeSending(t *testing.T) {
	tr := &neverCalledTransport{}
	_, err := NewClient(tr).ExecuteSlowJob(context.Background(), time.Duration(math.MaxInt32+1)*time.Millisecond)
	if !errors.Is(err, ErrDelayOutOfRange) {
		t.Fatalf("err = %v, want ErrDelayOutOfRange", err)
	}
	if tr.calls != 0 {
		t.Fatalf("transport was called %d times for a refused request", tr.calls)
	}
}
