//! `BufferAlloc` and `Counting`-as-`Allocator` under an installed `Counting`
//! global allocator: every byte must be counted exactly once.
//!
//! With `Counting` as the `#[global_allocator]`, `BufferAlloc` (which routes to
//! `Global`) must leave its manual recorders idle, and a local `Counting<Global>`
//! must not count what the global wrapper already counted. Double counting shows
//! up in `AdviseMemoryLimit` as Rust memory subtracted twice from the Go budget.
//!
//! Separate binary because it installs the global allocator. The harness and
//! test threads allocate too, so sizes are large and bounds carry slack.

#[cfg(gusset_allocator_api)]
#[test]
fn every_byte_is_counted_exactly_once() {
    use gusset::{get_alloc_stats, BufferAlloc, Counting};
    use std::alloc::{Global, System};

    #[global_allocator]
    static ALLOC: Counting<System> = Counting::new(System);

    const MIB: usize = 1 << 20;
    const SLACK: usize = 256 * 1024;
    let live = || get_alloc_stats().live_bytes as isize;

    let check = |what: &str, delta: isize, want: usize| {
        let want = want as isize;
        assert!(
            (delta - want).unsigned_abs() <= SLACK,
            "{what}: live moved by {delta}, expected ~{want} (2x means double counting)"
        );
    };

    let before = live();
    let mut v: Vec<u8, BufferAlloc> = Vec::with_capacity_in(8 * MIB, BufferAlloc);
    v.resize(8 * MIB, 1);
    check("BufferAlloc", live() - before, 8 * MIB);
    drop(v);
    check("BufferAlloc freed", live() - before, 0);

    let before = live();
    let mut v: Vec<u8, Counting<Global>> = Vec::new_in(Counting::new(Global));
    v.resize(8 * MIB, 2);
    v.shrink_to_fit();
    check("Counting<Global>", live() - before, 8 * MIB);
    drop(v);
    check("Counting<Global> freed", live() - before, 0);

    let before = live();
    let mut v: Vec<u8, Counting<System>> = Vec::new_in(Counting::new(System));
    v.resize(8 * MIB, 3);
    v.shrink_to_fit();
    check("Counting<System>", live() - before, 8 * MIB);
    drop(v);
    check("Counting<System> freed", live() - before, 0);
}
