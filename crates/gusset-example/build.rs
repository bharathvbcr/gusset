// Same probe as the runtime crate, so the example engine's allocator-aware
// opcode is compiled exactly when `gusset::BufferAlloc` implements `Allocator`.
include!("../gusset/allocator_probe.rs");

fn main() {
    probe_allocator_api();
}
