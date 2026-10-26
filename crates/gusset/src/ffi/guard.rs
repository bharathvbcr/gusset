//! Panic firewall and FFI boundary guard (R1, R2, R3).
#![allow(unsafe_code)]

use super::status::{FfiStatus, FFI_BAD_ARG, FFI_ERR, FFI_OK, FFI_PANIC};
use crate::pool::PoolError;
use std::panic::{catch_unwind, set_hook, take_hook, AssertUnwindSafe};
use std::ptr;

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Mutex, Once};

/// Panic location with static lifetime guarantees (R3).
#[derive(Debug, Clone, Copy)]
pub struct PanicLocation {
    /// Static string pointer to the source file name.
    pub file: &'static str,
    /// Source line number.
    pub line: u32,
}

const MAX_INTERNED_FILES: usize = 1024;
static FILE_STRING_CACHE: Mutex<Vec<&'static str>> = Mutex::new(Vec::new());

/// Interns a file path string into a process-lifetime static string.
///
/// `Location::file()` borrows from the hook's argument, so the string is copied and
/// leaked once per distinct panic site. The cache is bounded by the number of source
/// files that ever panic, and poisoning is recovered from rather than falling through
/// to an uncached `Box::leak` — that fallback leaked a fresh copy on *every* panic
/// once the lock was poisoned, turning a panic storm into unbounded memory growth.
pub fn intern_file_string(file: &str) -> &'static str {
    let mut cache = FILE_STRING_CACHE.lock().unwrap_or_else(|e| e.into_inner());
    for &s in cache.iter() {
        if s == file {
            return s;
        }
    }
    if cache.len() >= MAX_INTERNED_FILES {
        return "unknown_file";
    }
    let leaked: &'static str = Box::leak(file.to_string().into_boxed_str());
    cache.push(leaked);
    leaked
}

use std::thread::{self, ThreadId};

/// Upper bound on remembered panic locations.
///
/// An entry is added by the hook and removed by `take_panic_location` on the same
/// thread, immediately after `catch_unwind` returns. Panics that nobody takes —
/// adopter code, threads Gusset does not own, a worker dying outside its
/// `catch_unwind` — leave an entry behind, and `ThreadId`s are never reused, so an
/// unbounded list grew forever and made every panic an O(n) scan of it.
///
/// Overflow evicts the oldest entry. Losing a location degrades one error message to
/// "unknown", never to a wrong location: entries are keyed by thread id, so an
/// evicted entry can only ever be missing, not mismatched.
const MAX_PANIC_LOCATIONS: usize = 256;

/// The remembered panic locations, keyed by `K` (the thread id in production).
///
/// `len` mirrors `list.len()` and is only ever written while `list` is locked,
/// so it is exact at every point of its modification order. In steady state the
/// list is empty, and [`LocationTable::take`] — one call per FFI export and one
/// per job — answers `None` from a load instead of taking a process-global mutex.
///
/// Why the load cannot miss an entry its own thread is looking for: an entry is
/// inserted by the panic hook on the panicking thread, and taken by the firewall
/// on that same thread after `catch_unwind` returns. The insertion's `len` store
/// precedes the take's `len` load in that thread's program order, and an atomic
/// load never reads a value older than the thread's own earlier store to it
/// (write-read coherence), so the load returns that store or a later one. A
/// later value of 0 means the list was empty at that point, i.e. the entry had
/// been evicted — the same "missing, never wrong" outcome the lock gave. Entries
/// are keyed by thread, so another thread's panics are neither visible to nor
/// consumed by this thread's take, and cross-thread visibility is not needed.
/// The store is `Release` and the load `Acquire` anyway: free on x86, one
/// load-acquire on arm64, and it keeps the fast path correct if a later caller
/// ever takes an entry recorded by another thread.
///
/// A thread's untaken entry (adopter code, a worker dying outside its
/// `catch_unwind`) keeps `len` above zero until eviction, which only turns the
/// fast path back into the locked scan.
struct LocationTable<K> {
    list: Mutex<Vec<(K, PanicLocation)>>,
    len: AtomicUsize,
    cap: usize,
}

