//! Exported C ABI symbols for Gusset runtime (R1, R12).
#![allow(unsafe_code)]

pub mod alloc;
mod fields;
pub mod guard;
pub mod status;

use crate::header::CallHeader;
use crate::pool::ring::Ring;
use crate::pool::{Handle, JobResult};
use alloc::{get_alloc_stats, AllocStats};
use guard::{ffi_guard_code, install_panic_hook, FfiError};
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

/// Lines `log_event` gave up on because the ring stayed locked.
///
/// The lines lost that way are the ones that matter most ("WITHOUT
/// sigaltstack", "completion write failed", "drain budget expired"), and they
/// used to vanish with no trace. The count is reported on the next line that
/// does get in.
static LOG_DROPPED: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);

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
                None => {
                    LOG_DROPPED.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                    return;
                }
            }
        }
    };

    let dropped = LOG_DROPPED.swap(0, std::sync::atomic::Ordering::Relaxed);
    if dropped > 0 {
        let note = format!(
            "gusset: {} log line(s) dropped while the ring was busy",
            dropped
        );
        append_line(&mut buf, &note);
    }
    append_line(&mut buf, line);
}

/// Appends one line to the ring, evicting whole lines from the front.
fn append_line(buf: &mut Vec<u8>, line: &str) {
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

/// Passes a handle-bound export's failure code through, poisoning the handle
/// when it is `FFI_PANIC` (I2).
///
/// The engine's own firewall poisons in the worker, but a panic the export's
/// guard catches — anything unwinding out of the handle's own code on the
/// caller's thread — was reported as `FFI_PANIC` with the latch left clear, so
/// the next submit entered Rust again. Only `gusset_take` could already see
/// `FFI_PANIC` for an engine panic, whose handle is poisoned; storing again is
/// idempotent.
fn poison_on_panic(h: &Handle, code: i32) -> i32 {
    if code == FFI_PANIC {
        h.poison();
    }
    code
}

// ----------------------------------------------------------------------------
// The 17 Exported C Functions (R1)
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
        ffi_guard_code(status, || {
            let handle = Handle::open(pool_size, pipe_write_fd)?;
            let raw = Arc::into_raw(handle) as *mut Handle;
            ptr::write(out_handle, raw);
            Ok(())
        })
    };

    match res {
        Ok(()) => FFI_OK,
        Err(code) => code,
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
        ffi_guard_code(status, || {
            let arc = Arc::from_raw(handle);
            // On a timed-out join, close keeps another Arc on the joiner.
            // Dropping this one then does not free the pool under a worker
            // that is still inside an engine call.
            let joined = arc.close();
            drop(arc);
            joined.map_err(FfiError::from)
        })
    };

    match res {
        Ok(()) => FFI_OK,
        Err(code) => code,
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
        ffi_guard_code(status, || {
            #[cfg(test)]
            fault::trip();
            let ticket = h.submit(call_header, input_slice, buffer_id)?;
            ptr::write(out_ticket, ticket);
            Ok(())
        })
    };

    match res {
        Ok(()) => FFI_OK,
        // The panic's own report comes first; later calls see FFI_POISONED.
        Err(FFI_PANIC) => poison_on_panic(h, FFI_PANIC),
        Err(_) if h.is_poisoned() => {
            // submit() returns Err(String) for the poison latch, which ffi_guard
            // maps to FFI_ERR. R10 is FFI_POISONED without a second reading.
            if !status.is_null() {
                unsafe {
                    FfiStatus::overwrite(status, FfiStatus::poisoned("handle is poisoned"));
                }
            }
            FFI_POISONED
        }
        Err(code) => code,
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
        ffi_guard_code(status, || -> Result<(), FfiError> {
            #[cfg(test)]
            fault::trip();
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
                    // A cancel flag set by gusset_shutdown reads as Explicit to
                    // the engine. Reported that way, Go mapped it to
                    // context.Canceled although the caller's context was live;
                    // "Shutdown" lets it surface as ErrShutdown instead.
                    msg: if reason == crate::header::CancelReason::Explicit
                        && crate::pool::is_shutting_down()
                    {
                        "cancelled: Shutdown".to_string()
                    } else {
                        format!("cancelled: {:?}", reason)
                    },
                    file: Some("gusset.rs"),
                    line: line!(),
                }),
            }
        })
    };

    match res {
        Ok(()) => FFI_OK,
        Err(code) => poison_on_panic(h, code),
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
        ffi_guard_code(status, || {
            #[cfg(test)]
            fault::trip();
            h.cancel(ticket);
            Ok(())
        })
    };

    match res {
        Ok(()) => FFI_OK,
        Err(code) => poison_on_panic(h, code),
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
        ffi_guard_code(status, || {
            #[cfg(test)]
            fault::trip();
            h.cancel_all();
            Ok(())
        })
    };

    match res {
        Ok(()) => FFI_OK,
        Err(code) => poison_on_panic(h, code),
    }
}

