//! Helpers shared by this crate's integration tests.
//!
//! Every file in `tests/` is its own crate, so three of them carried
//! byte-identical copies of the pipe setup and the ticket read — an exact clone
//! group, and one that grew by a third copy the last time a test was added here.
//! This is the `mod common` form Cargo provides for the case: a directory, so it
//! is not itself compiled as a fourth test binary.

/// A POSIX pipe for completion tickets, as `(read_fd, write_fd)`.
///
/// The write end is handed to `Handle::open`, which takes ownership of it and
/// closes it in `close()`. The read end stays with the caller, which must close
/// it itself.
pub fn make_pipe() -> (i32, i32) {
    let mut fds = [0i32; 2];
    // SAFETY: `fds` is a valid two-element array for pipe(2) to fill.
    let rc = unsafe { libc::pipe(fds.as_mut_ptr()) };
    assert_eq!(rc, 0, "pipe() failed");
    (fds[0], fds[1])
}

/// Reads one completion ticket, so the worker never blocks writing it.
///
/// Loops rather than taking a single `read`: the 8 bytes are atomic under
/// `PIPE_BUF`, but a short read is still permitted by the interface and a test
/// that assumed otherwise would fail rarely and confusingly.
pub fn read_ticket(fd: i32) -> u64 {
    let mut buf = [0u8; 8];
    let mut got = 0usize;
    while got < buf.len() {
        // SAFETY: reading into a valid stack buffer from the pipe's read end.
        let n = unsafe {
            libc::read(
                fd,
                buf.as_mut_ptr().add(got) as *mut libc::c_void,
                buf.len() - got,
            )
        };
        assert!(n > 0, "completion pipe read failed");
        got += n as usize;
    }
    u64::from_ne_bytes(buf)
}
