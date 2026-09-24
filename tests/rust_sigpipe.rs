//! A completion write to a pipe whose read end is gone must not kill the host.
//!
//! On a thread Go did not create, SIGPIPE from `write(2)` reaches Go's handler,
//! which re-raises it with the default action: the whole process exits with
//! status 141. Workers now block SIGPIPE, so the write fails with EPIPE, the
//! failure is logged, and the handle refuses further work.
//!
//! Separate binary: the Rust test harness sets SIGPIPE to SIG_IGN at startup
//! (as every Rust executable does), which hides the bug; this test restores
//! the default action a Go host has for foreign threads, and that is process-
//! wide.

use gusset::header::{CallHeader, GUSSET_FLAG_DIAGNOSTIC_ENGINE};
use gusset::pool::Handle;
use std::time::{Duration, Instant};

#[test]
fn a_closed_completion_pipe_does_not_kill_the_process() {
    unsafe { libc::signal(libc::SIGPIPE, libc::SIG_DFL) };

    let mut fds = [0i32; 2];
    assert_eq!(unsafe { libc::pipe(fds.as_mut_ptr()) }, 0);
    let (r, w) = (fds[0], fds[1]);
    let h = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open: {e}"),
    };
    unsafe { libc::close(r) };

    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    if let Err(e) = h.submit(header, &[0, 1, 2], 0) {
        panic!("submit: {e}");
    }

    // Still alive after the write: the handle latches the broken pipe and
    // refuses new work rather than accepting jobs it can never complete.
    let start = Instant::now();
    while !h.is_poisoned() && start.elapsed() < Duration::from_secs(5) {
        std::thread::sleep(Duration::from_millis(10));
    }
    assert!(
        h.is_poisoned(),
        "a broken completion pipe must refuse new work"
    );
    assert!(h.submit(header, &[0], 0).is_err());
    h.close();
}
