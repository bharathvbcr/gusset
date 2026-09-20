//! Exported C ABI symbols for Gusset runtime (R1, R12).
#![allow(unsafe_code)]

pub mod alloc;
pub mod guard;
pub mod status;

use crate::header::CallHeader;
use crate::pool::{Handle, JobResult};
use alloc::{get_alloc_stats, AllocStats};
use guard::{ffi_guard, install_panic_hook};
use static_assertions::{assert_eq_align, assert_eq_size};
use status::{FfiStatus, FFI_BAD_ARG, FFI_ERR, FFI_OK, FFI_PANIC, FFI_POISONED};
use std::mem::{align_of, size_of};
use std::ptr;
use std::sync::{Arc, Mutex};

/// ABI version and type layout definition for cross-boundary integrity check (R12).
#[repr(C)]
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct AbiLayout {
    /// ABI contract version. Go init() checks for match.
    pub version: u32,
    /// Byte sizes of CallHeader, FfiStatus, and AbiLayout.
    pub sizes: [u32; 3],
    /// Alignments of CallHeader, FfiStatus, and AbiLayout.
    pub aligns: [u32; 3],
}

assert_eq_size!(AbiLayout, [u8; 28]);
assert_eq_align!(AbiLayout, u32);

/// Current Gusset ABI version.
pub const GUSSET_ABI_VERSION: u32 = 1;

static LOG_BUFFER: Mutex<Vec<u8>> = Mutex::new(Vec::new());

/// Appends a log line to the bounded internal ring buffer.
pub fn log_event(line: &str) {
    if let Ok(mut buf) = LOG_BUFFER.lock() {
        if buf.len() + line.len() > 65536 {
            buf.clear();
        }
        buf.extend_from_slice(line.as_bytes());
        buf.push(b'\n');
    }
}

// ----------------------------------------------------------------------------
// The 14 Exported C Functions (R1)
// ----------------------------------------------------------------------------

/// 1. Exports the ABI layout and sizes of all repr(C) types (R12).
///
/// # Safety
///
/// If non-null, `out` must point to valid writable memory for an `AbiLayout` struct.
#[no_mangle]
pub unsafe extern "C" fn gusset_abi_layout(out: *mut AbiLayout) {
    if !out.is_null() {
        let layout = AbiLayout {
            version: GUSSET_ABI_VERSION,
            sizes: [
                size_of::<CallHeader>() as u32,
                size_of::<FfiStatus>() as u32,
                size_of::<AbiLayout>() as u32,
            ],
            aligns: [
                align_of::<CallHeader>() as u32,
                align_of::<FfiStatus>() as u32,
                align_of::<AbiLayout>() as u32,
            ],
        };
        unsafe {
            ptr::write(out, layout);
        }
    }
}

/// 2. Initializes the Gusset runtime and installs panic hook.
///
/// # Safety
///
/// Safe to call across FFI. Modifies global process panic state.
#[no_mangle]
pub unsafe extern "C" fn gusset_init() -> i32 {
    install_panic_hook();
    FFI_OK
}

/// 3. Shuts down the Gusset runtime.
///
/// # Safety
///
/// Safe to call across FFI.
#[no_mangle]
pub unsafe extern "C" fn gusset_shutdown(_drain_ms: u32) -> i32 {
    FFI_OK
}

/// 4. Opens a new handle with a worker pool (I4).
///
/// # Safety
///
/// `out_handle` must point to writable memory. `status` if non-null must point to writable `FfiStatus`.
#[no_mangle]
pub unsafe extern "C" fn gusset_handle_open(
    pool_size: u32,
    pipe_write_fd: i32,
    out_handle: *mut *mut Handle,
    status: *mut FfiStatus,
) -> i32 {
    if out_handle.is_null() {
        if !status.is_null() {
            unsafe {
                ptr::write(status, FfiStatus::bad_arg("out_handle is null"));
            }
        }
        return FFI_BAD_ARG;
    }

    let res = unsafe {
        ffi_guard(status, || {
            let handle = Handle::open(pool_size, pipe_write_fd)?;
            let raw = Arc::into_raw(handle) as *mut Handle;
            ptr::write(out_handle, raw);
            Ok(())
        })
    };

    if res.is_some() {
        FFI_OK
    } else if !status.is_null() {
        unsafe { (*status).code }
    } else {
        FFI_BAD_ARG
    }
}

/// 5. Closes a handle and terminates worker threads.
///
/// # Safety
///
/// `handle` must be a valid handle pointer previously returned by `gusset_handle_open`.
#[no_mangle]
pub unsafe extern "C" fn gusset_handle_close(
    handle: *mut Handle,
    status: *mut FfiStatus,
) -> i32 {
    if handle.is_null() {
        if !status.is_null() {
            unsafe {
                ptr::write(status, FfiStatus::bad_arg("handle is null"));
            }
        }
        return FFI_BAD_ARG;
    }

    let res = unsafe {
        ffi_guard(status, || {
            let arc = Arc::from_raw(handle);
            arc.close();
            drop(arc);
            Ok(())
        })
    };

    if res.is_some() {
        FFI_OK
    } else if !status.is_null() {
        unsafe { (*status).code }
    } else {
        FFI_BAD_ARG
    }
}