/// 9a. Attaches a shared-memory completion ring to the handle.
///
/// From this call on, completions are published into the ring and the pipe
/// carries only wake tokens (8 zero bytes, "ticket 0") and overflow records.
/// The layout and the reader's protocol are in `gusset.h` (`GUSSET_RING_*`).
///
/// On success `*out_ring` is an owning reference: the ring memory stays valid,
/// even after `gusset_handle_close`, until it is passed to
/// `gusset_ring_release`, which the reader calls once it has stopped
/// reading. `*out_shared` and `*out_slots` point at the header and the first
/// slot; `*out_capacity` is the slot count, a power of two at least the pool
/// size. A second attach on the same handle is refused.
///
/// # Safety
///
/// `handle` must be a valid handle pointer, and every out pointer valid for a
/// write.
#[no_mangle]
pub unsafe extern "C" fn gusset_handle_ring(
    handle: *mut Handle,
    out_ring: *mut *const Ring,
    out_shared: *mut *const u8,
    out_slots: *mut *const u8,
    out_capacity: *mut u64,
    status: *mut FfiStatus,
) -> i32 {
    if handle.is_null()
        || out_ring.is_null()
        || out_shared.is_null()
        || out_slots.is_null()
        || out_capacity.is_null()
    {
        if !status.is_null() {
            unsafe {
                ptr::write(status, FfiStatus::bad_arg("null handle or out pointer"));
            }
        }
        return FFI_BAD_ARG;
    }

    let h = unsafe { &*handle };
    let res = unsafe {
        ffi_guard_code(status, || {
            #[cfg(test)]
            fault::trip();
            let ring = h.attach_ring()?;
            ptr::write(out_shared, ring.shared() as *const _ as *const u8);
            ptr::write(out_slots, ring.slots_ptr() as *const u8);
            ptr::write(out_capacity, ring.shared().capacity);
            ptr::write(out_ring, Arc::into_raw(ring));
            Ok(())
        })
    };

    match res {
        Ok(()) => FFI_OK,
        Err(code) => poison_on_panic(h, code),
    }
}

