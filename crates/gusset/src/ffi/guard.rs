//! Panic firewall and FFI boundary guard (R1, R2, R3).
#![allow(unsafe_code)]

use super::status::{FfiStatus, FFI_ERR, FFI_OK, FFI_PANIC};
use std::panic::{catch_unwind, set_hook, AssertUnwindSafe};
use std::ptr;
use std::sync::atomic::{AtomicBool, Ordering};

use std::sync::Mutex;

/// Panic location with static lifetime guarantees (R3).
#[derive(Debug, Clone, Copy)]
pub struct PanicLocation {
    /// Static string pointer to the source file name.
    pub file: &'static str,
    /// Source line number.
    pub line: u32,
}

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

static PANIC_LOCATIONS: Mutex<Vec<(ThreadId, PanicLocation)>> = Mutex::new(Vec::new());

/// Takes the last recorded panic location on the current thread.
pub fn take_panic_location() -> Option<PanicLocation> {
    let mut list = PANIC_LOCATIONS.lock().unwrap_or_else(|e| e.into_inner());
    let id = thread::current().id();
    let pos = list.iter().position(|(t, _)| *t == id)?;
    Some(list.swap_remove(pos).1)
}

/// Installs the Gusset global panic hook once (R2).
///
/// Records source location keyed by thread id for the firewall to read.
pub fn install_panic_hook() {
    static INSTALLED: AtomicBool = AtomicBool::new(false);
    if !INSTALLED.swap(true, Ordering::SeqCst) {
        set_hook(Box::new(|info| {
            if let Some(l) = info.location() {
                let loc = PanicLocation {
                    file: intern_file_string(l.file()),
                    line: l.line(),
                };
                let id = thread::current().id();
                let mut list = PANIC_LOCATIONS.lock().unwrap_or_else(|e| e.into_inner());
                if let Some(pos) = list.iter().position(|(t, _)| *t == id) {
                    list[pos].1 = loc;
                } else {
                    if list.len() >= MAX_PANIC_LOCATIONS {
                        list.remove(0);
                    }
                    list.push((id, loc));
                }
            }
        }));
    }
}

/// Number of panic locations currently remembered. Test and diagnostic use.
pub fn panic_location_count() -> usize {
    PANIC_LOCATIONS
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .len()
}

/// Extracts a string representation from an arbitrary panic payload without unwrapping (R3).
pub fn extract_panic_payload(payload: Box<dyn std::any::Any + Send>) -> String {
    if let Some(s) = payload.downcast_ref::<&str>() {
        (*s).to_string()
    } else if let Some(s) = payload.downcast_ref::<String>() {
        s.clone()
    } else if let Some(i) = payload.downcast_ref::<i32>() {
        format!("panic payload (i32): {}", i)
    } else if let Some(u) = payload.downcast_ref::<u64>() {
        format!("panic payload (u64): {}", u)
    } else {
        "Unknown Rust panic payload".to_string()
    }
}

/// Structured error passed through ffi_guard to populate FfiStatus (R1, R2, R3).
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

impl From<String> for FfiError {
    fn from(msg: String) -> Self {
        Self {
            code: FFI_ERR,
            msg,
            file: Some("gusset.rs"),
            line: line!(),
        }
    }
}

impl From<&str> for FfiError {
    fn from(msg: &str) -> Self {
        Self::from(msg.to_string())
    }
}

/// Executes a closure behind a panic firewall and populates out_status (R1, R2, R3).
///
/// # Safety
///
/// The caller must ensure that `status` is either null or points to valid, writable
/// memory for an `FfiStatus` struct.
pub unsafe fn ffi_guard<F, R>(status: *mut FfiStatus, f: F) -> Option<R>
where
    F: FnOnce() -> Result<R, FfiError>,
{
    if !status.is_null() {
        unsafe {
            ptr::write(status, FfiStatus::ok());
        }
    }

    let unwind_result = catch_unwind(AssertUnwindSafe(f));

    match unwind_result {
        Ok(Ok(val)) => {
            if !status.is_null() {
                unsafe {
                    (*status).code = FFI_OK;
                }
            }
            Some(val)
        }
        Ok(Err(ffi_err)) => {
            if !status.is_null() {
                let bytes = ffi_err.msg.as_bytes();
                let (file_ptr, file_len) = match ffi_err.file {
                    Some(f) => (f.as_ptr(), f.len()),
                    None => (c"unknown".as_ptr() as *const u8, 7),
                };
                unsafe {
                    ptr::write(
                        status,
                        FfiStatus::new_err(ffi_err.code, bytes, file_ptr, file_len, ffi_err.line),
                    );
                }
            }
            None
        }
        Err(panic_payload) => {
            let msg = extract_panic_payload(panic_payload);
            let loc = take_panic_location();
            if !status.is_null() {
                let (file_ptr, file_len, line) = if let Some(l) = loc {
                    (l.file.as_ptr(), l.file.len(), l.line)
                } else {
                    (c"unknown".as_ptr() as *const u8, 7, 0)
                };
                unsafe {
                    ptr::write(
                        status,
                        FfiStatus::new_err(FFI_PANIC, msg.as_bytes(), file_ptr, file_len, line),
                    );
                }
            }
            None
        }
    }
}
