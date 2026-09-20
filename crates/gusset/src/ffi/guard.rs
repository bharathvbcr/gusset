//! Panic firewall and FFI boundary guard (R1, R2, R3).
#![allow(unsafe_code)]

use super::status::{FfiStatus, FFI_ERR, FFI_OK, FFI_PANIC};
use std::cell::RefCell;
use std::panic::{catch_unwind, set_hook, AssertUnwindSafe};
use std::ptr;
use std::sync::atomic::{AtomicBool, Ordering};

#[derive(Clone)]
struct PanicLocation {
    file: String,
    line: u32,
}

thread_local! {
    static LAST_PANIC_LOCATION: RefCell<Option<PanicLocation>> = const { RefCell::new(None) };
}

/// Installs the Gusset global panic hook once (R2).
///
/// Records source location into thread-local storage for the firewall to read.
pub fn install_panic_hook() {
    static INSTALLED: AtomicBool = AtomicBool::new(false);
    if !INSTALLED.swap(true, Ordering::SeqCst) {
        set_hook(Box::new(|info| {
            let loc = info.location().map(|l| PanicLocation {
                file: l.file().to_string(),
                line: l.line(),
            });
            LAST_PANIC_LOCATION.with(|cell| {
                *cell.borrow_mut() = loc;
            });
        }));
    }
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

/// Executes a closure behind a panic firewall and populates out_status (R1, R2, R3).
///
/// # Safety
///
/// The caller must ensure that `status` is either null or points to valid, writable
/// memory for an `FfiStatus` struct.
pub unsafe fn ffi_guard<F, R>(status: *mut FfiStatus, f: F) -> Option<R>
where
    F: FnOnce() -> Result<R, String>,
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
        Ok(Err(err_msg)) => {
            if !status.is_null() {
                let bytes = err_msg.as_bytes();
                unsafe {
                    ptr::write(
                        status,
                        FfiStatus::new_err(FFI_ERR, bytes, c"gusset.rs".as_ptr() as *const u8, 9, line!()),
                    );
                }
            }
            None
        }
        Err(panic_payload) => {
            let msg = extract_panic_payload(panic_payload);
            let loc = LAST_PANIC_LOCATION.with(|cell| cell.borrow_mut().take());
            if !status.is_null() {
                let (file_ptr, file_len, line) = if let Some(ref l) = loc {
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