/// 6. Submits work to the handle pool (R16).
///
/// # Safety
///
/// `handle`, `header`, and `out_ticket` must be valid non-null pointers.
/// `input_ptr` must point to at least `input_len` readable bytes if `input_len > 0`.
#[no_mangle]
pub unsafe extern "C" fn gusset_submit(
    handle: *mut Handle,
    header: *const CallHeader,
    input_ptr: *const u8,
    input_len: usize,
    buffer_id: u64,
    out_ticket: *mut u64,
    status: *mut FfiStatus,
) -> i32 {
    if handle.is_null() || header.is_null() || out_ticket.is_null() {
        if !status.is_null() {
            unsafe {
                ptr::write(status, FfiStatus::bad_arg("null argument passed to submit"));
            }
        }
        return FFI_BAD_ARG;
    }

    let h = unsafe { &*handle };
    if h.is_poisoned() {
        if !status.is_null() {
            unsafe {
                ptr::write(status, FfiStatus::poisoned("handle is poisoned"));
            }
        }
        return FFI_POISONED;
    }

    let call_header = unsafe { *header };
    let input_slice = if input_len > 0 && !input_ptr.is_null() {
        unsafe { std::slice::from_raw_parts(input_ptr, input_len) }
    } else {
        &[]
    };

    let res = unsafe {
        ffi_guard(status, || {
            let ticket = h.submit(call_header, input_slice, buffer_id)?;
            ptr::write(out_ticket, ticket);
            Ok(())
        })
    };

    if res.is_some() {
        FFI_OK
    } else if !status.is_null() {
        unsafe { (*status).code }
    } else {
        FFI_BAD_ARG
    }
}

/// 7. Takes a completed job result (moves out exactly once).
///
/// # Safety
///
/// `handle`, `out_buf_id`, `out_ptr`, and `out_len` must be valid writable pointers.
#[no_mangle]
pub unsafe extern "C" fn gusset_take(
    handle: *mut Handle,
    ticket: u64,
    out_buf_id: *mut u64,
    out_ptr: *mut *mut u8,
    out_len: *mut usize,
    status: *mut FfiStatus,
) -> i32 {
    if handle.is_null() || out_buf_id.is_null() || out_ptr.is_null() || out_len.is_null() {
        if !status.is_null() {
            unsafe {
                ptr::write(status, FfiStatus::bad_arg("null argument passed to take"));
            }
        }
        return FFI_BAD_ARG;
    }

    let h = unsafe { &*handle };

    let res = unsafe {
        ffi_guard(status, || {
            let job_result = h.take(ticket)?;
            match job_result {
                JobResult::Ok(data) => {
                    if data.is_empty() {
                        ptr::write(out_buf_id, 0);
                        ptr::write(out_ptr, ptr::null_mut());
                        ptr::write(out_len, 0);
                    } else {
                        let len = data.len();
                        let (buf_id, buf_ptr) = h.buf_alloc(len)?;
                        ptr::copy_nonoverlapping(data.as_ptr(), buf_ptr, len);
                        ptr::write(out_buf_id, buf_id);
                        ptr::write(out_ptr, buf_ptr);
                        ptr::write(out_len, len);
                    }
                    Ok(())
                }
                JobResult::Err(msg) => Err(msg),
                JobResult::Panic(msg) => {
                    Err(format!("PANIC: {}", msg))
                }
                JobResult::Cancelled(reason) => Err(format!("cancelled: {:?}", reason)),
            }
        })
    };

    if res.is_some() {
        FFI_OK
    } else if !status.is_null() {
        unsafe {
            if (*status).code == FFI_ERR && !(*status).msg.is_null() && (*status).msg_len > 0 {
                let slice = std::slice::from_raw_parts((*status).msg, (*status).msg_len);
                if let Ok(msg_str) = std::str::from_utf8(slice) {
                    if msg_str.starts_with("PANIC:") {
                        (*status).code = FFI_PANIC;
                    }
                }
            }
            (*status).code
        }
    } else {
        FFI_BAD_ARG
    }
}

/// 8. Cancels a specific job ticket (I3).
///
/// # Safety
///
/// `handle` must be a valid handle pointer.
#[no_mangle]
pub unsafe extern "C" fn gusset_cancel(
    handle: *mut Handle,
    ticket: u64,
    status: *mut FfiStatus,
) -> i32 {
    if handle.is_null() {
        if !status.is_null() {
            unsafe {
                ptr::write(status, FfiStatus::bad_arg("handle is null"));
            }
        }
        return FFI_BAD_ARG;
    }

    let h = unsafe { &*handle };
    let res = unsafe {
        ffi_guard(status, || {
            h.cancel(ticket);
            Ok(())
        })
    };

    if res.is_some() {
        FFI_OK
    } else if !status.is_null() {
        unsafe { (*status).code }
    } else {
        FFI_BAD_ARG
    }
}

