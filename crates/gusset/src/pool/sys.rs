//! Low-level system interfaces: sigaltstack, pipe writing, and buffer allocation (R8, R16).
#![allow(unsafe_code)]

use crate::alloc::{record_alloc, record_dealloc};
use std::alloc::Layout;
use std::io::{Error, ErrorKind, Result};

/// Hard ceiling on a single Rust-owned buffer.
///
/// `NewBuffer` and take-side promotion feed a caller-controlled length into the
/// allocator. An unbounded length is an unbounded address-space request. 1 GiB
/// is well above the 200 MiB memlimit assertion and the 64 KiB egress benches;
/// anything larger belongs in Phase 4 isolation, not in-process.
pub const MAX_BUFFER_BYTES: usize = 1 << 30;

/// Refuses a zero or oversized buffer length without touching the allocator.
pub fn check_buffer_len(len: usize) -> std::result::Result<(), String> {
    if len == 0 {
        return Err("buffer length must be greater than zero".to_string());
    }
    if len > MAX_BUFFER_BYTES {
        return Err(format!(
            "buffer length {} exceeds maximum {} bytes",
            len, MAX_BUFFER_BYTES
        ));
    }
    Ok(())
}

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
        check_buffer_len(len)?;
        let layout =
            Layout::from_size_align(len, 64).map_err(|e| format!("invalid layout: {}", e))?;
        let ptr = unsafe { std::alloc::alloc(layout) };
        if ptr.is_null() {
            return Err("allocation failed".to_string());
        }
        record_alloc(len);
        Ok(Self { ptr, len, layout })
    }

    /// Copies `src` into a newly allocated buffer.
    ///
    /// Used to promote a large `JobResult::Ok` onto a `Buffer` on the worker
    /// thread so `gusset_take` is a pointer return rather than a memcpy on the
    /// cgo thread (R16 egress).
    pub fn from_bytes(src: &[u8]) -> std::result::Result<Self, String> {
        let buf = Self::allocate(src.len())?;
        unsafe {
            std::ptr::copy_nonoverlapping(src.as_ptr(), buf.ptr, src.len());
        }
        Ok(buf)
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
/// I5/R8 claim a worker runs on an explicit 8 MiB stack rather than whatever it
/// would otherwise inherit. A `thread::Builder` without `stack_size` gets Rust's
/// std default of 2 MiB (overridable by `RUST_MIN_STACK`), not the platform's
/// pthread default — which is 128 KiB on musl, 512 KiB for secondary threads on
/// darwin, and on glibc whatever `RLIMIT_STACK` says (commonly 8 MiB, but 2 MiB
/// on most architectures when that limit is unlimited). A deep-recursion probe
/// that fits in 2 MiB therefore passes whether or not Gusset set the size.
/// Reading the size back off the thread distinguishes the two on every platform,
/// which is what makes R8 testable without a musl host.
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
pub(crate) const WRITE_TICKET_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(10);

/// Upper bound on the backoff sleep between retries on a full pipe.
pub(crate) const WRITE_TICKET_MAX_BACKOFF: std::time::Duration =
    std::time::Duration::from_millis(8);

/// One non-blocking attempt to write an 8-byte ticket.
///
/// Returns [`ErrorKind::WouldBlock`] when the pipe is full and no byte of this
/// ticket has been committed, so the caller can sleep without holding the
/// exclusivity lock. A short write is finished inside this call: releasing the
/// lock between the two halves would let another ticket interleave and break
/// framing for every later completion.
pub fn write_ticket_attempt(fd: i32, ticket: u64) -> Result<()> {
    if fd < 0 {
        return Err(Error::new(
            ErrorKind::InvalidInput,
            "invalid file descriptor",
        ));
    }

    let bytes = ticket.to_ne_bytes();
    let mut remaining = &bytes[..];
    let mut committed = false;
    let mut partial_spins = 0u32;

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
                Some(code) if code == libc::EAGAIN || code == libc::EWOULDBLOCK => {
                    if !committed {
                        return Err(Error::new(ErrorKind::WouldBlock, "completion pipe full"));
                    }
                    partial_spins = partial_spins.saturating_add(1);
                    if partial_spins > 10_000 {
                        return Err(Error::new(
                            ErrorKind::TimedOut,
                            "torn completion write; pipe stayed full after a short write",
                        ));
                    }
                    std::thread::yield_now();
                    continue;
                }
                _ => return Err(err),
            }
        } else if n == 0 {
            return Err(Error::new(ErrorKind::WriteZero, "pipe write zero bytes"));
        } else {
            committed = true;
            partial_spins = 0;
            remaining = &remaining[n as usize..];
        }
    }

    Ok(())
}

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
    let mut backoff = std::time::Duration::from_micros(50);
    let mut blocked_since: Option<std::time::Instant> = None;

    loop {
        match write_ticket_attempt(fd, ticket) {
            Ok(()) => return Ok(()),
            Err(err) if err.kind() == ErrorKind::WouldBlock => {
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
            }
            Err(err) => return Err(err),
        }
    }
}

/// Sets the given file descriptor to non-blocking mode (O_NONBLOCK).
pub fn set_nonblocking(fd: i32) -> Result<()> {
    if fd < 0 {
        return Err(Error::new(
            ErrorKind::InvalidInput,
            "invalid file descriptor",
        ));
    }
    unsafe {
        let flags = libc::fcntl(fd, libc::F_GETFL);
        if flags < 0 {
            return Err(Error::last_os_error());
        }
        if libc::fcntl(fd, libc::F_SETFL, flags | libc::O_NONBLOCK) < 0 {
            return Err(Error::last_os_error());
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn check_buffer_len_refuses_zero_and_the_ceiling_without_allocating() {
        match check_buffer_len(0) {
            Ok(()) => panic!("zero length must be refused"),
            Err(e) => assert!(e.contains("greater than zero"), "got: {}", e),
        }
        match check_buffer_len(MAX_BUFFER_BYTES) {
            Ok(()) => {}
            Err(e) => panic!("exactly MAX_BUFFER_BYTES must be accepted, got: {}", e),
        }
        match check_buffer_len(MAX_BUFFER_BYTES.saturating_add(1)) {
            Ok(()) => panic!("MAX_BUFFER_BYTES + 1 must be refused without allocating"),
            Err(e) => assert!(e.contains("maximum"), "got: {}", e),
        }
    }

    #[test]
    fn allocate_refuses_above_maximum_without_touching_the_allocator() {
        match RawBuffer::allocate(MAX_BUFFER_BYTES.saturating_add(1)) {
            Ok(_) => panic!("allocate must refuse before posix_memalign"),
            Err(e) => assert!(e.contains("maximum"), "got: {}", e),
        }
    }
}
