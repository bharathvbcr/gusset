package ffibench

import "testing"

func BenchmarkCgoRustNoop(b *testing.B) {
	var acc uint64
	for i := 0; i < b.N; i++ { acc += Noop(uint64(i)) }
	_ = acc
}
func BenchmarkCgoCNoop(b *testing.B) {
	var acc uint64
	for i := 0; i < b.N; i++ { acc += CNoop(uint64(i)) }
	_ = acc
}
func BenchmarkCgoNoopParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		var acc uint64
		for pb.Next() { acc += Noop(1) }
		_ = acc
	})
}
func benchBatch(b *testing.B, n int) {
	s := make([]uint64, n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ { Batch(s) }
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(n), "ns/item")
}
func BenchmarkBatch16(b *testing.B)   { benchBatch(b, 16) }
func BenchmarkBatch256(b *testing.B)  { benchBatch(b, 256) }
func BenchmarkBatch4096(b *testing.B) { benchBatch(b, 4096) }

// What it costs to *form* a batch by funnelling items through a channel to a batcher goroutine.
func BenchmarkChannelHop(b *testing.B) {
	ch := make(chan uint64, 256)
	done := make(chan struct{})
	go func() { for range ch {}; close(done) }()
	for i := 0; i < b.N; i++ { ch <- uint64(i) }
	close(ch); <-done
}
func BenchmarkUnbufferedRoundTrip(b *testing.B) {
	req, resp := make(chan uint64), make(chan uint64)
	go func() { for v := range req { resp <- v + 1 } }()
	for i := 0; i < b.N; i++ { req <- 1; <-resp }
	close(req)
}
func BenchmarkPureGoLoopItem(b *testing.B) {
	s := make([]uint64, 4096)
	for i := 0; i < b.N; i++ { for j := range s { s[j] = s[j]*s[j] + 1 } }
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/4096, "ns/item")
}
func TestGuardedOK(t *testing.T) {
	v, err := Guarded(0); if err != nil || v != 42 { t.Fatal(v, err) }
	_, err = Guarded(1); if err == nil { t.Fatal("expected error") }
	t.Log("caught:", err)
}