/// 9. Cancels all pending jobs on the handle (I3).
///
/// # Safety
///
/// `handle` must be a valid handle pointer.
#[no_mangle]
pub unsafe extern "C" fn gusset_cancel_all(
    handle: *mut Handle,
    status: *mut FfiStatus,
) -> i32 {
    if handle.is_null() {
        if !status.is_null() {
            unsafe {
                ptr::write(status, FfiStatus::bad_arg("handle is null"));
            }
        }
        return FFI_BAD_ARG;
    }

    let h = unsafe { &*handle };
    let res = unsafe {
        ffi_guard(status, || {
            h.cancel_all();
            Ok(())
        })
    };

    if res.is_some() {
        FFI_OK
    } else if !status.is_null() {
        unsafe { (*status).code }
    } else {
        FFI_BAD_ARG
    }
}

/// 10. Frees a status message allocated by Rust (R4).
///
/// # Safety
///
/// If non-null, `status` must point to a valid `FfiStatus` struct.
#[no_mangle]
pub unsafe extern "C" fn gusset_status_free(status: *mut FfiStatus) {
    if !status.is_null() {
        unsafe {
            (*status).free_msg();
        }
    }
}

/// 11. Exports allocator statistics without allocating memory.
///
/// # Safety
///
/// If non-null, `out` must point to valid writable memory for an `AllocStats` struct.
#[no_mangle]
pub unsafe extern "C" fn gusset_alloc_stats(out: *mut AllocStats) {
    if !out.is_null() {
        unsafe {
            ptr::write(out, get_alloc_stats());
        }
    }
}

/// 12. Drains pending log messages into a provided buffer.
///
/// # Safety
///
/// `buf` must point to at least `len` writable bytes. `out_written` must point to valid writable memory.
#[no_mangle]
pub unsafe extern "C" fn gusset_drain_logs(
    buf: *mut u8,
    len: usize,
    out_written: *mut usize,
) {
    if buf.is_null() || len == 0 || out_written.is_null() {
        if !out_written.is_null() {
            unsafe {
                ptr::write(out_written, 0);
            }
        }
        return;
    }

    if let Ok(mut log_buf) = LOG_BUFFER.lock() {
        let count = log_buf.len().min(len);
        unsafe {
            ptr::copy_nonoverlapping(log_buf.as_ptr(), buf, count);
            ptr::write(out_written, count);
        }
        log_buf.drain(..count);
    } else {
        unsafe {
            ptr::write(out_written, 0);
        }
    }
}

/// 13. Allocates 64-byte aligned Rust-owned buffer memory (R16).
///
/// # Safety
///
/// `handle`, `out_id`, and `out_ptr` must be valid non-null writable pointers.
#[no_mangle]
pub unsafe extern "C" fn gusset_buf_alloc(
    handle: *mut Handle,
    len: usize,
    out_id: *mut u64,
    out_ptr: *mut *mut u8,
    status: *mut FfiStatus,
) -> i32 {
    if handle.is_null() || out_id.is_null() || out_ptr.is_null() {
        if !status.is_null() {
            unsafe {
                ptr::write(status, FfiStatus::bad_arg("null argument passed to buf_alloc"));
            }
        }
        return FFI_BAD_ARG;
    }

    let h = unsafe { &*handle };
    let res = unsafe {
        ffi_guard(status, || {
            let (id, p) = h.buf_alloc(len)?;
            ptr::write(out_id, id);
            ptr::write(out_ptr, p);
            Ok(())
        })
    };

    if res.is_some() {
        FFI_OK
    } else if !status.is_null() {
        unsafe { (*status).code }
    } else {
        FFI_BAD_ARG
    }
}

/// 14. Frees a Rust-owned buffer by id (R4, R16).
///
/// # Safety
///
/// `handle` must be a valid handle pointer.
#[no_mangle]
pub unsafe extern "C" fn gusset_buf_free(
    handle: *mut Handle,
    id: u64,
    status: *mut FfiStatus,
) -> i32 {
    if handle.is_null() {
        if !status.is_null() {
            unsafe {
                ptr::write(status, FfiStatus::bad_arg("handle is null"));
            }
        }
        return FFI_BAD_ARG;
    }

    let h = unsafe { &*handle };
    let res = unsafe { ffi_guard(status, || h.buf_free(id)) };

    if res.is_some() {
        FFI_OK
    } else if !status.is_null() {
        unsafe { (*status).code }
    } else {
        FFI_BAD_ARG
    }
}
