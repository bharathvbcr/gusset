//! A panic caught by the gusset firewall must return Gusset-owned buffer bytes.
//!
//! Every case runs inside `ffi_guard_code`, the firewall each `gusset_*` export
//! sits behind, with a handle opened through `gusset_handle_open`, and reads the
//! outcome from the `FfiStatus` it fills (`FFI_PANIC` and the panic message).
//!
//! * A buffer owned by the guarded closure (`RawBuffer`, and on Rust 1.100+ a
//!   `Vec` in `BufferAlloc`) is returned by unwinding: `Drop` runs inside the
//!   firewall's `catch_unwind`, so the live counter is back at its baseline
//!   once `ffi_guard_code` returns. A closure that forgets such a buffer leaks it.
//! * A buffer registered in the handle by `gusset_buf_alloc` is the real I1
//!   shape. Unwinding must not free it (the handle owns it, and a double free
//!   would be worse than a leak), and `gusset_buf_free` or `gusset_handle_close`
//!   must still return it afterwards.
//!
//! Separate binary: the counters are process-global. Another test in the same
//! process would move the baseline this measures. No `Counting` global allocator
//! is installed, so the live counter moves only with Gusset's own buffers. A
//! plain `Vec<u8>` is not counted here and is not asserted on.

#![allow(unsafe_code)]

use std::ptr;

use gusset::ffi::alloc::counting_is_active;
use gusset::ffi::guard::{ffi_guard_code, FfiError};
use gusset::ffi::gusset_status_free;
use gusset::ffi::status::{FFI_OK, FFI_PANIC};
use gusset::ffi::{gusset_buf_alloc, gusset_buf_free, gusset_handle_close, gusset_handle_open};
use gusset::pool::sys::RawBuffer;
use gusset::{get_alloc_stats, FfiStatus, Handle};

#[path = "common/mod.rs"]
mod common;
use common::make_pipe;

const N: usize = 1 << 20;

fn live() -> usize {
    get_alloc_stats().live_bytes
}

/// Runs `body` behind `ffi_guard_code`, which must catch the panic `body` raises
/// with `marker`, and checks what the firewall reported: the `FFI_PANIC` return
/// code, the same code and the intentional message in the status. Silences the
/// panic this test induces on purpose, and frees the status message.
fn guarded_panic(marker: &str, body: impl FnOnce() -> Result<(), FfiError>) {
    let previous = std::panic::take_hook();
    std::panic::set_hook(Box::new(|_| {}));
    let mut status = FfiStatus::ok();
    let outcome = unsafe { ffi_guard_code(&mut status, body) };
    std::panic::set_hook(previous);

    assert_eq!(
        outcome,
        Err(FFI_PANIC),
        "the firewall must turn the panic into FFI_PANIC"
    );
    assert_eq!(status.code, FFI_PANIC, "the status must carry FFI_PANIC");
    assert!(
        !status.msg.is_null(),
        "a panic status must carry its message"
    );
    let text = String::from_utf8_lossy(unsafe {
        std::slice::from_raw_parts(status.msg as *const u8, status.msg_len)
    })
    .into_owned();
    assert!(
        text.contains(marker),
        "panic must be the intentional unwind ({marker}), not an earlier failure: {text}"
    );
    unsafe { gusset_status_free(&mut status) };
}

/// Opens a handle on a fresh pipe; returns it with the read end to close.
fn open_handle() -> (*mut Handle, i32) {
    let (r, w) = make_pipe();
    let mut handle: *mut Handle = ptr::null_mut();
    let code = unsafe { gusset_handle_open(1, w, &mut handle, ptr::null_mut()) };
    assert_eq!(code, FFI_OK, "gusset_handle_open");
    assert!(!handle.is_null());
    (handle, r)
}