impl<K: Copy + PartialEq> LocationTable<K> {
    const fn new(cap: usize) -> Self {
        Self {
            list: Mutex::new(Vec::new()),
            len: AtomicUsize::new(0),
            cap,
        }
    }

    fn record(&self, key: K, loc: PanicLocation) {
        let mut list = self.list.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(pos) = list.iter().position(|(t, _)| *t == key) {
            list[pos].1 = loc;
        } else {
            if list.len() >= self.cap {
                list.remove(0);
            }
            list.push((key, loc));
        }
        self.len.store(list.len(), Ordering::Release);
    }

    fn take(&self, key: K) -> Option<PanicLocation> {
        if self.len.load(Ordering::Acquire) == 0 {
            return None;
        }
        let mut list = self.list.lock().unwrap_or_else(|e| e.into_inner());
        let pos = list.iter().position(|(t, _)| *t == key)?;
        let taken = list.swap_remove(pos).1;
        self.len.store(list.len(), Ordering::Release);
        Some(taken)
    }

    fn len(&self) -> usize {
        self.list.lock().unwrap_or_else(|e| e.into_inner()).len()
    }
}

static PANIC_LOCATIONS: LocationTable<ThreadId> = LocationTable::new(MAX_PANIC_LOCATIONS);

/// Takes the last recorded panic location on the current thread.
pub fn take_panic_location() -> Option<PanicLocation> {
    PANIC_LOCATIONS.take(thread::current().id())
}

/// Installs the Gusset global panic hook once (R2).
///
/// Records source location keyed by thread id for the firewall to read.
pub fn install_panic_hook() {
    // A `Once`, not a flag swapped before the hook exists: with the flag, a
    // second caller (another handle opening, gusset_init) returned while the
    // first was still between swap and set_hook, and a panic in that window
    // was caught with no location. `call_once_force` blocks concurrent callers
    // until the hook is in, and retries after a failed attempt rather than
    // poisoning every later call.
    static INSTALL: Once = Once::new();
    INSTALL.call_once_force(|_| {
        // Record the location, then call the hook we replaced. libtest prints
        // assertion failures through its hook; replacing it outright made every
        // Rust test that opened a handle fail with no message.
        let previous = take_hook();
        set_hook(Box::new(move |info| {
            if let Some(l) = info.location() {
                let loc = PanicLocation {
                    file: intern_file_string(l.file()),
                    line: l.line(),
                };
                PANIC_LOCATIONS.record(thread::current().id(), loc);
            }
            if !thread::current()
                .name()
                .is_some_and(|n| n.starts_with("gusset-w"))
            {
                previous(info);
            }
        }));
    });
}

/// Number of panic locations currently remembered. Test and diagnostic use.
pub fn panic_location_count() -> usize {
    PANIC_LOCATIONS.len()
}

const MAX_PANIC_PAYLOAD_BYTES: usize = 32 * 1024;

fn truncate_payload(mut s: String) -> String {
    if s.len() > MAX_PANIC_PAYLOAD_BYTES {
        let mut cutoff = MAX_PANIC_PAYLOAD_BYTES;
        while cutoff > 0 && !s.is_char_boundary(cutoff) {
            cutoff -= 1;
        }
        s.truncate(cutoff);
        s.push_str("... [truncated]");
    }
    s
}

