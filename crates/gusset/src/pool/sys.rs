//! Low-level system interfaces: sigaltstack, pipe writing, and buffer allocation (R8, R16).
#![allow(unsafe_code)]

#[cfg(gusset_allocator_api)]
use crate::alloc::BufferAlloc;
use crate::alloc::{count_buffer_alloc, count_buffer_dealloc, BUFFER_ALIGN};
use std::alloc::Layout;
use std::alloc::{GlobalAlloc, System};
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
///
/// `len` is what Go sees; `layout.size()` is what was allocated. They differ only
/// for an adopted [`BufferAlloc`] vector, whose spare capacity is kept rather
/// than paid for with a shrinking realloc.
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
    ///
    /// Zero-filled. `NewBuffer` hands this memory to Go, and an engine may
    /// return a buffer it allocated without writing every byte; uninitialized
    /// memory there exposed whatever the process last freed at that address
    /// (a previous request's payload, a key) to a caller that never wrote it.
    /// Go's own `make` zeroes, so callers reasonably assumed this did too.
    pub fn allocate(len: usize) -> std::result::Result<Self, String> {
        Self::allocate_with(len, true)
    }

    fn allocate_with(len: usize, zeroed: bool) -> std::result::Result<Self, String> {
        check_buffer_len(len)?;
        let layout = Layout::from_size_align(len, BUFFER_ALIGN)
            .map_err(|e| format!("invalid layout: {}", e))?;
        // System, not the global allocator: buffer memory is counted by Gusset
        // alone (see count_buffer_alloc), so it must not also pass through an
        // installed Counting.
        let ptr = unsafe {
            if zeroed {
                System.alloc_zeroed(layout)
            } else {
                System.alloc(layout)
            }
        };
        if ptr.is_null() {
            return Err("allocation failed".to_string());
        }
        count_buffer_alloc(len);
        Ok(Self { ptr, len, layout })
    }

    /// Copies `src` into a newly allocated buffer.
    ///
    /// Used to promote a large `JobResult::Ok` onto a `Buffer` on the worker
    /// thread so `gusset_take` is a pointer return rather than a memcpy on the
    /// cgo thread (R16 egress).
    pub fn from_bytes(src: &[u8]) -> std::result::Result<Self, String> {
        // Every byte is overwritten below, so zeroing first would be waste.
        let buf = Self::allocate_with(src.len(), false)?;
        unsafe {
            std::ptr::copy_nonoverlapping(src.as_ptr(), buf.ptr, src.len());
        }
        Ok(buf)
    }

    /// Takes ownership of a `Vec<u8, BufferAlloc>` without copying its bytes.
    ///
    /// The vector's memory was allocated by `BufferAlloc` as
    /// `(capacity, BUFFER_ALIGN)` from `System` and counted once — exactly what
    /// [`RawBuffer::allocate`] does — so `Drop` below frees and uncounts it with
    /// the same layout and the accounting stays balanced.
    ///
    /// Hands the vector back when it cannot be adopted as-is: empty (possibly a
    /// dangling pointer), over [`MAX_BUFFER_BYTES`], or not 64-byte aligned.
    /// The last can only happen if unsafe engine code built the vector with
    /// `Vec::from_raw_parts_in` over foreign memory; refusing it keeps Go's
    /// alignment guarantee and keeps a mismatched layout away from `dealloc`.
    #[cfg(gusset_allocator_api)]
    pub fn adopt(v: Vec<u8, BufferAlloc>) -> std::result::Result<Self, Vec<u8, BufferAlloc>> {
        // Capacity is capped as well as length: a 1-byte result in a 4 GiB
        // allocation would otherwise pin 4 GiB for as long as Go holds it.
        // Refused here, the caller copies the bytes out and frees the rest.
        if v.is_empty() || v.len() > MAX_BUFFER_BYTES || v.capacity() > MAX_BUFFER_BYTES {
            return Err(v);
        }
        if !(v.as_ptr() as usize).is_multiple_of(BUFFER_ALIGN) {
            return Err(v);
        }
        let layout = match Layout::from_size_align(v.capacity(), BUFFER_ALIGN) {
            Ok(l) => l,
            Err(_) => return Err(v),
        };
        // BufferAlloc is a zero-sized Copy type, so forgetting the vector forgets
        // nothing but the allocation this RawBuffer now owns.
        let mut v = std::mem::ManuallyDrop::new(v);
        Ok(Self {
            ptr: v.as_mut_ptr(),
            len: v.len(),
            layout,
        })
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
                System.dealloc(self.ptr, self.layout);
            }
            count_buffer_dealloc(self.layout.size());
        }
    }
}

/// Blocks SIGPIPE in the calling thread's signal mask. Returns false on failure.
///
/// Thread-scoped on purpose: the process-wide disposition belongs to the host
/// (Go's runtime owns it), and only writes from Gusset's own workers need the
/// EPIPE-instead-of-signal behaviour.
pub fn block_sigpipe_on_this_thread() -> bool {
    unsafe {
        let mut set: libc::sigset_t = std::mem::zeroed();
        libc::sigemptyset(&mut set);
        libc::sigaddset(&mut set, libc::SIGPIPE);
        libc::pthread_sigmask(libc::SIG_BLOCK, &set, std::ptr::null_mut()) == 0
    }
}

