//! Low-level system interfaces: sigaltstack, pipe writing, and buffer allocation (R8, R16).
#![allow(unsafe_code)]

use crate::alloc::{record_alloc, record_dealloc};
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
        let layout =
            Layout::from_size_align(len, 64).map_err(|e| format!("invalid layout: {}", e))?;
        let ptr = unsafe { std::alloc::alloc(layout) };
        if ptr.is_null() {
            return Err("allocation failed".to_string());
        }
        record_alloc(len);
        Ok(Self { ptr, len, layout })
    }

    /// Returns the raw pointer.
    pub fn as_mut_ptr(&self) -> *mut u8 {
        self.ptr
    }

    /// Returns a slice view of the buffer memory.
    pub fn as_slice(&self) -> &[u8] {
        unsafe { std::slice::from_raw_parts(self.ptr, self.len) }
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
            record_dealloc(self.len);
        }
    }
}

/// RAII guard that disables sigaltstack and frees the allocated stack memory on thread exit (R8).
pub struct SigAltStackGuard {
    stack_ptr: *mut libc::c_void,
}

impl Drop for SigAltStackGuard {
    fn drop(&mut self) {
        if !self.stack_ptr.is_null() {
            unsafe {
                let mut ss: libc::stack_t = std::mem::zeroed();
                ss.ss_flags = libc::SS_DISABLE;
                let _ = libc::sigaltstack(&ss, std::ptr::null_mut());
                libc::free(self.stack_ptr);
            }
        }
    }
}

/// Reports the calling OS thread's stack size, or `None` where the platform
/// cannot be asked.
///
/// I5/R8 claim a worker runs on an explicit 8 MiB stack rather than the pthread
/// default, which is 128 KiB on musl. On glibc and darwin the default is already
/// 8 MiB, so a deep-recursion probe there passes whether or not Gusset set the
/// size — it proves the platform, not the runtime. Reading the size back off the
/// thread distinguishes the two on every platform, which is what makes R8
/// testable without a musl host.
pub fn current_thread_stack_size() -> Option<usize> {
    #[cfg(target_vendor = "apple")]
    unsafe {
        // Darwin has no pthread_getattr_np; this is the documented equivalent and
        // returns the usable stack size for the calling thread.
        let size = libc::pthread_get_stacksize_np(libc::pthread_self());
        if size == 0 {
            None
        } else {
            Some(size)
        }
    }

    #[cfg(all(unix, not(target_vendor = "apple")))]
    unsafe {
        // glibc and musl both provide pthread_getattr_np. The attr owns a cpuset
        // allocation on glibc, so it must be destroyed even on the success path.
        let mut attr: libc::pthread_attr_t = std::mem::zeroed();
        if libc::pthread_getattr_np(libc::pthread_self(), &mut attr) != 0 {
            return None;
        }
        let mut size: libc::size_t = 0;
        let rc = libc::pthread_attr_getstacksize(&attr, &mut size);
        libc::pthread_attr_destroy(&mut attr);
        if rc != 0 || size == 0 {
            None
        } else {
            Some(size)
        }
    }
}

/// Installs a 64 KiB alternate signal stack on the current OS thread.
///
/// Go requires SA_ONSTACK from foreign threads; a foreign thread without
/// an alternate signal stack dies on stack overflow before Go's handler runs.
pub fn install_sigaltstack() -> Option<SigAltStackGuard> {
    const STACK_SIZE: usize = 65536; // 64 KiB
    unsafe {
        let stack_ptr = libc::malloc(STACK_SIZE);
        if stack_ptr.is_null() {
            return None;
        }

        let mut ss: libc::stack_t = std::mem::zeroed();
        ss.ss_sp = stack_ptr;
        ss.ss_size = STACK_SIZE;
        ss.ss_flags = 0;

        let ret = libc::sigaltstack(&ss, std::ptr::null_mut());
        if ret != 0 {
            libc::free(stack_ptr);
            None
        } else {
            Some(SigAltStackGuard { stack_ptr })
        }
    }
}

/// Longest a completion write will wait for a full pipe before giving up.
///
/// Bounded concurrency keeps unread tickets under `MAX_POOL_SIZE` (8 KiB of ticket
/// bytes), which every supported platform's pipe buffer holds, so a full pipe means
/// the reader has stalled. Waiting forever there would hang `Handle::close` in
/// `join`; this bound turns that deadlock into a reported error.
const WRITE_TICKET_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(10);

/// Upper bound on the backoff sleep between retries on a full pipe.
const WRITE_TICKET_MAX_BACKOFF: std::time::Duration = std::time::Duration::from_millis(8);

/// Writes an 8-byte ticket to the pipe file descriptor with EINTR/EAGAIN retries.
///
/// POSIX guarantees atomic writes for payloads up to PIPE_BUF (>= 512 bytes).
///
/// `EINTR` is retried without limit: Go's async preemption (`SIGURG`) interrupts
/// syscalls on this thread constantly and makes no progress claim either way.
/// `EAGAIN` means the pipe is genuinely full, so it backs off exponentially instead
/// of spinning `yield_now` at 100% CPU, and gives up once [`WRITE_TICKET_TIMEOUT`]
/// has elapsed.
pub fn write_ticket(fd: i32, ticket: u64) -> Result<()> {
    if fd < 0 {
        return Err(Error::new(
            ErrorKind::InvalidInput,
            "invalid file descriptor",
        ));
    }

    let bytes = ticket.to_ne_bytes();
    let mut remaining = &bytes[..];
    let mut backoff = std::time::Duration::from_micros(50);
    let mut blocked_since: Option<std::time::Instant> = None;

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
                    let since = *blocked_since.get_or_insert_with(std::time::Instant::now);
                    if since.elapsed() >= WRITE_TICKET_TIMEOUT {
                        return Err(Error::new(
                            ErrorKind::TimedOut,
                            format!(
                                "completion pipe full for {:?}; reader is not draining",
                                WRITE_TICKET_TIMEOUT
                            ),
                        ));
                    }
                    std::thread::sleep(backoff);
                    backoff = (backoff * 2).min(WRITE_TICKET_MAX_BACKOFF);
                    continue;
                }
                _ => return Err(err),
            }
        } else if n == 0 {
            return Err(Error::new(ErrorKind::WriteZero, "pipe write zero bytes"));
        } else {
            // Progress: reset the stall window so a slow-but-live reader is not
            // timed out by the cumulative wait of earlier partial writes.
            blocked_since = None;
            backoff = std::time::Duration::from_micros(50);
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
