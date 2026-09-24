//! Exported C ABI symbols for Gusset runtime (R1, R12).
#![allow(unsafe_code)]

pub mod alloc;
mod fields;
pub mod guard;
pub mod status;

use crate::header::CallHeader;
use crate::pool::{Handle, JobResult};
use alloc::{get_alloc_stats, AllocStats};
use guard::{ffi_guard, install_panic_hook, FfiError};
use static_assertions::{assert_eq_align, assert_eq_size};
use status::{FfiStatus, FFI_BAD_ARG, FFI_ERR, FFI_OK, FFI_PANIC, FFI_POISONED};
use std::mem::{align_of, size_of};
use std::ptr;
use std::sync::{Arc, Mutex};

/// Number of `#[repr(C)]` types whose layout crosses the boundary and is verified.
///
/// Every type Go reads or writes through the ABI must appear here. `AllocStats` was
/// missing from version 1: `gusset_alloc_stats` writes it straight into Go memory,
/// so a size or alignment disagreement corrupted the Go heap with nothing to catch
/// it, which is exactly the failure R12/I6 exists to prevent.
pub const ABI_TYPE_COUNT: usize = 4;

/// ABI version and type layout definition for cross-boundary integrity check (R12).
#[repr(C)]
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct AbiLayout {
    /// ABI contract version. Go init() checks for match.
    pub version: u32,
    /// Byte sizes of CallHeader, FfiStatus, AbiLayout, and AllocStats, in that order.
    pub sizes: [u32; ABI_TYPE_COUNT],
    /// Alignments of CallHeader, FfiStatus, AbiLayout, and AllocStats, in that order.
    pub aligns: [u32; ABI_TYPE_COUNT],
}

assert_eq_size!(AbiLayout, [u8; 36]);
assert_eq_align!(AbiLayout, u32);

/// Current Gusset ABI version.
///
/// Bumped to 2 when `AllocStats` joined the verified set and `AbiLayout` itself grew
/// from 28 to 36 bytes. A Go binary built against version 1 refuses to start against
/// this library, which is the intended outcome: its `AbiLayout` is the wrong size.
pub const GUSSET_ABI_VERSION: u32 = 2;

/// Byte budget for the log ring.
const LOG_RING_CAPACITY: usize = 65536;

static LOG_BUFFER: Mutex<Vec<u8>> = Mutex::new(Vec::new());

/// Appends a log line to the bounded internal ring buffer.
///
/// Evicts whole lines from the front until the new line fits, rather than clearing
/// the ring: an overflow used to discard every diagnostic collected so far, which is
/// exactly the moment those diagnostics matter. A line longer than the whole budget
/// is truncated instead of being appended wholesale, which previously let one
/// oversized message push the buffer past its cap without limit.
/// Recovers from lock poisoning and performs a short spin-retry loop if the buffer
/// is contended with `gusset_drain_logs`. Returns if still blocked (avoiding deadlock
/// on re-entrancy from panic hooks).
pub fn log_event(line: &str) {
    let mut buf = match LOG_BUFFER.try_lock() {
        Ok(b) => b,
        Err(std::sync::TryLockError::Poisoned(p)) => p.into_inner(),
        Err(std::sync::TryLockError::WouldBlock) => {
            let mut acquired = None;
            for _ in 0..16 {
                std::hint::spin_loop();
                match LOG_BUFFER.try_lock() {
                    Ok(b) => {
                        acquired = Some(b);
                        break;
                    }
                    Err(std::sync::TryLockError::Poisoned(p)) => {
                        acquired = Some(p.into_inner());
                        break;
                    }
                    Err(std::sync::TryLockError::WouldBlock) => {}
                }
            }
            match acquired {
                Some(b) => b,
                None => return,
            }
        }
    };

    // Reserve one byte for the newline. Truncate on a char boundary so the ring
    // never hands Go a partial UTF-8 sequence.
    let max_payload = LOG_RING_CAPACITY - 1;
    let bytes = if line.len() > max_payload {
        let mut end = max_payload;
        while end > 0 && !line.is_char_boundary(end) {
            end -= 1;
        }
        &line.as_bytes()[..end]
    } else {
        line.as_bytes()
    };
    let needed = bytes.len() + 1;

    while buf.len() + needed > LOG_RING_CAPACITY {
        match buf.iter().position(|&b| b == b'\n') {
            // Drop the oldest complete line, newline included.
            Some(nl) => {
                buf.drain(..=nl);
            }
            // No line boundary left: the remainder is a single partial line.
            None => {
                buf.clear();
                break;
            }
        }
    }

    buf.extend_from_slice(bytes);
    buf.push(b'\n');
}

