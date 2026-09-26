package gusset

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
)

// BenchmarkCompletionTransport compares the shared-memory completion ring
// with the pipe on identical work, in one binary: the only difference between
// the arms is withPipeOnly. Run:
//
//	go test -run '^$' -bench CompletionTransport -count=6 .
func BenchmarkCompletionTransport(b *testing.B) {
	spin := func(iters uint32) []byte {
		in := []byte{11, 0, 0, 0, 0}
		binary.LittleEndian.PutUint32(in[1:], iters)
		return in
	}
	works := []struct {
		name string
		in   []byte
	}{
		{"noop", []byte{0}},
		// Mode 11's loop: about 10 us on the recording host.
		{"spin10us", spin(10_000)},
		{"spin100us", spin(100_000)},
	}
	for _, mode := range []struct {
		name string
		opts []Option
	}{
		{"ring", nil},
		{"pipe", []Option{withPipeOnly()}},
	} {
		h, err := Open(append([]Option{WithPoolSize(4), WithDiagnosticEngine()}, mode.opts...)...)
		if err != nil {
			b.Fatal(err)
		}
		ctx := context.Background()
		// 64 KiB in a Buffer, echoed: a stored result collected with take.
		b.Run("buf64k/serial/"+mode.name, func(b *testing.B) {
			buf, err := h.NewBuffer(64 << 10)
			if err != nil {
				b.Fatal(err)
			}
			defer buf.Free()
			data := buf.Bytes()
			for i := range data {
				data[i] = byte(i)
			}
			data[0] = 0
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				t, err := h.Submit(ctx, buf)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := h.Wait(ctx, t); err != nil {
					b.Fatal(err)
				}
			}
		})
		for _, w := range works {
			b.Run(fmt.Sprintf("%s/serial/%s", w.name, mode.name), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, err := h.Call(ctx, w.in); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(fmt.Sprintf("%s/parallel/%s", w.name, mode.name), func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						if _, err := h.Call(ctx, w.in); err != nil {
							b.Error(err)
							return
						}
					}
				})
			})
		}
		_ = h.Close()
	}
}
