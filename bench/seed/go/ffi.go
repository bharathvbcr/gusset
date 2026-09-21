package ffibench

/*
#cgo LDFLAGS: -L${SRCDIR}/../rs/target/release -lffibench -lm -ldl -lpthread
#cgo noescape rs_noop
#cgo nocallback rs_noop
#cgo noescape rs_batch
#cgo nocallback rs_batch
#cgo noescape rs_spin
#cgo nocallback rs_spin
#include <stdint.h>
#include <stddef.h>
typedef struct { int32_t code; char* error_msg; } FfiStatus;
uint64_t rs_noop(uint64_t x);
uint64_t rs_batch(uint64_t* p, size_t n);
uint64_t rs_spin(uint64_t iters);
uint64_t rs_guarded_doc(int32_t mode, FfiStatus* st);
void rs_free_string(char* p);
static inline uint64_t c_noop(uint64_t x) { return x + 1; }
*/
import "C"
import (
	"errors"
	"unsafe"
)

func Noop(x uint64) uint64 { return uint64(C.rs_noop(C.uint64_t(x))) }

// Spin is the raw-cgo half of the crossover measurement: a single blocking cgo
// call into a fixed-cost Rust loop. Gusset diagnostic mode 11 runs the identical
// loop, so the difference between the two is transport and nothing else.
func Spin(iters uint64) uint64 { return uint64(C.rs_spin(C.uint64_t(iters))) }
func CNoop(x uint64) uint64    { return uint64(C.c_noop(C.uint64_t(x))) }
func Batch(s []uint64) uint64 {
	return uint64(C.rs_batch((*C.uint64_t)(unsafe.Pointer(&s[0])), C.size_t(len(s))))
}
func Guarded(mode int) (uint64, error) {
	var st C.FfiStatus
	v := C.rs_guarded_doc(C.int32_t(mode), &st)
	if st.code != 0 {
		msg := C.GoString(st.error_msg)
		C.rs_free_string(st.error_msg)
		return 0, errors.New(msg)
	}
	return uint64(v), nil
}
