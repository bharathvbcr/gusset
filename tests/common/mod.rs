//! Helpers shared by every Rust test binary in this workspace.
//!
//! Each integration test is its own crate, so a private helper cannot be
//! shared and each file grew its own copy: eight `make_pipe`s, four ticket
//! readers in three shapes, three `Must` traits and three repo-file readers.
//! A fix to one copy (a bounded wait, close-on-exec) silently missed the
//! rest. This is the one definition. It is a directory, so Cargo never builds
//! it as a test binary of its own, and every binary still runs in its own
//! process.
//!
//! The workspace-root `tests/` files reach it as `mod common;`. The others
//! name it with `#[path]`: `crates/gusset/tests/*.rs`,
//! `crates/gusset-example/tests/*.rs`, and the pool unit tests in
//! `crates/gusset/src/pool/tests/mod.rs`. A binary uses only some of these
//! helpers, hence `dead_code`. The lib's `deny(unsafe_code)` reaches the copy
//! the unit tests compile, hence `unsafe_code`.

#![allow(dead_code, unsafe_code)]

use std::path::PathBuf;
use std::time::{Duration, Instant};

/// How long [`read_ticket`] waits for a completion before failing the test.
///
/// A regression that loses a ticket then fails with a message instead of
/// hanging the binary until CI's job timeout.
pub const TICKET_WAIT: Duration = Duration::from_secs(5);

/// `expect` without tripping the crate's R3 clippy ban on it.
pub trait Must<T> {
    /// The value, or a panic naming `msg` and the failure.
    fn must(self, msg: &str) -> T;
}

impl<T, E: std::fmt::Debug> Must<T> for Result<T, E> {
    fn must(self, msg: &str) -> T {
        match self {
            Ok(v) => v,
            Err(e) => panic!("{msg}: {e:?}"),
        }
    }
}

impl<T> Must<T> for Option<T> {
    fn must(self, msg: &str) -> T {
        match self {
            Some(v) => v,
            None => panic!("{msg}: None"),
        }
    }
}

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

/// Reads one completion ticket, failing the test if none arrives within
/// [`TICKET_WAIT`] or the pipe closes first.
pub fn read_ticket(fd: i32) -> u64 {
    match read_ticket_within(fd, TICKET_WAIT) {
        Some(t) => t,
        None => panic!("no completion ticket within {TICKET_WAIT:?}, or the pipe closed"),
    }
}

/// Reads one 8-byte completion ticket, or `None` once `limit` passes without
/// one or the pipe reports end-of-file or an error.
///
/// Loops rather than taking a single `read`: the 8 bytes are atomic under
/// `PIPE_BUF`, but a short read is still permitted by the interface and a test
/// that assumed otherwise would fail rarely and confusingly. A failed or
/// interrupted `poll` retries until the deadline rather than failing at once.
pub fn read_ticket_within(fd: i32, limit: Duration) -> Option<u64> {
    let deadline = Instant::now() + limit;
    let mut buf = [0u8; 8];
    let mut got = 0usize;
    while got < buf.len() {
        let left = deadline.saturating_duration_since(Instant::now());
        if left.is_zero() {
            return None;
        }
        let mut pfd = libc::pollfd {
            fd,
            events: libc::POLLIN,
            revents: 0,
        };
        let ms = left.as_millis().min(i32::MAX as u128) as i32;
        // SAFETY: `pfd` is one valid pollfd for the duration of the call.
        let rc = unsafe { libc::poll(&mut pfd, 1, ms.max(1)) };
        if rc <= 0 {
            continue;
        }
        // SAFETY: reading into a valid stack buffer from the pipe's read end.
        let n = unsafe {
            libc::read(
                fd,
                buf.as_mut_ptr().add(got) as *mut libc::c_void,
                buf.len() - got,
            )
        };
        if n <= 0 {
            return None;
        }
        got += n as usize;
    }
    Some(u64::from_ne_bytes(buf))
}

/// The workspace root.
///
/// Every binary that includes this module is a `[[test]]` of a crate two
/// levels down (`crates/<name>`), wherever its source file lives, so
/// `CARGO_MANIFEST_DIR` is that crate.
pub fn repo_root() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../..")
}

/// Reads a file named relative to the workspace root, failing the test with
/// the full path if it cannot.
pub fn read_repo_file(rel: &str) -> String {
    let full = repo_root().join(rel);
    match std::fs::read_to_string(&full) {
        Ok(s) => s,
        Err(e) => panic!("cannot read {}: {}", full.display(), e),
    }
}