/// Drops a caught panic payload without letting its destructor unwind.
///
/// The payload is engine-chosen: `panic_any(T)` where `T::drop` panics is a
/// legal Rust program. Dropping it after `catch_unwind` has returned is outside
/// every firewall, so the second panic unwound the worker thread and its ticket
/// never completed. A payload thrown by that destructor is leaked, not dropped:
/// it may be another bomb, and recursing into it has no bound.
pub fn drop_panic_payload(payload: Box<dyn std::any::Any + Send>) {
    if let Err(second) = catch_unwind(AssertUnwindSafe(move || drop(payload))) {
        // The ordinary `panic!` payloads cannot panic when dropped. Forgetting
        // them leaked a box on every panicking destructor (Miri flags it).
        if second.is::<&'static str>() || second.is::<String>() {
            drop(second);
        } else {
            std::mem::forget(second);
        }
        // The destructor's panic recorded its own location for this thread.
        // Callers take the engine's location before disposing of the payload,
        // so this one is stale and must not be reported for the next panic.
        let _ = take_panic_location();
    }
}

/// Extracts a string representation from an arbitrary panic payload without unwrapping (R3).
///
/// Consumes the payload and disposes of it through [`drop_panic_payload`]. Take
/// the panic location first: disposal discards any location its own panic records.
pub fn extract_panic_payload(payload: Box<dyn std::any::Any + Send>) -> String {
    let msg = describe_panic_payload(&*payload);
    drop_panic_payload(payload);
    msg
}

fn describe_panic_payload(payload: &(dyn std::any::Any + Send)) -> String {
    if let Some(s) = payload.downcast_ref::<&str>() {
        truncate_payload((*s).to_string())
    } else if let Some(s) = payload.downcast_ref::<String>() {
        truncate_payload(s.clone())
    } else if let Some(i) = payload.downcast_ref::<i32>() {
        format!("panic payload (i32): {}", i)
    } else if let Some(u) = payload.downcast_ref::<u32>() {
        format!("panic payload (u32): {}", u)
    } else if let Some(i) = payload.downcast_ref::<i64>() {
        format!("panic payload (i64): {}", i)
    } else if let Some(u) = payload.downcast_ref::<u64>() {
        format!("panic payload (u64): {}", u)
    } else if let Some(u) = payload.downcast_ref::<usize>() {
        format!("panic payload (usize): {}", u)
    } else if let Some(i) = payload.downcast_ref::<isize>() {
        format!("panic payload (isize): {}", i)
    } else if let Some(b) = payload.downcast_ref::<bool>() {
        format!("panic payload (bool): {}", b)
    } else {
        "Unknown Rust panic payload".to_string()
    }
}

/// Structured error passed through ffi_guard_code to populate FfiStatus (R1, R2, R3).
#[derive(Debug, Clone)]
pub struct FfiError {
    /// Return status code (FFI_ERR, FFI_PANIC, FFI_POISONED, FFI_BAD_ARG).
    pub code: i32,
    /// Detailed error message.
    pub msg: String,
    /// Source file path where error occurred.
    pub file: Option<&'static str>,
    /// Source line number where error occurred.
    pub line: u32,
}

/// An `FFI_ERR` with no source location.
///
/// Only a caught panic has a location worth reporting. This used to stamp a
/// file named `gusset.rs`, which does not exist, with this function's own line,
/// and Go printed it to users as "(at gusset.rs:204)".
impl From<String> for FfiError {
    fn from(msg: String) -> Self {
        Self {
            code: FFI_ERR,
            msg,
            file: None,
            line: 0,
        }
    }
}

/// A pool refusal keeps its class: the caller's argument is `FFI_BAD_ARG`, the
/// rest `FFI_ERR` (`DECISIONS.md` 2026-10-24).
impl From<PoolError> for FfiError {
    fn from(err: PoolError) -> Self {
        match err {
            PoolError::BadArg(msg) => Self {
                code: FFI_BAD_ARG,
                msg,
                file: None,
                line: 0,
            },
            PoolError::Failed(msg) => Self::from(msg),
        }
    }
}

impl From<&str> for FfiError {
    fn from(msg: &str) -> Self {
        Self::from(msg.to_string())
    }
}

