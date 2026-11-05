//! A caught panic must return Gusset-owned buffer bytes before any handle closes.
//!
//! `catch_unwind` runs `Drop` for values still live in the closure. The caller
//! then drops the `Err` payload. `RawBuffer`'s `Drop`, and on Rust 1.100+
//! `BufferAlloc::deallocate`, are what put those bytes back on the live
//! counter. This binary installs no `Counting` global allocator and opens no
//! handle, so a counter that stays up is a `Drop` that did not run.
//!
//! Separate binary: the counters are process-global. Another test in the same
//! process would move the baseline this measures. A plain `Vec<u8>` is not
//! counted here and is not asserted on.

use std::any::Any;
use std::sync::Mutex;

use gusset::ffi::alloc::counting_is_active;
use gusset::get_alloc_stats;
use gusset::pool::sys::RawBuffer;

/// Message the panic hook observed. The hook is `'static`, so it cannot borrow
/// the caller's stack. One test runs in this binary.
static HOOK_MESSAGE: Mutex<String> = Mutex::new(String::new());

fn lock_message() -> std::sync::MutexGuard<'static, String> {
    match HOOK_MESSAGE.lock() {
        Ok(guard) => guard,
        Err(poisoned) => poisoned.into_inner(),
    }
}

fn live() -> usize {
    get_alloc_stats().live_bytes
}

/// Catches `body` without printing the panic this test induces on purpose.
///
/// The message is taken from `payload_as_str` rather than a downcast: a panic
/// payload is not only `&str` or `String`, and an earlier failure must stay
/// visible after `catch_unwind` has swallowed it.
fn catch_quiet(body: impl FnOnce() + std::panic::UnwindSafe) -> Result<(), Box<dyn Any + Send>> {
    lock_message().clear();
    let previous = std::panic::take_hook();
    std::panic::set_hook(Box::new(|info| {
        *lock_message() = match info.payload_as_str() {
            Some(text) => text.to_owned(),
            None => info.to_string(),
        };
    }));
    let caught = std::panic::catch_unwind(body);
    std::panic::set_hook(previous);
    caught
}

/// Drops the caught payload, then requires the live counter back at `before`.
fn assert_reclaimed(before: usize, caught: Result<(), Box<dyn Any + Send>>, marker: &str) {
    let payload = match caught {
        Err(payload) => payload,
        Ok(()) => panic!("the closure must panic while the buffer is still live"),
    };
    let text = lock_message().clone();
    assert!(
        text.contains(marker),
        "panic must be the intentional unwind ({marker}), not an earlier failure: {text}"
    );
    drop(payload);
    assert_eq!(
        live(),
        before,
        "after the unwind and dropping its payload, live bytes must be back at \
         the baseline; no handle is closed"
    );
}

fn reclaim_raw_buffer() {
    const N: usize = 1 << 20;
    let before = live();
    let caught = catch_quiet(std::panic::AssertUnwindSafe(|| {
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
        panic!("unwind-reclaim-raw");
    }));
    assert_reclaimed(before, caught, "unwind-reclaim-raw");
}

#[cfg(gusset_allocator_api)]
#[allow(
    clippy::incompatible_msrv,
    reason = "compiled only where allocator_probe.rs found the stable Allocator API"
)]
fn reclaim_buffer_alloc() {
    use std::alloc::Allocator;

    use gusset::BufferAlloc;

    const N: usize = 1 << 20;
    let before = live();
    let caught = catch_quiet(std::panic::AssertUnwindSafe(|| {
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
    }));
    assert_reclaimed(before, caught, "unwind-reclaim-buffer-alloc");
}

#[test]
fn unwind_reclaims_counted_bytes_before_close() {
    assert!(
        !counting_is_active(),
        "this test must not install Counting as a global allocator"
    );
    reclaim_raw_buffer();
    #[cfg(gusset_allocator_api)]
    reclaim_buffer_alloc();
}