/// 9b. Drops the reference `gusset_handle_ring` returned. NULL is a no-op.
///
/// # Safety
///
/// `ring` must be NULL or a pointer from `gusset_handle_ring` not yet
/// released, and nothing may read the ring afterwards.
#[no_mangle]
pub unsafe extern "C" fn gusset_ring_release(ring: *const Ring) {
    if ring.is_null() {
        return;
    }
    // Dropping never unwinds: the ring holds only atomics and two boxes.
    drop(unsafe { Arc::from_raw(ring) });
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
/// Hands over whole lines while they fit; a single line longer than `len` is
/// split on a UTF-8 character boundary. A partial fill
/// does not mean the ring is empty; a caller flushing it drains until
/// `*out_written` is 0.
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
        let count = drain_cut(&log_buf, len);
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

/// How many bytes of `ring` a drain into `cap` bytes hands over.
///
/// Everything, when it fits. Otherwise up to the last whole line that fits,
/// or, for a single line longer than `cap`, up to the last character boundary.
/// Cutting at `cap` split lines across drains and characters across chunks,
/// breaking `append_line`'s promise that Go never sees a partial UTF-8
/// sequence. A `cap` smaller than the first character still hands over `cap`
/// raw bytes: returning 0 would read as "empty" and strand the ring.
fn drain_cut(ring: &[u8], cap: usize) -> usize {
    let count = ring.len().min(cap);
    if count == ring.len() {
        return count;
    }
    if let Some(nl) = ring[..count].iter().rposition(|&b| b == b'\n') {
        return nl + 1;
    }
    let mut cut = count;
    // `ring[cut]` exists: count < ring.len(). A UTF-8 continuation byte is 10xxxxxx.
    while cut > 0 && ring[cut] & 0xC0 == 0x80 {
        cut -= 1;
    }
    if cut == 0 {
        count
    } else {
        cut
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
        ffi_guard_code(status, || {
            #[cfg(test)]
            fault::trip();
            let (id, p) = h.buf_alloc_published(len)?;
            ptr::write(out_id, id);
            ptr::write(out_ptr, p);
            Ok(())
        })
    };

    match res {
        Ok(()) => FFI_OK,
        // The panic's own report comes first; later calls see FFI_POISONED.
        Err(FFI_PANIC) => poison_on_panic(h, FFI_PANIC),
        Err(_) if h.is_poisoned() => {
            if !status.is_null() {
                unsafe {
                    FfiStatus::overwrite(status, FfiStatus::poisoned("handle is poisoned"));
                }
            }
            FFI_POISONED
        }
        Err(code) => code,
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
    let res = unsafe {
        ffi_guard_code(status, || {
            #[cfg(test)]
            fault::trip();
            h.buf_free(id).map_err(FfiError::from)
        })
    };

    match res {
        Ok(()) => FFI_OK,
        Err(code) => poison_on_panic(h, code),
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

#[cfg(test)]
mod drain_cut_tests {
    use super::drain_cut;

    #[test]
    fn a_cut_prefers_lines_then_characters_and_always_progresses() {
        let ring = "ab\n€€\n".as_bytes();
        assert_eq!(drain_cut(ring, 64), ring.len(), "everything that fits");
        assert_eq!(drain_cut(ring, 5), 3, "the last whole line that fits");
        // "€€\n" alone, cap 4: no newline, and byte 4 is mid-character.
        let tail = "€€\n".as_bytes();
        assert_eq!(drain_cut(tail, 4), 3, "back to the character boundary");
        assert_eq!(drain_cut(tail, 1), 1, "smaller than a character: raw bytes");
        assert_eq!(
            drain_cut(&tail[1..], 1),
            1,
            "starting mid-character still progresses"
        );
        assert_eq!(drain_cut(&[], 8), 0);
    }
}

/// Test-only fault point inside the guarded body of every handle-bound export.
///
/// Nothing Gusset itself does in those bodies panics on a stable toolchain, but
/// adopter code can run there: a nightly `std::thread::add_spawn_hook` runs in
/// the parent during `Builder::spawn`, which `submit` reaches when it respawns a
/// dead worker. Without injection a regression test could only assert the
/// success path. Armed per thread, so parallel tests cannot consume each
/// other's fault.
#[cfg(test)]
mod fault {
    use std::sync::Mutex;
    use std::thread::{self, ThreadId};

    /// The thread whose next guarded export body panics (R7 keeps this out of
    /// `thread_local!`; the test calls the export on the arming thread).
    static ARMED_BY: Mutex<Option<ThreadId>> = Mutex::new(None);

    /// Makes the next guarded export body on this thread panic, once.
    pub(super) fn arm() {
        *ARMED_BY.lock().unwrap_or_else(|e| e.into_inner()) = Some(thread::current().id());
    }

    pub(super) fn trip() {
        let mut armed = ARMED_BY.lock().unwrap_or_else(|e| e.into_inner());
        if *armed == Some(thread::current().id()) {
            *armed = None;
            drop(armed);
            panic!("injected fault inside a guarded export");
        }
    }
}

/// I2: a panic the firewall catches inside a handle-bound export poisons the
/// handle, exactly as a panic inside the engine does. The guard reported
/// `FFI_PANIC` and left the latch clear, so the next `gusset_submit` and
/// `gusset_buf_alloc` entered Rust again. A C host (and Go's `NewBuffer`,
/// `Cancel` and `BufFree`, which do not latch on `ErrPanic`) had nothing
/// else telling it to stop.
#[cfg(test)]
mod export_panic_poisons_tests {
    use super::*;

    fn open() -> (Arc<Handle>, i32) {
        let mut fds = [0i32; 2];
        // SAFETY: `fds` is a valid two-element array for pipe(2) to fill.
        assert_eq!(unsafe { libc::pipe(fds.as_mut_ptr()) }, 0, "pipe() failed");
        match Handle::open(1, fds[1]) {
            Ok(h) => (h, fds[0]),
            Err(e) => panic!("open failed: {e}"),
        }
    }

    fn submit(raw: *mut Handle, st: &mut FfiStatus) -> i32 {
        let header = CallHeader::default();
        let mut ticket = 0u64;
        let input = [0u8; 1];
        // SAFETY: `raw` is live for the call; every pointer is local.
        unsafe { gusset_submit(raw, &header, input.as_ptr(), 1, 0, &mut ticket, st) }
    }

    type Call = fn(*mut Handle, &mut FfiStatus) -> i32;

    fn take(raw: *mut Handle, st: &mut FfiStatus) -> i32 {
        let (mut id, mut p, mut n) = (0u64, ptr::null_mut(), 0usize);
        // SAFETY: as in `submit`.
        unsafe { gusset_take(raw, 1, &mut id, &mut p, &mut n, st) }
    }

    fn cancel(raw: *mut Handle, st: &mut FfiStatus) -> i32 {
        // SAFETY: as in `submit`.
        unsafe { gusset_cancel(raw, 1, st) }
    }

    fn cancel_all(raw: *mut Handle, st: &mut FfiStatus) -> i32 {
        // SAFETY: as in `submit`.
        unsafe { gusset_cancel_all(raw, st) }
    }

    fn buf_alloc(raw: *mut Handle, st: &mut FfiStatus) -> i32 {
        let (mut id, mut p) = (0u64, ptr::null_mut());
        // SAFETY: as in `submit`.
        unsafe { gusset_buf_alloc(raw, 64, &mut id, &mut p, st) }
    }

    fn buf_free(raw: *mut Handle, st: &mut FfiStatus) -> i32 {
        // SAFETY: as in `submit`.
        unsafe { gusset_buf_free(raw, 1, st) }
    }

    fn handle_ring(raw: *mut Handle, st: &mut FfiStatus) -> i32 {
        let mut ring: *const Ring = ptr::null();
        let (mut sh, mut sl, mut cap) = (ptr::null(), ptr::null(), 0u64);
        // SAFETY: as in `submit`. The ring is released only if handed out.
        unsafe {
            let rc = gusset_handle_ring(raw, &mut ring, &mut sh, &mut sl, &mut cap, st);
            gusset_ring_release(ring);
            rc
        }
    }

    #[test]
    #[cfg_attr(miri, ignore)]
    fn a_panic_caught_in_any_handle_export_poisons_the_handle() {
        let calls: [(&str, Call); 7] = [
            ("gusset_submit", submit),
            ("gusset_take", take),
            ("gusset_cancel", cancel),
            ("gusset_cancel_all", cancel_all),
            ("gusset_buf_alloc", buf_alloc),
            ("gusset_buf_free", buf_free),
            ("gusset_handle_ring", handle_ring),
        ];

        for (name, call) in calls {
            let (handle, r) = open();
            let raw = Arc::as_ptr(&handle) as *mut Handle;
            let mut st = FfiStatus::ok();

            fault::arm();
            let rc = call(raw, &mut st);
            let code = st.code;
            // SAFETY: the export initialised `st`; every free below likewise.
            unsafe { gusset_status_free(&mut st) };
            assert_eq!(
                rc, FFI_PANIC,
                "{name}: an injected panic must report FFI_PANIC first"
            );
            assert_eq!(
                code, FFI_PANIC,
                "{name}: the status must carry the returned code"
            );
            assert!(
                handle.is_poisoned(),
                "{name}: a panic caught at the boundary left the handle unpoisoned (I2)"
            );

            let rc = submit(raw, &mut st);
            unsafe { gusset_status_free(&mut st) };
            assert_eq!(rc, FFI_POISONED, "{name}: a later submit must fail fast");
            let rc = buf_alloc(raw, &mut st);
            unsafe { gusset_status_free(&mut st) };
            assert_eq!(rc, FFI_POISONED, "{name}: a later buf_alloc must fail fast");

            assert!(handle.close().is_ok());
            // SAFETY: `r` is the read end this test opened.
            unsafe { libc::close(r) };
        }
    }
}
