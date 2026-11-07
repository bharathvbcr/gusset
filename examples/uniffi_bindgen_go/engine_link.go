//go:build gusset_uniffi_engine

package uniffi_bindgen_go

/*
// Built only with -tags gusset_uniffi_engine, and linked only against the
// umbrella archive rust/ builds (Gusset plus this engine, R14): `make
// test-uniffi-link` puts its directory first in CGO_LDFLAGS, ahead of
// internal/ffi's own -L, so -lgusset resolves to it. Gusset's own
// target/release/libgusset.a has no init_uniffi_engine, so a link that picked
// it fails with an undefined symbol rather than running without the engine.
#cgo noescape init_uniffi_engine
#cgo nocallback init_uniffi_engine
#include <stdint.h>
int32_t init_uniffi_engine(uint32_t opcode);
*/
import "C"

import "fmt"

// RegisterCompiledEngine registers the compiled Rust engine with Gusset under
// opcode. Open the handle that reaches it with gusset.WithOpcode(opcode). Call
// it once per process: the registry is process-global, and a second call for
// the same opcode is refused.
func RegisterCompiledEngine(opcode uint32) error {
	if rc := C.init_uniffi_engine(C.uint32_t(opcode)); rc != 0 {
		return fmt.Errorf("uniffi: init_uniffi_engine(%d) returned %d: opcode 0, or opcode already registered", opcode, int32(rc))
	}
	return nil
}