/// RAII guard that disables sigaltstack and frees the allocated stack memory on thread exit (R8).
pub struct SigAltStackGuard {
    /// Base of the mapping, guard page included.
    map_base: *mut libc::c_void,
    /// Length of the mapping, guard page included.
    map_len: usize,
}

impl Drop for SigAltStackGuard {
    fn drop(&mut self) {
        if !self.map_base.is_null() {
            unsafe {
                // Unmap only if the kernel no longer points at this mapping.
                // SS_DISABLE fails with EPERM while running on the alternate
                // stack, and something else may have installed its own since;
                // unmapping in either case would leave the next signal on this
                // thread running on unmapped memory. Leaking is the safe side.
                let mut ss: libc::stack_t = std::mem::zeroed();
                ss.ss_flags = libc::SS_DISABLE;
                let disabled = libc::sigaltstack(&ss, std::ptr::null_mut()) == 0;
                let mut cur: libc::stack_t = std::mem::zeroed();
                let read = libc::sigaltstack(std::ptr::null(), &mut cur) == 0;
                let base = self.map_base as usize;
                let ours =
                    (cur.ss_sp as usize) >= base && (cur.ss_sp as usize) < base + self.map_len;
                let still_installed = read && (cur.ss_flags & libc::SS_DISABLE) == 0 && ours;
                if disabled && read && !still_installed {
                    libc::munmap(self.map_base, self.map_len);
                }
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

/// Floor for the alternate signal stack: twice Go's own 32 KiB gsignal stack.
pub const SIGALTSTACK_MIN: usize = 64 * 1024;

/// Size of each worker's alternate signal stack.
///
/// A fixed 64 KiB was chosen when signal frames were a few KiB. On arm64 with
/// SVE/SME and on x86-64 with AVX-512 plus AMX the kernel's frame can approach
/// or pass that, and a frame that does not fit is delivered as SIGSEGV — the
/// exact failure the alternate stack exists to prevent. Linux reports the real
/// minimum in `AT_MINSIGSTKSZ`; four times it (glibc's `sysconf(_SC_SIGSTKSZ)`
/// rule) leaves room for the handler itself.
///
/// What it buys: a signal delivered to a worker (Go's preemption and profiling
/// signals included) runs its handler on this stack rather than on a stack that
/// may be nearly exhausted, so Go's handler can run at all. It does not make a
/// Rust stack overflow survivable — Go re-raises a foreign thread's SIGSEGV with
/// the default action, so the process still exits.
pub fn sigaltstack_size() -> usize {
    #[cfg(target_os = "linux")]
    {
        let min = unsafe { libc::getauxval(libc::AT_MINSIGSTKSZ) } as usize;
        // The frame plus room for Go's handler, which needs ~32 KiB below it
        // (needm on a thread Go did not create).
        SIGALTSTACK_MIN
            .max(min.saturating_mul(4))
            .max(min.saturating_add(32 * 1024))
    }
    #[cfg(not(target_os = "linux"))]
    {
        SIGALTSTACK_MIN.max(libc::SIGSTKSZ)
    }
}

/// Installs an alternate signal stack (at least 64 KiB) on the current OS thread.
///
/// Go requires SA_ONSTACK handlers to have an alternate stack on threads it did
/// not create; see [`sigaltstack_size`] for what that does and does not buy.
pub fn install_sigaltstack() -> Option<SigAltStackGuard> {
    unsafe {
        let page = match libc::sysconf(libc::_SC_PAGESIZE) {
            p if p > 0 => p as usize,
            _ => 4096,
        };
        let stack_size = sigaltstack_size().div_ceil(page) * page;
        // One PROT_NONE page below the stack: a signal frame that overruns it
        // faults instead of silently corrupting whatever malloc put there.
        let map_len = stack_size + page;
        let map_base = libc::mmap(
            std::ptr::null_mut(),
            map_len,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_PRIVATE | libc::MAP_ANON,
            -1,
            0,
        );
        if map_base == libc::MAP_FAILED {
            return None;
        }
        if libc::mprotect(map_base, page, libc::PROT_NONE) != 0 {
            libc::munmap(map_base, map_len);
            return None;
        }

        let mut ss: libc::stack_t = std::mem::zeroed();
        ss.ss_sp = (map_base as *mut u8).add(page) as *mut libc::c_void;
        ss.ss_size = stack_size;
        ss.ss_flags = 0;

        if libc::sigaltstack(&ss, std::ptr::null_mut()) != 0 {
            libc::munmap(map_base, map_len);
            None
        } else {
            Some(SigAltStackGuard { map_base, map_len })
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

/// Makes sure the pipe behind `fd` can hold `bytes` unread bytes.
///
/// Linux only: the capacity is adjustable there and can be as small as one
/// page. macOS has no interface to query or grow a pipe; it normally grows
/// pipe buffers on demand (16–64 KiB), but under kernel memory pressure can
/// hand out 512 bytes, 64 tickets. A pool larger than that on macOS can then
/// see completion writes wait for the reader. They are delayed, never lost:
/// the worker retries until the reader drains or the handle closes, and Go's
/// reader keeps draining during Close. A descriptor that is not a pipe reports EBADF/EINVAL from
/// F_GETPIPE_SZ and is left alone — tests hand in other descriptor kinds, and
/// the ownership contract for those is checked elsewhere.
pub fn ensure_pipe_capacity(fd: i32, bytes: usize) -> std::result::Result<(), String> {
    #[cfg(target_os = "linux")]
    unsafe {
        let cur = libc::fcntl(fd, libc::F_GETPIPE_SZ);
        if cur < 0 || cur as usize >= bytes {
            return Ok(());
        }
        let want = match libc::c_int::try_from(bytes) {
            Ok(w) => w,
            Err(_) => {
                return Err(format!(
                    "completion pipe capacity {} is not representable",
                    bytes
                ))
            }
        };
        if libc::fcntl(fd, libc::F_SETPIPE_SZ, want) < 0 {
            return Err(format!(
                "completion pipe holds {} bytes but this pool needs {} for its unread tickets, \
                 and growing it failed: {} (lower the pool size or raise \
                 /proc/sys/fs/pipe-user-pages-soft)",
                cur,
                bytes,
                Error::last_os_error()
            ));
        }
    }
    #[cfg(not(target_os = "linux"))]
    let _ = (fd, bytes);
    Ok(())
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

/// Closes a file descriptor.
///
/// Called once, never retried. On Linux the descriptor is released even when
/// close(2) reports EINTR, so a retry could close a number another thread had
/// just been given for an unrelated file — the double close the descriptor
/// ownership rules elsewhere exist to prevent. (POSIX leaves the state after
/// EINTR unspecified; not retrying can at worst leak one descriptor, which is
/// the safe side.)
pub fn close_fd(fd: i32) {
    if fd >= 0 {
        unsafe {
            libc::close(fd);
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

    /// The allocator and adoption paths in the lib, so the nightly Miri job
    /// (which runs `--lib` only) checks them for UB. No accounting assertions:
    /// unit tests run in parallel and share the process-global counters;
    /// exact accounting lives in the `rust_allocator_api` binary.
    #[cfg(gusset_allocator_api)]
    #[test]
    fn buffer_alloc_growth_shrink_and_adoption_are_sound() {
        use crate::alloc::{BufferAlloc, Counting};
        use std::alloc::System;

        let mut v: Vec<u8, BufferAlloc> = Vec::new_in(BufferAlloc);
        for i in 0..300u32 {
            v.push(i as u8);
            assert_eq!(v.as_ptr() as usize % BUFFER_ALIGN, 0);
        }
        v.truncate(100);
        v.shrink_to_fit();
        assert_eq!(v.as_ptr() as usize % BUFFER_ALIGN, 0);
        let ptr = v.as_ptr();

        let buf = match RawBuffer::adopt(v) {
            Ok(b) => b,
            Err(_) => panic!("an aligned, non-empty BufferAlloc vector must be adopted"),
        };
        assert_eq!(buf.as_mut_ptr() as *const u8, ptr, "adoption must not copy");
        assert!(buf
            .as_slice()
            .iter()
            .copied()
            .eq((0..100u32).map(|i| i as u8)));
        drop(buf);

        // Spare capacity is adopted too, and freed with the full layout.
        let mut v: Vec<u8, BufferAlloc> = Vec::with_capacity_in(4096, BufferAlloc);
        v.extend_from_slice(b"abc");
        match RawBuffer::adopt(v) {
            Ok(b) => assert_eq!(b.as_slice(), b"abc"),
            Err(_) => panic!("a vector with spare capacity must be adopted"),
        }

        // Empty vectors are handed back, never adopted over a dangling pointer.
        let empty: Vec<u8, BufferAlloc> = Vec::with_capacity_in(64, BufferAlloc);
        assert!(RawBuffer::adopt(empty).is_err());
        assert!(RawBuffer::adopt(Vec::new_in(BufferAlloc)).is_err());

        // Counting over System and over BufferAlloc, through every resize path.
        let mut c: Vec<u64, Counting<System>> = Vec::new_in(Counting::new(System));
        c.extend(0..1000u64);
        c.truncate(3);
        c.shrink_to_fit();
        assert_eq!(c.as_slice(), &[0, 1, 2]);
        let mut d: Vec<u8, Counting<BufferAlloc>> = Vec::new_in(Counting::new(BufferAlloc));
        d.resize(5000, 7);
        d.shrink_to_fit();
        assert_eq!(d.as_ptr() as usize % BUFFER_ALIGN, 0);
    }

    #[test]
    fn allocate_refuses_above_maximum_without_touching_the_allocator() {
        match RawBuffer::allocate(MAX_BUFFER_BYTES.saturating_add(1)) {
            Ok(_) => panic!("allocate must refuse before posix_memalign"),
            Err(e) => assert!(e.contains("maximum"), "got: {}", e),
        }
    }
}
