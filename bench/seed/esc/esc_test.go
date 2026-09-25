package esc

import "testing"

func BenchmarkPlain(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Plain(i)
	}
}
func BenchmarkNoEscape(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		NoEscape(i)
	}
}