/// Executes a closure behind the panic firewall, populates `status` (R1, R2, R3)
/// and returns the failure code, whatever `status` is.
///
/// The status is optional in every export. Reading the code back out of it
/// left a caller that passes NULL with no code at all, and every export then
/// reported `FFI_BAD_ARG` for an engine error or a caught panic alike — a C
/// host never learned its handle was poisoned (I2).
///
/// # Safety
///
/// The caller must ensure that `status` is either null or points to valid, writable
/// memory for an `FfiStatus` struct.
pub unsafe fn ffi_guard_code<F, R>(status: *mut FfiStatus, f: F) -> Result<R, i32>
where
    F: FnOnce() -> Result<R, FfiError>,
{
    if !status.is_null() {
        unsafe {
            ptr::write(status, FfiStatus::ok());
        }
    }

    // Go reuses OS threads across cgo calls; drop any location an earlier,
    // already-handled panic on this thread left behind (see execute_unit).
    let _ = take_panic_location();
    let unwind_result = catch_unwind(AssertUnwindSafe(f));

    match unwind_result {
        Ok(Ok(val)) => {
            if !status.is_null() {
                unsafe {
                    (*status).code = FFI_OK;
                }
            }
            Ok(val)
        }
        Ok(Err(ffi_err)) => {
            if !status.is_null() {
                // Capped like a panic payload. An engine's Err(String) is as
                // unbounded as its input, and Go reads at most 64 KiB of a
                // status message, cutting wherever that lands.
                let msg = truncate_payload(ffi_err.msg);
                let bytes = msg.as_bytes();
                // No location is a null file, not a placeholder name: the
                // field is read as a path in this crate.
                let (file_ptr, file_len) = match ffi_err.file {
                    Some(f) => (f.as_ptr(), f.len()),
                    None => (ptr::null(), 0),
                };
                unsafe {
                    ptr::write(
                        status,
                        FfiStatus::new_err(ffi_err.code, bytes, file_ptr, file_len, ffi_err.line),
                    );
                }
            }
            Err(ffi_err.code)
        }
        Err(panic_payload) => {
            // Location first: disposing of the payload can panic again and record
            // the destructor's location over the one that caused this failure.
            let loc = take_panic_location();
            let msg = extract_panic_payload(panic_payload);
            if !status.is_null() {
                let (file_ptr, file_len, line) = if let Some(l) = loc {
                    (l.file.as_ptr(), l.file.len(), l.line)
                } else {
                    (ptr::null(), 0, 0)
                };
                unsafe {
                    ptr::write(
                        status,
                        FfiStatus::new_err(FFI_PANIC, msg.as_bytes(), file_ptr, file_len, line),
                    );
                }
            }
            Err(FFI_PANIC)
        }
    }
}

/// Pointer-level tests of the firewall and of status message ownership. Pure
/// and thread-free, so the nightly Miri job runs them.
#[cfg(test)]
mod tests {
    use super::*;
    use crate::ffi::gusset_status_free;
    use crate::ffi::status::FFI_BAD_ARG;

    fn msg_of(st: &FfiStatus) -> String {
        if st.msg.is_null() {
            return String::new();
        }
        let bytes = unsafe { std::slice::from_raw_parts(st.msg, st.msg_len) };
        String::from_utf8_lossy(bytes).into_owned()
    }

    #[test]
    fn the_code_survives_a_null_status() {
        let r: Result<(), i32> = unsafe {
            ffi_guard_code(ptr::null_mut(), || {
                Err(FfiError {
                    code: FFI_BAD_ARG,
                    msg: "x".to_string(),
                    file: None,
                    line: 0,
                })
            })
        };
        assert_eq!(r, Err(FFI_BAD_ARG));
        let r: Result<(), i32> =
            unsafe { ffi_guard_code(ptr::null_mut(), || -> Result<(), FfiError> { panic!("p") }) };
        assert_eq!(r, Err(FFI_PANIC));
        let r = unsafe { ffi_guard_code(ptr::null_mut(), || Ok(7u8)) };
        assert_eq!(r, Ok(7));
    }

