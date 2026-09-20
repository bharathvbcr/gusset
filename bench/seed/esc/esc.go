package esc

/*
#cgo noescape take_ne
#cgo nocallback take_ne
#include <stdint.h>
static inline uint64_t take(uint64_t* p, size_t n) { return p[0] + n; }
static inline uint64_t take_ne(uint64_t* p, size_t n) { return p[0] + n; }
*/
import "C"
import "unsafe"

func Plain(n int) uint64 {
	var buf [8]uint64
	return uint64(C.take((*C.uint64_t)(unsafe.Pointer(&buf[0])), C.size_t(n)))
}
func NoEscape(n int) uint64 {
	var buf [8]uint64
	return uint64(C.take_ne((*C.uint64_t)(unsafe.Pointer(&buf[0])), C.size_t(n)))
}