// ----------------------------------------------------------------------------
// The 15 Exported C Functions (R1)
// ----------------------------------------------------------------------------

/// 1. Exports the ABI layout and sizes of all repr(C) types (R12).
///
/// # Safety
///
/// If non-null, `out` must point to valid writable memory for an `AbiLayout` struct.
#[no_mangle]
pub unsafe extern "C" fn gusset_abi_layout(out: *mut AbiLayout) {
    if !out.is_null() {
        let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            let layout = AbiLayout {
                version: GUSSET_ABI_VERSION,
                sizes: [
                    size_of::<CallHeader>() as u32,
                    size_of::<FfiStatus>() as u32,
                    size_of::<AbiLayout>() as u32,
                    size_of::<AllocStats>() as u32,
                ],
                aligns: [
                    align_of::<CallHeader>() as u32,
                    align_of::<FfiStatus>() as u32,
                    align_of::<AbiLayout>() as u32,
                    align_of::<AllocStats>() as u32,
                ],
            };
            unsafe {
                ptr::write(out, layout);
            }
        }));
    }
}

/// Reports the offset and size of every named field of the four `#[repr(C)]` types.
///
/// Returns the field count. Writes `min(cap, count)` entries into each non-null
/// out pointer and never writes past `cap`, so a caller that passes a short
/// buffer learns the real count without a smash. A null pointer is not written.
/// A count of zero means this call panicked; Go treats that as a mismatch.
///
/// This is a separate export from [`gusset_abi_layout`] so ABI version 2's
/// 36-byte `AbiLayout` stays the size those callers allocate.
///
/// # Safety
///
/// Each non-null pointer must address `cap` writable `u32`s for the duration
/// of the call. Rust does not retain either pointer.
#[no_mangle]
pub unsafe extern "C" fn gusset_abi_fields(offsets: *mut u32, sizes: *mut u32, cap: u32) -> u32 {
    // A zero count cannot be a successful report: the table is non-empty, and
    // Go's init refuses anything other than ABI_FIELD_COUNT. Default is that zero.
    std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        let (off, sz) = fields::layout();
        let take = (cap as usize).min(fields::ABI_FIELD_COUNT);
        unsafe {
            if !offsets.is_null() {
                for (i, value) in off.iter().enumerate().take(take) {
                    ptr::write(offsets.add(i), *value);
                }
            }
            if !sizes.is_null() {
                for (i, value) in sz.iter().enumerate().take(take) {
                    ptr::write(sizes.add(i), *value);
                }
            }
        }
        fields::ABI_FIELD_COUNT as u32
    }))
    .unwrap_or_default()
}

/// 2. Initializes the Gusset runtime and installs panic hook.
///
/// # Safety
///
/// Safe to call across FFI. Modifies global process panic state.
#[no_mangle]
pub unsafe extern "C" fn gusset_init() -> i32 {
    match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        install_panic_hook();
        crate::pool::rearm();
    })) {
        Ok(()) => FFI_OK,
        Err(_) => FFI_ERR,
    }
}

