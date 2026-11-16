// The runtime crate's allocator-API build hook (`include!`d by `build.rs`).
//
// The stable `std::alloc::Allocator` trait is guaranteed by `rust-version = "1.100"`,
// so there is no longer a probe, no `cfg(gusset_allocator_api)` and no fallback: the
// allocator-aware surface (`BufferAlloc`, `Counting<A>` as an `Allocator`,
// `JobOutput::Allocated`) is always compiled in.
//
// What remains is the published build metadata. `gusset` declares `links = "gusset"`,
// so a dependent's `build.rs` can still read `DEP_GUSSET_ALLOCATOR_API`; it is always
// `1`. Dependents that gated code on it keep compiling and keep their code enabled;
// new dependents need no gate at all.
//
// This file keeps its name and entry point only because `build.rs` includes it by
// that name; once `build.rs` is next edited, inline this function and delete the file.

fn probe_allocator_api() {
    println!("cargo::metadata=allocator_api=1");
}
