package cgoprobe

/*
#include <stdlib.h>
*/
import "C"

import "unsafe"

// FreeWithLibcFree releases a pointer with the C allocator.
//
// This is R4's forbidden operation, isolated in one named function so the rule has
// exactly one place it can be violated from. Gusset's buffers come from Rust's
// allocator via `std::alloc::alloc` with 64-byte alignment; handing one of those
// pointers to `free` is an allocator mismatch, and the resulting heap corruption is
// silent until something unrelated crashes.
//
// Nothing in the library calls this. Its only caller is the ASan negative test,
// which asserts that the mismatch is *detected* — the one place where committing
// the violation is the point. `gussetvet` allows `C.free` here and forbids it
// everywhere else.
func FreeWithLibcFree(p unsafe.Pointer) {
	C.free(p)
}
