//! FfiStatus and status code definitions for cross-boundary error handling.

use static_assertions::{assert_eq_align, assert_eq_size};
use std::ptr;

/// FFI operation succeeded.
pub const FFI_OK: i32 = 0;
/// Generic error reported by engine.
pub const FFI_ERR: i32 = 1;
/// Rust panic caught by firewall (handle is poisoned).
pub const FFI_PANIC: i32 = 2;
/// Handle is poisoned due to a previous caught panic; all calls fail fast.
pub const FFI_POISONED: i32 = 3;
/// Invalid argument passed across boundary (e.g. invalid UTF-8, null pointer).
pub const FFI_BAD_ARG: i32 = 4;

/// ABI status struct returned across FFI (R3).
///
/// Strings and buffers cross as `(ptr, len)`. No CString and no NUL termination.
#[repr(C)]
pub struct FfiStatus {
    /// Return code: FFI_OK, FFI_ERR, FFI_PANIC, FFI_POISONED, or FFI_BAD_ARG.
    pub code: i32,
    /// Pointer to UTF-8 error or panic message allocated by Rust.
    pub msg: *mut u8,
    /// Byte length of message.
    pub msg_len: usize,
    /// Static string pointer to source file where error or panic occurred.
    pub file: *const u8,
    /// Byte length of file name.
    pub file_len: usize,
    /// Source line number where error or panic occurred.
    pub line: u32,
}

#[cfg(target_pointer_width = "64")]
assert_eq_size!(FfiStatus, [u8; 48]);
assert_eq_align!(FfiStatus, usize);

impl FfiStatus {
    /// Constructs an empty OK status.
    pub fn ok() -> Self {
        Self {
            code: FFI_OK,
            msg: ptr::null_mut(),
            msg_len: 0,
            file: ptr::null(),
            file_len: 0,
            line: 0,
        }
    }

    /// Constructs an error status with an allocated message buffer.
    pub fn new_err(code: i32, msg: &[u8], file: *const u8, file_len: usize, line: u32) -> Self {
        if msg.is_empty() {
            Self {
                code,
                msg: ptr::null_mut(),
                msg_len: 0,
                file,
                file_len,
                line,
            }
        } else {
            let boxed = msg.to_vec().into_boxed_slice();
            let msg_len = boxed.len();
            let msg = Box::into_raw(boxed) as *mut u8;
            Self {
                code,
                msg,
                msg_len,
                file,
                file_len,
                line,
            }
        }
    }

    /// Constructs a BAD_ARG error status.
    pub fn bad_arg(msg: &str) -> Self {
        Self::new_err(
            FFI_BAD_ARG,
            msg.as_bytes(),
            c"ffi.rs".as_ptr() as *const u8,
            6,
            line!(),
        )
    }

    /// Constructs a POISONED error status.
    pub fn poisoned(msg: &str) -> Self {
        Self::new_err(
            FFI_POISONED,
            msg.as_bytes(),
            c"handle.rs".as_ptr() as *const u8,
            9,
            line!(),
        )
    }

    /// Cleans up any message allocation owned by this status (R4).
    pub fn free_msg(&mut self) {
        if !self.msg.is_null() && self.msg_len > 0 {
            unsafe {
                let slice_ptr = ptr::slice_from_raw_parts_mut(self.msg, self.msg_len);
                drop(Box::from_raw(slice_ptr));
            }
            self.msg = ptr::null_mut();
            self.msg_len = 0;
        }
    }
}