    #[test]
    fn a_status_message_is_freed_once_and_a_second_free_is_a_no_op() {
        let mut st = FfiStatus::ok();
        let r: Result<(), i32> =
            unsafe { ffi_guard_code(&mut st, || Err(FfiError::from("engine said no"))) };
        assert_eq!(r, Err(FFI_ERR));
        assert_eq!(st.code, FFI_ERR);
        assert_eq!(msg_of(&st), "engine said no");
        unsafe { gusset_status_free(&mut st) };
        assert!(st.msg.is_null());
        assert_eq!(st.msg_len, 0);
        // Miri reports a double free here if the first call left the pointer.
        unsafe { gusset_status_free(&mut st) };
        unsafe { gusset_status_free(ptr::null_mut()) };
    }

    #[test]
    fn non_string_and_self_destructing_payloads_are_contained() {
        let mut st = FfiStatus::ok();
        let _: Result<(), i32> = unsafe {
            ffi_guard_code(&mut st, || -> Result<(), FfiError> {
                std::panic::panic_any(7u8)
            })
        };
        assert_eq!(st.code, FFI_PANIC);
        assert_eq!(msg_of(&st), "Unknown Rust panic payload");
        unsafe { gusset_status_free(&mut st) };

        struct Bomb;
        impl Drop for Bomb {
            fn drop(&mut self) {
                panic!("payload destructor");
            }
        }
        let _: Result<(), i32> = unsafe {
            ffi_guard_code(&mut st, || -> Result<(), FfiError> {
                std::panic::panic_any(Bomb)
            })
        };
        assert_eq!(st.code, FFI_PANIC);
        unsafe { gusset_status_free(&mut st) };
    }

    #[test]
    fn an_error_message_is_capped_on_a_char_boundary() {
        let mut st = FfiStatus::ok();
        let _: Result<(), i32> = unsafe {
            ffi_guard_code(&mut st, || -> Result<(), FfiError> {
                Err(FfiError::from("€".repeat(20_000)))
            })
        };
        let bytes = unsafe { std::slice::from_raw_parts(st.msg, st.msg_len) };
        assert!(bytes.len() <= MAX_PANIC_PAYLOAD_BYTES + "... [truncated]".len());
        assert!(std::str::from_utf8(bytes).is_ok());
        unsafe { gusset_status_free(&mut st) };
    }

    /// A status names a source location only when it has a real one. Engine
    /// errors, bad arguments and poison used to carry `gusset.rs`, `ffi.rs` and
    /// `handle.rs` — none of which exist — and Go printed "(at gusset.rs:204)".
    #[test]
    fn only_a_caught_panic_carries_a_source_location() {
        fn assert_no_location(st: &FfiStatus, what: &str) {
            assert!(st.file.is_null(), "{what}: file must be null");
            assert_eq!(st.file_len, 0, "{what}: file_len");
            assert_eq!(st.line, 0, "{what}: line");
        }

        let mut st = FfiStatus::ok();
        let _: Result<(), i32> =
            unsafe { ffi_guard_code(&mut st, || Err(FfiError::from("engine said no"))) };
        assert_eq!(st.code, FFI_ERR);
        assert_no_location(&st, "engine error");
        unsafe { gusset_status_free(&mut st) };

        let mut st = FfiStatus::bad_arg("handle is null");
        assert_no_location(&st, "bad_arg");
        unsafe { gusset_status_free(&mut st) };

        let mut st = FfiStatus::poisoned("handle is poisoned");
        assert_no_location(&st, "poisoned");
        unsafe { gusset_status_free(&mut st) };

        // The panic path still reports where it happened: this file.
        install_panic_hook();
        let mut st = FfiStatus::ok();
        let _: Result<(), i32> =
            unsafe { ffi_guard_code(&mut st, || -> Result<(), FfiError> { panic!("located") }) };
        assert_eq!(st.code, FFI_PANIC);
        assert!(!st.file.is_null(), "a caught panic must keep its location");
        let file = unsafe { std::slice::from_raw_parts(st.file, st.file_len) };
        let file = String::from_utf8_lossy(file);
        assert!(file.ends_with("ffi/guard.rs"), "panic location: {file}");
        assert!(st.line > 0);
        unsafe { gusset_status_free(&mut st) };
    }