/// 3. Shuts down the Gusset runtime, draining in-flight work.
///
/// Refuses new submissions, cancels every job on every live handle, then waits up
/// to `drain_ms` for the work already running to finish. Returns `FFI_OK` on a
/// clean drain and `FFI_ERR` when work was still in flight at the deadline —
/// cancellation is cooperative, so a work unit that never calls
/// `JobContext::check` cannot be drained, and the caller is told rather than
/// handed a success it can't rely on.
///
/// # Safety
///
/// Safe to call across FFI.
#[no_mangle]
pub unsafe extern "C" fn gusset_shutdown(drain_ms: u32) -> i32 {
    match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        let remaining = crate::pool::shutdown(std::time::Duration::from_millis(drain_ms as u64));
        if remaining == 0 {
            FFI_OK
        } else {
            log_event(&format!(
                "gusset: shutdown drain budget of {} ms expired with {} work unit(s) still in flight",
                drain_ms, remaining
            ));
            FFI_ERR
        }
    })) {
        Ok(code) => code,
        Err(_) => FFI_ERR,
    }
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
pub unsafe extern "C" fn gusset_handle_close(handle: *mut Handle, status: *mut FfiStatus) -> i32 {
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

    // Validate (ptr, len) before a slice exists. `slice::from_raw_parts` with a
    // length past isize::MAX is undefined behaviour — in a debug build its
    // precondition check aborts rather than unwinds, so no firewall below could
    // catch it — and a null pointer with a length used to be read as empty input,
    // running the engine on bytes the caller never sent. A buffer submission
    // carries its input by id, so any inline bytes alongside it are ignored and
    // never touched.
    let input_slice: &[u8] = if buffer_id != 0 || input_len == 0 {
        &[]
    } else if input_ptr.is_null() {
        if !status.is_null() {
            unsafe {
                ptr::write(
                    status,
                    FfiStatus::bad_arg("input_ptr is null with a nonzero input_len"),
                );
            }
        }
        return FFI_BAD_ARG;
    } else if input_len > crate::pool::MAX_INLINE_INPUT {
        if !status.is_null() {
            unsafe {
                ptr::write(
                    status,
                    FfiStatus::bad_arg(
                        "inline input exceeds the 4096-byte copy limit; use a Buffer",
                    ),
                );
            }
        }
        return FFI_BAD_ARG;
    } else {
        unsafe { std::slice::from_raw_parts(input_ptr, input_len) }
    };
    let call_header = unsafe { *header };

    let res = unsafe {
        ffi_guard(status, || {
            let ticket = h.submit(call_header, input_slice, buffer_id)?;
            ptr::write(out_ticket, ticket);
            Ok(())
        })
    };

    if res.is_some() {
        FFI_OK
    } else if h.is_poisoned() {
        // submit() returns Err(String) for the poison latch, which ffi_guard
        // maps to FFI_ERR. R10 is FFI_POISONED without a second reading.
        if !status.is_null() {
            unsafe {
                FfiStatus::overwrite(status, FfiStatus::poisoned("handle is poisoned"));
            }
        }
        FFI_POISONED
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
        ffi_guard(status, || -> Result<(), FfiError> {
            let job_result = h.take(ticket).map_err(FfiError::from)?;
            match job_result {
                JobResult::Ok(data) => {
                    if data.is_empty() {
                        ptr::write(out_buf_id, 0);
                        ptr::write(out_ptr, ptr::null_mut());
                        ptr::write(out_len, 0);
                    } else {
                        let len = data.len();
                        // Not buf_alloc: that refuses a poisoned handle, and this
                        // result already exists — a sibling's panic must not
                        // turn it into an error (I2 refuses new work only).
                        let (buf_id, buf_ptr) = h.buf_from_bytes(&data).map_err(FfiError::from)?;
                        ptr::write(out_buf_id, buf_id);
                        ptr::write(out_ptr, buf_ptr);
                        ptr::write(out_len, len);
                    }
                    Ok(())
                }
                JobResult::Buffer(buf_id) => {
                    let (buf_ptr, len) = h.buf_get(buf_id).map_err(FfiError::from)?;
                    // High bit: "this id is the result's own buffer; Go frees it once
                    // the waiter has consumed it" (R16). Ids stay below 1 << 63, so
                    // the flag loses nothing and Go strips it with &^.
                    ptr::write(out_buf_id, buf_id | crate::pool::TAKE_OWNED_FLAG);
                    ptr::write(out_ptr, buf_ptr);
                    ptr::write(out_len, len);
                    Ok(())
                }
                JobResult::Err(msg) => Err(FfiError {
                    code: FFI_ERR,
                    msg,
                    file: Some("gusset.rs"),
                    line: line!(),
                }),
                JobResult::Panic { msg, file, line } => Err(FfiError {
                    code: FFI_PANIC,
                    msg: format!("PANIC: {}", msg),
                    file,
                    line,
                }),
                JobResult::Cancelled(reason) => Err(FfiError {
                    code: FFI_ERR,
                    msg: format!("cancelled: {:?}", reason),
                    file: Some("gusset.rs"),
                    line: line!(),
                }),
            }
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
pub unsafe extern "C" fn gusset_cancel_all(handle: *mut Handle, status: *mut FfiStatus) -> i32 {
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
        let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| unsafe {
            (*status).free_msg();
        }));
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
        let stats = std::panic::catch_unwind(std::panic::AssertUnwindSafe(get_alloc_stats))
            .unwrap_or(AllocStats {
                live_bytes: 0,
                peak_bytes: 0,
                alloc_count: 0,
            });
        unsafe {
            ptr::write(out, stats);
        }
    }
}

/// 12. Drains pending log messages into a provided buffer.
///
/// # Safety
///
/// `buf` must point to at least `len` writable bytes. `out_written` must point to valid writable memory.
#[no_mangle]
pub unsafe extern "C" fn gusset_drain_logs(buf: *mut u8, len: usize, out_written: *mut usize) {
    if buf.is_null() || len == 0 || out_written.is_null() {
        if !out_written.is_null() {
            unsafe {
                ptr::write(out_written, 0);
            }
        }
        return;
    }

    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        let mut log_buf = LOG_BUFFER.lock().unwrap_or_else(|e| e.into_inner());
        let count = log_buf.len().min(len);
        unsafe {
            ptr::copy_nonoverlapping(log_buf.as_ptr(), buf, count);
        }
        log_buf.drain(..count);
        count
    }));
    unsafe {
        ptr::write(out_written, result.unwrap_or(0));
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
                ptr::write(
                    status,
                    FfiStatus::bad_arg("null argument passed to buf_alloc"),
                );
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

    let res = unsafe {
        ffi_guard(status, || {
            let (id, p) = h.buf_alloc_published(len)?;
            ptr::write(out_id, id);
            ptr::write(out_ptr, p);
            Ok(())
        })
    };

    if res.is_some() {
        FFI_OK
    } else if h.is_poisoned() {
        if !status.is_null() {
            unsafe {
                FfiStatus::overwrite(status, FfiStatus::poisoned("handle is poisoned"));
            }
        }
        FFI_POISONED
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
    let res = unsafe { ffi_guard(status, || h.buf_free(id).map_err(FfiError::from)) };

    if res.is_some() {
        FFI_OK
    } else if !status.is_null() {
        unsafe { (*status).code }
    } else {
        FFI_BAD_ARG
    }
}

#[cfg(test)]
mod abi_field_export_tests {
    use super::*;

    #[test]
    fn null_query_returns_the_count_and_writes_nothing() {
        let n = unsafe { gusset_abi_fields(std::ptr::null_mut(), std::ptr::null_mut(), 0) };
        if n != fields::ABI_FIELD_COUNT as u32 {
            panic!("expected {}, got {n}", fields::ABI_FIELD_COUNT);
        }
        let n = unsafe { gusset_abi_fields(std::ptr::null_mut(), std::ptr::null_mut(), u32::MAX) };
        if n != fields::ABI_FIELD_COUNT as u32 {
            panic!("a huge cap with null pointers wrote or returned {n}");
        }
    }

    #[test]
    fn a_short_cap_does_not_write_past_the_caller_buffer() {
        let mut offsets = [0xFFFF_FFFFu32; 4];
        let mut sizes = [0xFFFF_FFFFu32; 4];
        let n = unsafe { gusset_abi_fields(offsets.as_mut_ptr(), sizes.as_mut_ptr(), 1) };
        if n != fields::ABI_FIELD_COUNT as u32 {
            panic!("expected full count, got {n}");
        }
        // trace_id is the first field: offset 0, size 16. The other three
        // slots of this buffer were not part of the caller's cap.
        if offsets[0] != 0 || sizes[0] != 16 {
            panic!("first field: offset {} size {}", offsets[0], sizes[0]);
        }
        for i in 1..4 {
            if offsets[i] != 0xFFFF_FFFF || sizes[i] != 0xFFFF_FFFF {
                panic!("wrote past cap at index {i}");
            }
        }
    }

    #[test]
    fn a_long_cap_does_not_write_past_the_field_count() {
        let extra = 3usize;
        let mut offsets = vec![0xFFFF_FFFFu32; fields::ABI_FIELD_COUNT + extra];
        let mut sizes = vec![0xFFFF_FFFFu32; fields::ABI_FIELD_COUNT + extra];
        let cap = (fields::ABI_FIELD_COUNT + extra) as u32;
        let n = unsafe { gusset_abi_fields(offsets.as_mut_ptr(), sizes.as_mut_ptr(), cap) };
        if n != fields::ABI_FIELD_COUNT as u32 {
            panic!("expected {}, got {n}", fields::ABI_FIELD_COUNT);
        }
        let (want_off, want_sz) = fields::layout();
        if offsets[..fields::ABI_FIELD_COUNT] != want_off {
            panic!("offsets diverged from layout()");
        }
        if sizes[..fields::ABI_FIELD_COUNT] != want_sz {
            panic!("sizes diverged from layout()");
        }
        for i in 0..extra {
            let at = fields::ABI_FIELD_COUNT + i;
            if offsets[at] != 0xFFFF_FFFF || sizes[at] != 0xFFFF_FFFF {
                panic!("wrote past the field count at {at}");
            }
        }
    }
}
