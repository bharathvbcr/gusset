package bench_test

import (
	"context"
	"testing"

	"github.com/bharathvbcr/gusset"
)

func BenchmarkGussetCallNoop(b *testing.B) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx := context.Background()
	payload := []byte{0}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		res, err := h.Call(ctx, payload)
		if err != nil {
			b.Fatalf("Call failed: %v", err)
		}
		_ = res
	}
}

func BenchmarkGussetCallParallel(b *testing.B) {
	h, err := gusset.Open(gusset.WithPoolSize(8), gusset.WithDiagnosticEngine())
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx := context.Background()
	payload := []byte{0}

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, err := h.Call(ctx, payload)
			if err != nil {
				b.Errorf("Call failed: %v", err)
				return
			}
			_ = res
		}
	})
}

func BenchmarkGussetSubmitWait(b *testing.B) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	ctx := context.Background()
	payload := []byte{0}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		ticket, err := h.Submit(ctx, payload)
		if err != nil {
			b.Fatalf("Submit failed: %v", err)
		}
		res, err := h.Wait(ctx, ticket)
		if err != nil {
			b.Fatalf("Wait failed: %v", err)
		}
		_ = res
	}
}

func BenchmarkGussetBufferLarge(b *testing.B) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const bufSize = 64 * 1024
	buf, err := h.NewBuffer(bufSize)
	if err != nil {
		b.Fatalf("NewBuffer failed: %v", err)
	}
	defer buf.Free()
	// Fill explicitly. The diagnostic engine dispatches on input[0], and this
	// benchmark never wrote the buffer, so every iteration ran whatever mode
	// the allocator's leftover byte selected — panics, and mode 16 asked for
	// multi-GiB outputs — and runs failed or measured the wrong work. Mode 0
	// echoes: a 64 KiB request and a 64 KiB result, as the name says.
	data := buf.Bytes()
	for i := range data {
		data[i] = byte(i)
	}
	data[0] = 0

	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		ticket, err := h.Submit(ctx, buf)
		if err != nil {
			b.Fatalf("Submit failed: %v", err)
		}
		res, err := h.Wait(ctx, ticket)
		if err != nil {
			b.Fatalf("Wait failed: %v", err)
		}
		_ = res
	}
}

func BenchmarkGussetBufferLargeZeroCopy(b *testing.B) {
	h, err := gusset.Open(gusset.WithPoolSize(4), gusset.WithDiagnosticEngine())
	if err != nil {
		b.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	const bufSize = 64 * 1024
	buf, err := h.NewBuffer(bufSize)
	if err != nil {
		b.Fatalf("NewBuffer failed: %v", err)
	}
	defer buf.Free()
	// Fill explicitly. The diagnostic engine dispatches on input[0], and this
	// benchmark never wrote the buffer, so every iteration ran whatever mode
	// the allocator's leftover byte selected — panics, and mode 16 asked for
	// multi-GiB outputs — and runs failed or measured the wrong work. Mode 0
	// echoes: a 64 KiB request and a 64 KiB result, as the name says.
	data := buf.Bytes()
	for i := range data {
		data[i] = byte(i)
	}
	data[0] = 0

	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		ticket, err := h.Submit(ctx, buf)
		if err != nil {
			b.Fatalf("Submit failed: %v", err)
		}
		outBuf, err := h.WaitBuffer(ctx, ticket)
		if err != nil {
			b.Fatalf("WaitBuffer failed: %v", err)
		}
		_ = outBuf.Bytes()
		_ = outBuf.Free()
	}
}

func BenchmarkChannelHop(b *testing.B) {
	ch := make(chan uint64, 256)
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		ch <- uint64(i)
	}
	close(ch)
	<-done
}
