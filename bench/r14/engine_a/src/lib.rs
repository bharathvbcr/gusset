//! Engine A of the R14 spike: its own `#[global_allocator]`, a `thread_local!`,
//! a worker thread per call, a panic caught inside the engine, and probes for
//! its allocator and its thread-local storage.

// R7 bans thread_local! in Gusset's ffi/ and pool/ because goroutines migrate
// between OS threads. These engines are not Gusset code: ea_tls_next is the probe
// for per-copy thread-local storage, and the host pins its OS thread to call it.
#![allow(clippy::disallowed_macros)]

use std::alloc::{GlobalAlloc, Layout, System};
use std::panic::{catch_unwind, AssertUnwindSafe};
use std::sync::atomic::{AtomicI64, Ordering};

struct Counting;

static LIVE: AtomicI64 = AtomicI64::new(0);

unsafe impl GlobalAlloc for Counting {
    unsafe fn alloc(&self, l: Layout) -> *mut u8 {
        LIVE.fetch_add(l.size() as i64, Ordering::Relaxed);
        System.alloc(l)
    }
    unsafe fn dealloc(&self, p: *mut u8, l: Layout) {
        LIVE.fetch_sub(l.size() as i64, Ordering::Relaxed);
        System.dealloc(p, l)
    }
}

#[global_allocator]
static GA: Counting = Counting;

/// Returns the sum of `0..n` computed on a fresh thread, or -1 when `n < 0`,
/// which panics inside the engine and is caught here.
#[no_mangle]
pub extern "C" fn ea_call(n: i64) -> i64 {
    catch_unwind(AssertUnwindSafe(|| {
        if n < 0 {
            panic!("engine_a induced panic {n}");
        }
        let worker = std::thread::Builder::new()
            .stack_size(1 << 20)
            .spawn(move || (0..n).collect::<Vec<i64>>().iter().sum::<i64>());
        match worker.map(|h| h.join()) {
            Ok(Ok(sum)) => sum,
            _ => -100,
        }
    }))
    .unwrap_or(-1)
}

/// Bytes live in engine A's allocator. Engine B's allocations never show here.
#[no_mangle]
pub extern "C" fn ea_live_bytes() -> i64 {
    LIVE.load(Ordering::Relaxed)
}

/// Installs a silent panic hook in engine A's copy of std.
#[no_mangle]
pub extern "C" fn ea_set_quiet_hook() {
    std::panic::set_hook(Box::new(|_| {}));
}

static HELD_A: std::sync::Mutex<Vec<Vec<u8>>> = std::sync::Mutex::new(Vec::new());

thread_local!(static TLS_A: std::cell::Cell<i64> = const { std::cell::Cell::new(0) });

/// Allocates and keeps `bytes` through engine A's global allocator.
#[no_mangle]
pub extern "C" fn ea_hold(bytes: u64) {
    let mut held = HELD_A.lock().unwrap_or_else(|p| p.into_inner());
    held.push(vec![1u8; bytes as usize]);
}

/// Frees everything `ea_hold` kept.
#[no_mangle]
pub extern "C" fn ea_release() {
    HELD_A.lock().unwrap_or_else(|p| p.into_inner()).clear();
}

/// Increments and returns this OS thread's counter in engine A's copy of std,
/// or -1 if that copy believes the thread is still unwinding.
#[no_mangle]
pub extern "C" fn ea_tls_next() -> i64 {
    if std::thread::panicking() {
        return -1;
    }
    TLS_A.with(|c| {
        c.set(c.get() + 1);
        c.get()
    })
}
