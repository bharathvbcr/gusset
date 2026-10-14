//! Dispatch cost of the diagnostic engine versus a registered handler.
//!
//! Two comparisons, both through the public pool API (no new C export, and this
//! binary is not a staticlib):
//!
//! * opcode 0 on the diagnostic engine versus opcode 0 on a handler registered
//!   with [`set_engine_handler`]. [`register_engine`] refuses opcode 0 because
//!   dispatch never consults the registry for it.
//! * opcode 0 on the diagnostic engine versus a nonzero opcode registered with
//!   [`register_engine`].
//!
//! ```text
//! cargo bench -p gusset --bench dispatch --no-run
//! ```

use gusset::header::{
    CallHeader, JobContext, GUSSET_FLAG_DIAGNOSTIC_ENGINE, GUSSET_FLAG_INLINE_COMPLETION,
};
use gusset::pool::{Handle, INLINE_RECORD_FLAG};
use gusset::{clear_engine_handlers, register_engine, set_engine_handler};
use std::time::{Duration, Instant};

const ITERS: u32 = 1000;

fn fail(msg: &str) -> ! {
    eprintln!("dispatch bench: {msg}");
    std::process::exit(1);
}

fn open_pipe() -> (i32, i32) {
    let mut fds = [0i32; 2];
    // SAFETY: pipe(2) writes two open descriptors into this array.
    let rc = unsafe { libc::pipe(fds.as_mut_ptr()) };
    if rc != 0 {
        fail("pipe failed");
    }
    // SAFETY: fds[0] is a read end this process just created.
    let flags = unsafe { libc::fcntl(fds[0], libc::F_GETFL) };
    if flags < 0 {
        fail("fcntl F_GETFL failed");
    }
    // SAFETY: same descriptor; only O_NONBLOCK is added.
    let set = unsafe { libc::fcntl(fds[0], libc::F_SETFL, flags | libc::O_NONBLOCK) };
    if set < 0 {
        fail("fcntl O_NONBLOCK failed");
    }
    (fds[0], fds[1])
}

fn read_full(fd: i32, buf: &mut [u8]) -> bool {
    let mut got = 0usize;
    let mut spins = 0u32;
    while got < buf.len() {
        // SAFETY: `buf[got..]` is a valid writable slice and `fd` is the pipe read end.
        let n = unsafe {
            libc::read(
                fd,
                buf[got..].as_mut_ptr().cast::<libc::c_void>(),
                buf.len() - got,
            )
        };
        if n > 0 {
            got += n as usize;
            spins = 0;
            continue;
        }
        if n == 0 {
            return false;
        }
        let err = std::io::Error::last_os_error().raw_os_error();
        if err == Some(libc::EINTR) || err == Some(libc::EAGAIN) || err == Some(libc::EWOULDBLOCK) {
            spins = spins.saturating_add(1);
            if spins > 100_000_000 {
                return false;
            }
            std::hint::spin_loop();
            continue;
        }
        return false;
    }
    true
}

fn drain_one(h: &Handle, fd: i32, ticket: u64) {
    let mut word = [0u8; 8];
    if !read_full(fd, &mut word) {
        fail("completion read failed");
    }
    let first = u64::from_ne_bytes(word);
    if first & INLINE_RECORD_FLAG == 0 {
        if first != ticket {
            fail("bare ticket mismatch");
        }
        if let Err(err) = h.take(ticket) {
            fail(&err);
        }
        return;
    }
    if first & !INLINE_RECORD_FLAG != ticket {
        fail("inline ticket mismatch");
    }
    if !read_full(fd, &mut word) {
        fail("inline length read failed");
    }
    let len = u64::from_ne_bytes(word) as usize;
    if len != 1 {
        fail("expected a 1-byte echo");
    }
    let mut body = [0u8; 8];
    if !read_full(fd, &mut body) {
        fail("inline body read failed");
    }
    if body[0] != 0 {
        fail("echo mismatch");
    }
}

fn echo(_ctx: &JobContext, input: &[u8]) -> Result<Vec<u8>, String> {
    Ok(input.to_vec())
}

fn time_roundtrips(h: &Handle, fd: i32, header: CallHeader) -> Duration {
    let payload = [0u8];
    for _ in 0..32 {
        let ticket = match h.submit(header, &payload, 0) {
            Ok(ticket) => ticket,
            Err(err) => fail(&err),
        };
        drain_one(h, fd, ticket);
    }
    let started = Instant::now();
    for _ in 0..ITERS {
        let ticket = match h.submit(header, &payload, 0) {
            Ok(ticket) => ticket,
            Err(err) => fail(&err),
        };
        drain_one(h, fd, ticket);
    }
    started.elapsed()
}

fn report(name: &str, elapsed: Duration) {
    let ns = elapsed.as_nanos() / u128::from(ITERS);
    println!("{name}\t{ITERS}\t{ns} ns/op");
}

fn main() {
    let (read_fd, write_fd) = open_pipe();
    let handle = match Handle::open(1, write_fd) {
        Ok(handle) => handle,
        Err(err) => fail(&err),
    };
    let inline = GUSSET_FLAG_INLINE_COMPLETION;
    let diagnostic = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE | inline,
        ..CallHeader::default()
    };
    let opcode0 = CallHeader {
        flags: inline,
        ..CallHeader::default()
    };
    let opcode_nonzero = CallHeader {
        flags: inline,
        reserved: 7,
        ..CallHeader::default()
    };

    let diagnostic_opcode0 = time_roundtrips(&handle, read_fd, diagnostic);
    set_engine_handler(echo);
    let registered_opcode0 = time_roundtrips(&handle, read_fd, opcode0);
    clear_engine_handlers();

    if let Err(err) = register_engine(7, echo) {
        fail(&err);
    }
    let opcode0_again = time_roundtrips(&handle, read_fd, diagnostic);
    let registered_nonzero = time_roundtrips(&handle, read_fd, opcode_nonzero);
    clear_engine_handlers();

    if let Err(err) = handle.close() {
        fail(&err);
    }
    // SAFETY: read_fd is the pipe read end from open_pipe, still open.
    unsafe {
        libc::close(read_fd);
    }

    report("dispatch/diagnostic_opcode0", diagnostic_opcode0);
    report("dispatch/registered_opcode0", registered_opcode0);
    report("dispatch/opcode0", opcode0_again);
    report("dispatch/registered_nonzero", registered_nonzero);
}
