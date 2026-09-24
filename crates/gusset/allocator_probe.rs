// Shared by the build scripts of `gusset` and `gusset-example` via `include!`.
//
// Detects whether the compiler in use accepts `std::alloc::Allocator` without a
// feature gate (stable from Rust 1.100) and, if so, emits `cfg(gusset_allocator_api)`.
//
// A probe rather than a version comparison: a nightly that reports 1.100 but
// predates the stabilization, or a vendor toolchain with its own numbering, gets
// the answer the compiler actually gives. The probe never adds `#![feature]`, so a
// nightly cannot turn the gate on by accident — `rust-version` stays 1.97 and the
// allocator-aware surface appears only where it is stable.
//
// `GUSSET_ALLOCATOR_API=0` forces the fallback on a new toolchain (CI uses it to
// keep the pre-1.100 path compiled and tested); `=1` makes a failed probe a hard
// build error instead of a silent fallback, for jobs that must exercise it.

fn probe_allocator_api() {
    use std::env;
    use std::path::PathBuf;
    use std::process::Command;

    println!("cargo::rustc-check-cfg=cfg(gusset_allocator_api)");
    println!("cargo::rerun-if-env-changed=GUSSET_ALLOCATOR_API");
    println!("cargo::rerun-if-env-changed=RUSTC");

    let forced = env::var("GUSSET_ALLOCATOR_API").ok();
    if forced.as_deref() == Some("0") {
        return;
    }

    let out_dir = match env::var_os("OUT_DIR") {
        Some(d) => PathBuf::from(d),
        None => return,
    };
    let rustc = env::var_os("RUSTC").unwrap_or_else(|| "rustc".into());
    let src = out_dir.join("gusset_allocator_probe.rs");
    // Exercises every item the gated code uses, so a toolchain that stabilized
    // the trait but not (say) `Box::into_raw_with_allocator` falls back instead
    // of failing to build the crate.
    let code = r#"
        #![no_std]
        extern crate alloc;
        use core::alloc::{Allocator, AllocError, Layout};
        use core::ptr::NonNull;
        use alloc::alloc::Global;
        use alloc::boxed::Box;
        use alloc::vec::Vec;
        #[derive(Clone, Copy)]
        pub struct P;
        unsafe impl Allocator for P {
            fn allocate(&self, l: Layout) -> Result<NonNull<[u8]>, AllocError> { Global.allocate(l) }
            fn allocate_zeroed(&self, l: Layout) -> Result<NonNull<[u8]>, AllocError> { Global.allocate_zeroed(l) }
            unsafe fn deallocate(&self, p: NonNull<u8>, l: Layout) { unsafe { Global.deallocate(p, l) } }
            unsafe fn grow(&self, p: NonNull<u8>, o: Layout, n: Layout) -> Result<NonNull<[u8]>, AllocError> { unsafe { Global.grow(p, o, n) } }
            unsafe fn grow_zeroed(&self, p: NonNull<u8>, o: Layout, n: Layout) -> Result<NonNull<[u8]>, AllocError> { unsafe { Global.grow_zeroed(p, o, n) } }
            unsafe fn shrink(&self, p: NonNull<u8>, o: Layout, n: Layout) -> Result<NonNull<[u8]>, AllocError> { unsafe { Global.shrink(p, o, n) } }
        }
        pub fn f() -> usize {
            let mut v: Vec<u8, P> = Vec::with_capacity_in(8, P);
            let _ = v.try_reserve(8);
            let b: Box<u8, P> = Box::new_in(1, P);
            let (raw, a) = Box::into_raw_with_allocator(b);
            let b = unsafe { Box::from_raw_in(raw, a) };
            v.len() + *b as usize + Vec::<u8, P>::new_in(P).capacity()
        }
    "#;
    if std::fs::write(&src, code).is_err() {
        return;
    }

    let mut cmd = Command::new(rustc);
    cmd.arg("--crate-type=lib")
        .arg("--edition=2021")
        .arg("--emit=metadata")
        .arg("--crate-name=gusset_allocator_probe")
        .arg("--out-dir")
        .arg(&out_dir)
        .arg("--cap-lints=allow");
    if let Ok(target) = env::var("TARGET") {
        cmd.arg("--target").arg(target);
    }
    cmd.arg(&src);

    let ok = cmd
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false);
    if ok {
        println!("cargo::rustc-cfg=gusset_allocator_api");
    } else if forced.as_deref() == Some("1") {
        panic!(
            "GUSSET_ALLOCATOR_API=1 but this rustc does not accept a stable \
             std::alloc::Allocator (Rust 1.100 or newer is required)"
        );
    }
}