    fn loc(line: u32) -> PanicLocation {
        PanicLocation { file: "t.rs", line }
    }

    #[test]
    fn the_entry_count_returns_to_zero_after_take() {
        let t = LocationTable::<u64>::new(8);
        assert_eq!(t.len.load(Ordering::Acquire), 0);
        t.record(1, loc(10));
        t.record(2, loc(20));
        assert_eq!(t.len.load(Ordering::Acquire), 2);
        // A second panic on a thread replaces its entry; it does not add one.
        t.record(1, loc(11));
        assert_eq!(t.len.load(Ordering::Acquire), 2);
        assert_eq!(t.len(), 2);

        assert_eq!(t.take(1).map(|l| l.line), Some(11));
        assert_eq!(t.len.load(Ordering::Acquire), 1);
        // A key with no entry takes nothing and leaves the count alone.
        assert!(t.take(1).is_none());
        assert!(t.take(3).is_none());
        assert_eq!(t.len.load(Ordering::Acquire), 1);
        assert_eq!(t.take(2).map(|l| l.line), Some(20));
        assert_eq!(t.len.load(Ordering::Acquire), 0);
        assert_eq!(t.len(), 0);
    }

    #[test]
    fn eviction_at_the_cap_keeps_the_count_exact() {
        const CAP: usize = 4;
        let t = LocationTable::<u64>::new(CAP);
        for k in 0..(CAP as u64 * 3) {
            t.record(k, loc(k as u32));
            let want = (k as usize + 1).min(CAP);
            assert_eq!(t.len.load(Ordering::Acquire), want, "after record {k}");
            assert_eq!(t.len(), want, "list length after record {k}");
        }
        // The oldest were evicted: missing, never mismatched.
        for k in 0..(CAP as u64 * 2) {
            assert!(t.take(k).is_none(), "key {k} should have been evicted");
        }
        assert_eq!(t.len.load(Ordering::Acquire), CAP);
        for k in (CAP as u64 * 2)..(CAP as u64 * 3) {
            assert_eq!(t.take(k).map(|l| l.line), Some(k as u32));
        }
        assert_eq!(t.len.load(Ordering::Acquire), 0);
        assert_eq!(t.len(), 0);
    }

    /// With nothing recorded, a take answers from the count and never touches
    /// the lock. The list is held locked here; the pre-fast-path take blocked
    /// on it, so the answer never arrives and the receive times out.
    #[test]
    fn an_empty_table_is_taken_without_the_lock() {
        let t = std::sync::Arc::new(LocationTable::<u64>::new(4));
        let held = t.list.lock().unwrap_or_else(|e| e.into_inner());
        let (tx, rx) = std::sync::mpsc::channel();
        let taker = {
            let t = std::sync::Arc::clone(&t);
            thread::spawn(move || {
                let _ = tx.send(t.take(7).is_none());
            })
        };
        let answered = rx.recv_timeout(std::time::Duration::from_secs(10));
        drop(held);
        let joined = taker.join();
        assert_eq!(answered, Ok(true), "take must not wait on the lock");
        assert!(joined.is_ok());
    }

    /// Threads hammer one table, each recording its own key and taking it back
    /// at once. The cap is far above the key count, so eviction cannot explain a
    /// miss: a `None` here is a lost entry. That is what the count would cause if
    /// it were published after the list lock is released, since another thread's
    /// stale store could then read 0 while this thread's entry is in the list.
    #[test]
    fn threads_always_take_back_the_entry_they_just_recorded() {
        const THREADS: u64 = 8;
        let rounds: u32 = if cfg!(miri) { 20 } else { 200_000 };
        let t = LocationTable::<u64>::new(1024);
        let start = std::sync::Barrier::new(THREADS as usize);
        std::thread::scope(|scope| {
            for key in 0..THREADS {
                let (t, start) = (&t, &start);
                scope.spawn(move || {
                    start.wait();
                    for round in 0..rounds {
                        t.record(key, loc(round));
                        let got = t.take(key);
                        assert_eq!(
                            got.map(|l| l.line),
                            Some(round),
                            "thread {key} round {round}: lost or foreign entry"
                        );
                    }
                });
            }
        });
        assert_eq!(t.len.load(Ordering::Acquire), 0);
        assert_eq!(t.len(), 0);
    }

