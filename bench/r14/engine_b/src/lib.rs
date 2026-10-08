//! Engine B of the R14 spike: the default allocator, a non-string panic
//! payload, a panic hook that counts what it sees, and the same allocator and
//! thread-local probes as engine A.

// R7 bans thread_local! in Gusset's ffi/ and pool/ because goroutines migrate
// between OS threads. These engines are not Gusset code: eb_tls_next is the probe
// for per-copy thread-local storage, and the host pins its OS thread to call it.
#![allow(clippy::disallowed_macros)]

use std::panic::{catch_unwind, AssertUnwindSafe};
use std::sync::atomic::{AtomicU64, Ordering};

static HOOK_HITS: AtomicU64 = AtomicU64::new(0);

/// Returns twice the length of an `n`-character string built on the heap, or -2
/// when `n < 0`, which panics with a `u32` payload and is caught here.
#[no_mangle]
pub extern "C" fn eb_call(n: i64) -> i64 {
    catch_unwind(AssertUnwindSafe(|| {
        if n < 0 {
            std::panic::panic_any(n as u32);
        }
        let s: String = (0..n).map(|i| char::from(b'a' + (i % 26) as u8)).collect();
        s.len() as i64 * 2
    }))
    .unwrap_or(-2)
}

/// Installs a counting panic hook in engine B's copy of std. If B shared std
/// with anything else, this hook would also count — or be replaced by — the
/// other copy's panics.
#[no_mangle]
pub extern "C" fn eb_set_counting_hook() {
    std::panic::set_hook(Box::new(|_| {
        HOOK_HITS.fetch_add(1, Ordering::Relaxed);
    }));
}

/// Panics engine B's hook has seen.
#[no_mangle]
pub extern "C" fn eb_hook_hits() -> u64 {
    HOOK_HITS.load(Ordering::Relaxed)
}

static HELD_B: std::sync::Mutex<Vec<Vec<u8>>> = std::sync::Mutex::new(Vec::new());

thread_local!(static TLS_B: std::cell::Cell<i64> = const { std::cell::Cell::new(0) });

/// Allocates and keeps `bytes` through engine B's global allocator.
#[no_mangle]
pub extern "C" fn eb_hold(bytes: u64) {
    let mut held = HELD_B.lock().unwrap_or_else(|p| p.into_inner());
    held.push(vec![1u8; bytes as usize]);
}

/// Frees everything `eb_hold` kept.
#[no_mangle]
pub extern "C" fn eb_release() {
    HELD_B.lock().unwrap_or_else(|p| p.into_inner()).clear();
}

/// Increments and returns this OS thread's counter in engine B's copy of std,
/// or -1 if that copy believes the thread is still unwinding.
#[no_mangle]
pub extern "C" fn eb_tls_next() -> i64 {
    if std::thread::panicking() {
        return -1;
    }
    TLS_B.with(|c| {
        c.set(c.get() + 1);
        c.get()
    })
}
