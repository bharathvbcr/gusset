//! Low-level system interfaces: sigaltstack, pipe writing, and buffer allocation (R8, R16).
#![allow(unsafe_code)]

use std::alloc::Layout;
use std::io::{Error, ErrorKind, Result};

/// Raw buffer allocated in Rust memory with 64-byte alignment (R16).
#[derive(Debug)]
pub struct RawBuffer {
    ptr: *mut u8,
    len: usize,
    layout: Layout,
}

unsafe impl Send for RawBuffer {}
unsafe impl Sync for RawBuffer {}

impl RawBuffer {
    /// Allocates 64-byte aligned memory.
    pub fn allocate(len: usize) -> std::result::Result<Self, String> {
        if len == 0 {
            return Err("buffer length must be greater than zero".to_string());
        }
        let layout = Layout::from_size_align(len, 64)
            .map_err(|e| format!("invalid layout: {}", e))?;
        let ptr = unsafe { std::alloc::alloc(layout) };
        if ptr.is_null() {
            return Err("allocation failed".to_string());
        }
        Ok(Self { ptr, len, layout })
    }

    /// Returns the raw pointer.
    pub fn as_mut_ptr(&self) -> *mut u8 {
        self.ptr
    }

    /// Returns the byte length.
    pub fn len(&self) -> usize {
        self.len
    }

    /// Checks if the buffer is empty.
    pub fn is_empty(&self) -> bool {
        self.len == 0
    }

    /// Copies buffer bytes into a new Vec<u8>.
    pub fn to_vec(&self) -> Vec<u8> {
        unsafe { std::slice::from_raw_parts(self.ptr, self.len).to_vec() }
    }
}

impl Drop for RawBuffer {
    fn drop(&mut self) {
        if !self.ptr.is_null() {
            unsafe {
                std::alloc::dealloc(self.ptr, self.layout);
            }
        }
    }
}

/// Installs a 64 KiB alternate signal stack on the current OS thread.
///
/// Go requires SA_ONSTACK from foreign threads; a foreign thread without
/// an alternate signal stack dies on stack overflow before Go's handler runs.
pub fn install_sigaltstack() {
    const STACK_SIZE: usize = 65536; // 64 KiB
    unsafe {
        let stack_ptr = libc::malloc(STACK_SIZE);
        if stack_ptr.is_null() {
            return;
        }

        let mut ss: libc::stack_t = std::mem::zeroed();
        ss.ss_sp = stack_ptr;
        ss.ss_size = STACK_SIZE;
        ss.ss_flags = 0;

        let ret = libc::sigaltstack(&ss, std::ptr::null_mut());
        if ret != 0 {
            libc::free(stack_ptr);
        }
    }
}

/// Writes an 8-byte ticket to the pipe file descriptor with EINTR/EAGAIN retries.
///
/// POSIX guarantees atomic writes for payloads up to PIPE_BUF (>= 512 bytes).
pub fn write_ticket(fd: i32, ticket: u64) -> Result<()> {
    if fd < 0 {
        return Err(Error::new(ErrorKind::InvalidInput, "invalid file descriptor"));
    }

    let bytes = ticket.to_ne_bytes();
    let mut remaining = &bytes[..];

    while !remaining.is_empty() {
        let n = unsafe {
            libc::write(
                fd,
                remaining.as_ptr() as *const libc::c_void,
                remaining.len(),
            )
        };

        if n < 0 {
            let err = Error::last_os_error();
            match err.raw_os_error() {
                Some(libc::EINTR) => continue,
                Some(libc::EAGAIN) => {
                    std::thread::yield_now();
                    continue;
                }
                _ => return Err(err),
            }
        } else if n == 0 {
            return Err(Error::new(ErrorKind::WriteZero, "pipe write zero bytes"));
        } else {
            remaining = &remaining[n as usize..];
        }
    }

    Ok(())
}

/// Closes a file descriptor safely with EINTR retry.
pub fn close_fd(fd: i32) {
    if fd >= 0 {
        unsafe {
            loop {
                let ret = libc::close(fd);
                if ret != 0 && Error::last_os_error().raw_os_error() == Some(libc::EINTR) {
                    continue;
                }
                break;
            }
        }
    }
}