    /// The count is exact whenever the table is quiescent. Threads record
    /// together, stop at a barrier, and each checks the count is the number of
    /// entries; then they take together and check it is zero. A count published
    /// after the list lock is released can land out of order with another
    /// thread's update and read high or low once everything has settled, which
    /// the take-back test above cannot see (a high count only costs the fast
    /// path; it loses nothing).
    #[test]
    fn the_count_is_exact_once_concurrent_updates_settle() {
        const THREADS: u64 = 8;
        let rounds: u32 = if cfg!(miri) { 4 } else { 2_000 };
        let t = LocationTable::<u64>::new(1024);
        let gate = std::sync::Barrier::new(THREADS as usize);
        std::thread::scope(|scope| {
            for key in 0..THREADS {
                let (t, gate) = (&t, &gate);
                scope.spawn(move || {
                    for round in 0..rounds {
                        gate.wait();
                        t.record(key, loc(round));
                        gate.wait();
                        assert_eq!(
                            t.len.load(Ordering::Acquire),
                            THREADS as usize,
                            "thread {key} round {round}: count after the records settled"
                        );
                        assert_eq!(t.len(), THREADS as usize);
                        gate.wait();
                        assert_eq!(t.take(key).map(|l| l.line), Some(round));
                        gate.wait();
                        assert_eq!(
                            t.len.load(Ordering::Acquire),
                            0,
                            "thread {key} round {round}: count after the takes settled"
                        );
                        assert_eq!(t.len(), 0);
                    }
                });
            }
        });
    }

    /// Several threads panic under `catch_unwind` at distinct sites and take
    /// their own location, concurrently and repeatedly, through the real hook
    /// and the real global table. Each must get its own site, and nothing may
    /// be left behind for the next call on that thread.
    #[test]
    fn concurrent_panics_each_get_their_own_location() {
        // One site per thread, each on its own line, paired with that line.
        let sites: [(u32, fn()); 6] = [
            (line!(), || panic!("site 0")),
            (line!(), || panic!("site 1")),
            (line!(), || panic!("site 2")),
            (line!(), || panic!("site 3")),
            (line!(), || panic!("site 4")),
            (line!(), || panic!("site 5")),
        ];
        install_panic_hook();
        let start = std::sync::Barrier::new(sites.len());
        std::thread::scope(|scope| {
            for (want_line, site) in sites {
                let start = &start;
                scope.spawn(move || {
                    start.wait();
                    for round in 0..200 {
                        // A quiet call between panics: no location to find.
                        assert!(
                            take_panic_location().is_none(),
                            "round {round}: stale location"
                        );
                        let mut st = FfiStatus::ok();
                        let r: Result<(), i32> = unsafe {
                            ffi_guard_code(&mut st, || -> Result<(), FfiError> {
                                site();
                                Ok(())
                            })
                        };
                        assert_eq!(r, Err(FFI_PANIC));
                        assert!(!st.file.is_null());
                        let file = unsafe { std::slice::from_raw_parts(st.file, st.file_len) };
                        assert!(
                            String::from_utf8_lossy(file).ends_with("ffi/guard.rs"),
                            "wrong file"
                        );
                        assert_eq!(st.line, want_line, "round {round}: another thread's site");
                        unsafe { gusset_status_free(&mut st) };
                        // The firewall consumed it: nothing is left for this thread.
                        assert!(take_panic_location().is_none());
                    }
                });
            }
        });
    }
}
