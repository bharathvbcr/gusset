// Package cgoprobe commits deliberate cgo pointer violations so the test suite can
// verify that GOEXPERIMENT=cgocheck2 is actually armed.
//
// It exists as its own package because Go does not permit cgo in `_test.go` files,
// and it is deliberately tiny: it links no Gusset symbol and depends on nothing, so
// it cannot mask a real failure in the runtime it is used to test.
//
// Nothing in the shipping library imports this. It is reachable only from
// `tests/pitfalls`.
package cgoprobe

/*
#include <stdlib.h>

// Returns a heap slot in C memory, big enough to hold one pointer. The probe
// stores a Go pointer into it, which is the violation R16 forbids: Rust must
// never retain Go memory.
//
// No `#cgo noescape` directive in this preamble, on purpose. R5 requires those on
// the real Gusset exports and `gussetvet` enforces it for any preamble carrying a
// `#cgo` line; adding one here would drag this file into that check and change the
// escape analysis the probe is meant to leave alone.
static void** gusset_cgocheck_slot(void) { return (void**)malloc(sizeof(void*)); }
static void gusset_cgocheck_free(void** p) { free(p); }
*/
import "C"

import (
	"runtime"
	"unsafe"
)

// StoreGoPointerIntoCMemory commits the violation that *only* cgocheck2 detects.
//
// The obvious probe — passing C a Go pointer whose pointee holds another Go
// pointer — does not work here: baseline cgo checking is on in every build and
// catches it, so it fires with and without the experiment and says nothing about
// which is in effect.
//
// Baseline cgo checking inspects pointers passed as call arguments. It does not
// inspect ordinary writes, so storing a Go pointer into C-allocated memory passes
// unnoticed — and that store is exactly R16's failure mode: Rust keeping a Go
// pointer past the call, after which the Go collector is free to move or reclaim
// the object underneath it.
//
// Under GOEXPERIMENT=cgocheck2 the write barrier throws
//
//	fatal error: Go pointer stored into non-Go memory
//
// Without the experiment this returns normally, which is what makes it a usable
// probe for whether the experiment is on.
func StoreGoPointerIntoCMemory() {
	slot := C.gusset_cgocheck_slot()
	if slot == nil {
		panic("cgoprobe: malloc failed")
	}
	defer C.gusset_cgocheck_free(slot)

	v := new(int)
	*v = 42

	// The write barrier on this store is what cgocheck2 instruments.
	dst := (*unsafe.Pointer)(unsafe.Pointer(slot))
	*dst = unsafe.Pointer(v)

	runtime.KeepAlive(v)
}