/// A buffer the guarded closure owns is reclaimed by the unwind the firewall catches.
fn reclaim_raw_buffer_in_guard(handle: *mut Handle) {
    let before = live();
    guarded_panic("unwind-reclaim-raw", || {
        let buf = match RawBuffer::allocate(N) {
            Ok(buf) => buf,
            Err(e) => panic!("allocate: {e}"),
        };
        assert_eq!(buf.len(), N, "the buffer must stay live until the panic");
        assert_eq!(
            live(),
            before + N,
            "RawBuffer must be counted before the panic"
        );
        assert!(!handle.is_null(), "the handle stays open across the panic");
        panic!("unwind-reclaim-raw");
    });
    assert_eq!(
        live(),
        before,
        "once the firewall has caught the panic, live bytes must be back at the baseline"
    );
}

/// A buffer registered in the handle survives the unwind, then is returned.
fn reclaim_registered_buffer_after_guarded_panic(handle: *mut Handle, free_it: bool) {
    let before = live();
    let mut id = 0u64;
    let mut held = 0usize;
    guarded_panic("unwind-reclaim-registered", || {
        let mut p: *mut u8 = ptr::null_mut();
        let code = unsafe { gusset_buf_alloc(handle, N, &mut id, &mut p, ptr::null_mut()) };
        assert_eq!(code, FFI_OK, "gusset_buf_alloc");
        assert_ne!(id, 0, "the buffer must be registered in the handle");
        held = live() - before;
        assert!(
            held >= N,
            "the registered buffer must be counted, got {held}"
        );
        panic!("unwind-reclaim-registered");
    });
    assert_eq!(
        live() - before,
        held,
        "a buffer registered in the handle is owned by it: the unwind must neither free nor leak it"
    );
    if free_it {
        let code = unsafe { gusset_buf_free(handle, id, ptr::null_mut()) };
        assert_eq!(code, FFI_OK, "gusset_buf_free");
        assert_eq!(
            live(),
            before,
            "gusset_buf_free must return the buffer the panicking closure registered"
        );
    }
}

#[cfg(gusset_allocator_api)]
#[allow(
    clippy::incompatible_msrv,
    reason = "compiled only where allocator_probe.rs found the stable Allocator API"
)]
fn reclaim_buffer_alloc_in_guard() {
    use gusset::BufferAlloc;

    let before = live();
    guarded_panic("unwind-reclaim-buffer-alloc", || {
        let mut buf: Vec<u8, BufferAlloc> = Vec::new_in(BufferAlloc);
        match buf.try_reserve(N) {
            Ok(()) => {}
            Err(e) => panic!("try_reserve: {e}"),
        }
        assert!(
            buf.capacity() >= N,
            "try_reserve must provide the requested capacity, got {}",
            buf.capacity()
        );
        assert_eq!(
            live(),
            before + buf.capacity(),
            "BufferAlloc must count the reserved block once"
        );
        panic!("unwind-reclaim-buffer-alloc");
    });
    assert_eq!(
        live(),
        before,
        "BufferAlloc::deallocate must return the block when the firewall catches the panic"
    );
}

#[test]
fn guarded_panic_reclaims_counted_bytes_with_an_open_handle() {
    assert!(
        !counting_is_active(),
        "this test must not install Counting as a global allocator"
    );
    let baseline = live();
    let (handle, read_end) = open_handle();
    let opened = live();

    reclaim_raw_buffer_in_guard(handle);
    #[cfg(gusset_allocator_api)]
    reclaim_buffer_alloc_in_guard();
    reclaim_registered_buffer_after_guarded_panic(handle, true);

    // Left registered, the panicking closure's buffer is still the handle's:
    // closing the handle is what returns it.
    reclaim_registered_buffer_after_guarded_panic(handle, false);
    assert!(
        live() > opened,
        "the unfreed registered buffer must still be held before close"
    );
    let code = unsafe { gusset_handle_close(handle, ptr::null_mut()) };
    assert_eq!(code, FFI_OK, "gusset_handle_close");
    unsafe { libc::close(read_end) };
    assert_eq!(
        live(),
        baseline,
        "closing the handle must return every buffer a guarded panic left registered"
    );
}
