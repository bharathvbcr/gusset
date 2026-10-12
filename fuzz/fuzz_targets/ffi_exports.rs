//! The raw C exports, driven the way a hostile or buggy host would: the input
//! bytes decode into a sequence of calls with adversarial lengths, ids,
//! tickets, null pointers, a second ring attach and refused opens. No call may
//! unwind, abort or touch memory it was not given (run under ASan, see
//! `fuzz/Cargo.toml`), and every call must return one of the documented codes.
//!
//! What it never does is break the host contract the exports document: a
//! handle pointer is used only while it is live and closed exactly once (they
//! are unauthenticated, `DECISIONS.md` 2026-10-12), every non-null pointer is
//! valid for the length claimed, and an `FfiStatus` is freed once. Everything
//! else (ids and tickets that never existed, lengths past every limit,
//! NULL where NULL is documented as refused, descriptors that are not a pipe
//! write end) is fair game.
//!
//! One handle (pool of 2) per input, opened and closed through the exports. A
//! panic that poisons it ends that handle's life and the next call opens a
//! fresh one, so a poisoned handle cannot hide the rest of the sequence.
#![no_main]
#![allow(unsafe_code)]

use gusset::ffi::status::{FfiStatus, FFI_BAD_ARG, FFI_ERR, FFI_OK, FFI_PANIC, FFI_POISONED};
use gusset::ffi::{
    gusset_abi_fields, gusset_abi_layout, gusset_alloc_stats, gusset_buf_alloc, gusset_buf_free,
    gusset_cancel, gusset_cancel_all, gusset_drain_logs, gusset_handle_close, gusset_handle_open,
    gusset_handle_ring, gusset_ring_release, gusset_status_free, gusset_submit, gusset_take,
    AbiLayout,
};
use gusset::header::{CallHeader, GUSSET_FLAG_DIAGNOSTIC_ENGINE, GUSSET_FLAG_INLINE_COMPLETION};
use gusset::pool::ring::Ring;
use gusset::pool::MAX_POOL_SIZE;
use gusset::{AllocStats, Handle};
use libfuzzer_sys::fuzz_target;
use std::ptr;

/// Calls per input. Bounds the work and keeps the completion pipe (at least
/// 64 KiB on every supported OS) from filling: each call completes at most one
/// record of 64 bytes.
const MAX_OPS: usize = 48;
/// Largest buffer the sequence really allocates: a hostile length is tried
/// separately, and a valid one stays small enough for libFuzzer's RSS limit.
const MAX_REAL_BUFFER: usize = 1 << 20;
const TAKE_OWNED_FLAG: u64 = 1 << 63;

/// Reads the input as a stream of bytes, yielding zero once it runs out so a
/// short input still decodes (to a run of the first operation).
struct Cursor<'a> {
    data: &'a [u8],
    at: usize,
}

impl<'a> Cursor<'a> {
    fn u8(&mut self) -> u8 {
        let b = self.data.get(self.at).copied().unwrap_or(0);
        self.at += 1;
        b
    }
    fn u32(&mut self) -> u32 {
        u32::from_le_bytes([self.u8(), self.u8(), self.u8(), self.u8()])
    }
    fn u64(&mut self) -> u64 {
        (u64::from(self.u32()) << 32) | u64::from(self.u32())
    }
    fn done(&self) -> bool {
        self.at >= self.data.len()
    }
}

fn pipe() -> (i32, i32) {
    let mut fds = [0i32; 2];
    // SAFETY: fds is a valid two-element array.
    assert_eq!(unsafe { libc::pipe(fds.as_mut_ptr()) }, 0, "pipe failed");
    (fds[0], fds[1])
}

fn close_fd(fd: i32) {
    // SAFETY: the descriptor is one this target created and still owns.
    unsafe {
        libc::close(fd);
    }
}

/// A live handle and what the host holds on it.
struct Rig {
    handle: *mut Handle,
    read_fd: i32,
    ring: *const Ring,
    tickets: Vec<u64>,
    buffers: Vec<u64>,
}

/// Every code an export may return.
fn documented(code: i32) -> bool {
    matches!(
        code,
        FFI_OK | FFI_ERR | FFI_PANIC | FFI_POISONED | FFI_BAD_ARG
    )
}

/// Runs `call` with one of: a real status, or NULL (documented as optional),
/// then checks the code and frees the status exactly once. A failure must
/// leave a message that a host can read; a success must leave none.
fn with_status(null_status: bool, call: impl FnOnce(*mut FfiStatus) -> i32) -> i32 {
    if null_status {
        let code = call(ptr::null_mut());
        assert!(documented(code), "undocumented code {code}");
        return code;
    }
    let mut st = FfiStatus::ok();
    let code = call(&mut st);
    assert!(documented(code), "undocumented code {code}");
    if code != FFI_OK {
        assert_eq!(st.code, code, "status and return code disagree");
        if !st.msg.is_null() {
            // SAFETY: msg/msg_len were written together by the export.
            let msg = unsafe { std::slice::from_raw_parts(st.msg, st.msg_len) };
            assert!(
                std::str::from_utf8(msg).is_ok(),
                "status message is not UTF-8"
            );
        }
    }
    // SAFETY: st is a status this target owns and frees once.
    unsafe { gusset_status_free(&mut st) };
    code
}

impl Rig {
    fn open() -> Option<Rig> {
        let (r, w) = pipe();
        // SAFETY: r is a valid descriptor.
        unsafe {
            let fl = libc::fcntl(r, libc::F_GETFL);
            libc::fcntl(r, libc::F_SETFL, fl | libc::O_NONBLOCK);
        }
        let mut handle: *mut Handle = ptr::null_mut();
        let code = with_status(false, |st| unsafe {
            gusset_handle_open(2, w, &mut handle, st)
        });
        if code != FFI_OK {
            close_fd(r);
            close_fd(w);
            return None;
        }
        Some(Rig {
            handle,
            read_fd: r,
            ring: ptr::null(),
            tickets: Vec::new(),
            buffers: Vec::new(),
        })
    }

    fn close(self) {
        if !self.ring.is_null() {
            // SAFETY: a ring from gusset_handle_ring, released once.
            unsafe { gusset_ring_release(self.ring) };
        }
        // SAFETY: the handle is live and closed exactly once, here.
        let code = with_status(false, |st| unsafe { gusset_handle_close(self.handle, st) });
        assert!(documented(code));
        close_fd(self.read_fd);
    }

    /// A ticket: one this rig issued, or one it never did.
    fn ticket(&self, c: &mut Cursor) -> u64 {
        let pick = c.u8() as usize;
        match (c.u8() % 4, self.tickets.is_empty()) {
            (0, _) | (_, true) => c.u64(),
            (1, false) => 0,
            _ => self.tickets[pick % self.tickets.len()],
        }
    }

    /// A buffer id: one this rig holds, or one it never did.
    fn buffer_id(&self, c: &mut Cursor) -> u64 {
        let pick = c.u8() as usize;
        match (c.u8() % 4, self.buffers.is_empty()) {
            (0, _) | (_, true) => c.u64(),
            (1, false) => 0,
            _ => self.buffers[pick % self.buffers.len()],
        }
    }

    fn step(&mut self, c: &mut Cursor, payload: &[u8]) {
        let op = c.u8() % 13;
        let null_status = c.u8().is_multiple_of(8);
        // SAFETY: self.handle is live (see `close`); every other pointer below
        // is a local valid for the length claimed, or NULL where the export
        // documents NULL as refused.
        unsafe {
            match op {
                0 => {
                    let flags = match c.u8() % 5 {
                        0 => 0,
                        1 => GUSSET_FLAG_DIAGNOSTIC_ENGINE,
                        2 => GUSSET_FLAG_DIAGNOSTIC_ENGINE | GUSSET_FLAG_INLINE_COMPLETION,
                        3 => GUSSET_FLAG_INLINE_COMPLETION,
                        _ => c.u32(),
                    };
                    let header = CallHeader {
                        trace_id: [c.u8(); 16],
                        span_id: [c.u8(); 8],
                        // Mostly short, so a deadline actually fires.
                        timeout_ns: if c.u8().is_multiple_of(2) {
                            0
                        } else {
                            u64::from(c.u32() % 5_000_000)
                        },
                        flags,
                        reserved: if c.u8().is_multiple_of(4) { c.u32() } else { 0 },
                    };
                    let mut input = [0u8; 64];
                    // The diagnostic engine picks its behaviour from input[0]:
                    // only the modes that finish promptly and cannot abort
                    // (echo, panic, trace echo, stack size, bounded generated
                    // output, a short cooperative sleep).
                    input[0] = [0u8, 1, 7, 8, 16, 5][(c.u8() % 6) as usize];
                    input[1..5].copy_from_slice(&(c.u32() % (1 << 16)).to_le_bytes());
                    if input[0] == 5 {
                        input[1] %= 3;
                    }
                    let n = payload.len().min(48);
                    input[5..5 + n.min(59)].copy_from_slice(&payload[..n.min(59)]);
                    let len_kind = c.u8() % 6;
                    // An inline length is backed by the whole 4 KiB copy limit
                    // so any claimed length up to it is a valid read.
                    let backing = vec![0xA5u8; 4096];
                    let (in_ptr, in_len, buffer_id): (*const u8, usize, u64) = match len_kind {
                        0 => (input.as_ptr(), 64, 0),
                        1 => (ptr::null(), 0, 0),
                        // NULL with a length: refused, never read.
                        2 => (ptr::null(), 1 + (c.u32() as usize % 4096), 0),
                        // Past the copy limit, up to usize::MAX: refused before
                        // any slice exists.
                        3 => (
                            backing.as_ptr(),
                            4097 + c.u64() as usize % (usize::MAX - 4097),
                            0,
                        ),
                        4 => (backing.as_ptr(), c.u32() as usize % 4097, 0),
                        _ => (input.as_ptr(), 64, self.buffer_id(c)),
                    };
                    let header_ptr = if c.u8().is_multiple_of(16) {
                        ptr::null()
                    } else {
                        &header as *const _
                    };
                    let mut ticket = 0u64;
                    let code = with_status(null_status, |st| {
                        gusset_submit(
                            self.handle,
                            header_ptr,
                            in_ptr,
                            in_len,
                            buffer_id,
                            &mut ticket,
                            st,
                        )
                    });
                    if code == FFI_OK {
                        self.tickets.push(ticket);
                    }
                }
                1 => {
                    let ticket = self.ticket(c);
                    let (mut id, mut p, mut len) = (0u64, ptr::null_mut::<u8>(), 0usize);
                    let code = with_status(null_status, |st| {
                        gusset_take(self.handle, ticket, &mut id, &mut p, &mut len, st)
                    });
                    if code == FFI_OK && id != 0 {
                        // The result is readable for len bytes until it is freed.
                        let bytes = std::slice::from_raw_parts(p, len);
                        std::hint::black_box(bytes.iter().fold(0u8, |a, b| a ^ b));
                        let freed = with_status(null_status, |st| {
                            gusset_buf_free(self.handle, id & !TAKE_OWNED_FLAG, st)
                        });
                        assert_eq!(freed, FFI_OK, "an owned take buffer must free");
                    }
                    self.tickets.retain(|t| *t != ticket || code != FFI_OK);
                }
                2 => {
                    let len = match c.u8() % 6 {
                        0 => 0,
                        1 => usize::MAX,
                        2 => isize::MAX as usize,
                        3 => c.u64() as usize,
                        _ => c.u32() as usize % (MAX_REAL_BUFFER + 1),
                    };
                    let (mut id, mut p) = (0u64, ptr::null_mut::<u8>());
                    let code = with_status(null_status, |st| {
                        gusset_buf_alloc(self.handle, len, &mut id, &mut p, st)
                    });
                    if code == FFI_OK {
                        if len > 0 {
                            // Writable for the whole length it was given.
                            ptr::write(p, 1);
                            ptr::write(p.add(len - 1), 2);
                        }
                        self.buffers.push(id);
                    }
                }
                3 => {
                    let id = self.buffer_id(c);
                    let code = with_status(null_status, |st| gusset_buf_free(self.handle, id, st));
                    if code == FFI_OK {
                        self.buffers.retain(|b| *b != id);
                    }
                }
                4 => {
                    let ticket = self.ticket(c);
                    let code =
                        with_status(null_status, |st| gusset_cancel(self.handle, ticket, st));
                    assert!(code == FFI_OK || code == FFI_POISONED || code == FFI_PANIC);
                }
                5 => {
                    with_status(null_status, |st| gusset_cancel_all(self.handle, st));
                }
                6 => {
                    // A second attach is refused; a ring already held is kept
                    // and the new one released, which is what a host does.
                    let (mut ring, mut shared, mut slots, mut cap) = (
                        ptr::null::<Ring>(),
                        ptr::null::<u8>(),
                        ptr::null::<u8>(),
                        0u64,
                    );
                    let code = with_status(null_status, |st| {
                        gusset_handle_ring(
                            self.handle,
                            &mut ring,
                            &mut shared,
                            &mut slots,
                            &mut cap,
                            st,
                        )
                    });
                    if code == FFI_OK {
                        assert!(self.ring.is_null(), "a second ring attach succeeded");
                        assert!(!ring.is_null() && !shared.is_null() && !slots.is_null());
                        assert!(cap.is_power_of_two());
                        self.ring = ring;
                    } else {
                        assert!(ring.is_null(), "a refused attach wrote a ring pointer");
                    }
                }
                7 => {
                    // Any cap, but a non-null pointer must address cap entries:
                    // back it with the real field count and pass at most that.
                    let n = gusset_abi_fields(ptr::null_mut(), ptr::null_mut(), c.u32()) as usize;
                    assert!(n > 0, "gusset_abi_fields reported no fields");
                    let mut off = vec![0xFFFF_FFFFu32; n + 1];
                    let mut sz = vec![0xFFFF_FFFFu32; n + 1];
                    let cap = (c.u32() as usize % (n + 1)) as u32;
                    let (po, ps) = match c.u8() % 4 {
                        0 => (ptr::null_mut(), sz.as_mut_ptr()),
                        1 => (off.as_mut_ptr(), ptr::null_mut()),
                        _ => (off.as_mut_ptr(), sz.as_mut_ptr()),
                    };
                    assert_eq!(gusset_abi_fields(po, ps, cap) as usize, n);
                    // Never past cap: the sentinel after it survives.
                    assert_eq!(off[n], 0xFFFF_FFFF);
                    assert_eq!(sz[n], 0xFFFF_FFFF);
                    let mut layout = std::mem::MaybeUninit::<AbiLayout>::zeroed();
                    gusset_abi_layout(layout.as_mut_ptr());
                    gusset_abi_layout(ptr::null_mut());
                }
                8 => {
                    let mut st = FfiStatus::ok();
                    // Freeing a status that carries nothing, then again, then
                    // NULL: all no-ops.
                    gusset_status_free(&mut st);
                    gusset_status_free(&mut st);
                    gusset_status_free(ptr::null_mut());
                    let mut stats = std::mem::MaybeUninit::<AllocStats>::zeroed();
                    gusset_alloc_stats(stats.as_mut_ptr());
                    gusset_alloc_stats(ptr::null_mut());
                }
                9 => {
                    let cap = match c.u8() % 4 {
                        0 => 0,
                        1 => 1,
                        2 => 7,
                        _ => 256,
                    };
                    let mut buf = vec![0xEEu8; 257];
                    let mut written = usize::MAX;
                    gusset_drain_logs(buf.as_mut_ptr(), cap, &mut written);
                    assert!(written <= cap, "drain_logs wrote {written} of {cap}");
                    assert_eq!(buf[256], 0xEE, "drain_logs wrote past its buffer");
                    gusset_drain_logs(ptr::null_mut(), cap, &mut written);
                    gusset_drain_logs(buf.as_mut_ptr(), cap, ptr::null_mut());
                }
                10 => {
                    // Opens that must be refused, or succeed and close cleanly.
                    let (r, w) = pipe();
                    let mut w_open = true;
                    let mut extra = None;
                    let fd = match c.u8() % 7 {
                        0 => -1,
                        1 => i32::MAX,
                        // The read end: open, but not writable.
                        2 => r,
                        // Closed before the open.
                        3 => {
                            close_fd(w);
                            w_open = false;
                            w
                        }
                        // Open for writing, but not a pipe.
                        4 => {
                            let f = libc::open(c"/dev/null".as_ptr(), libc::O_WRONLY);
                            extra = Some(f);
                            f
                        }
                        _ => w,
                    };
                    let pool = match c.u8() % 5 {
                        0 => MAX_POOL_SIZE as u32 + 1,
                        1 => u32::MAX,
                        n => u32::from(n) % 3,
                    };
                    // -1 for a descriptor that is not open, before and after.
                    let flags_before = libc::fcntl(fd, libc::F_GETFL);
                    let mut h: *mut Handle = ptr::null_mut();
                    let out = if c.u8().is_multiple_of(16) {
                        ptr::null_mut()
                    } else {
                        &mut h as *mut _
                    };
                    let code = with_status(null_status, |st| gusset_handle_open(pool, fd, out, st));
                    if code == FFI_OK {
                        // Only the write end is ever accepted, and the handle
                        // owns it from here: closing the handle closes w.
                        assert_eq!(
                            fd, w,
                            "an open accepted a descriptor that is not a pipe write end"
                        );
                        let closed = with_status(false, |st| gusset_handle_close(h, st));
                        assert!(documented(closed));
                        w_open = false;
                    } else {
                        assert!(h.is_null(), "a refused open wrote a handle");
                        // The descriptor stays the caller's: still open (or
                        // still not), with its status flags untouched (no
                        // O_NONBLOCK left behind by a refused open).
                        assert_eq!(
                            libc::fcntl(fd, libc::F_GETFL),
                            flags_before,
                            "a refused open changed the caller's descriptor"
                        );
                    }
                    if w_open {
                        close_fd(w);
                    }
                    close_fd(r);
                    if let Some(f) = extra {
                        if f >= 0 {
                            close_fd(f);
                        }
                    }
                }
                11 => {
                    // NULL handles are refused, not dereferenced.
                    let mut t = 0u64;
                    let code = with_status(null_status, |st| gusset_cancel(ptr::null_mut(), 1, st));
                    assert_eq!(code, FFI_BAD_ARG);
                    let code =
                        with_status(null_status, |st| gusset_cancel_all(ptr::null_mut(), st));
                    assert_eq!(code, FFI_BAD_ARG);
                    let code =
                        with_status(null_status, |st| gusset_buf_free(ptr::null_mut(), 1, st));
                    assert_eq!(code, FFI_BAD_ARG);
                    let code = with_status(null_status, |st| {
                        gusset_submit(ptr::null_mut(), ptr::null(), ptr::null(), 0, 0, &mut t, st)
                    });
                    assert_eq!(code, FFI_BAD_ARG);
                    let code =
                        with_status(null_status, |st| gusset_handle_close(ptr::null_mut(), st));
                    assert_eq!(code, FFI_BAD_ARG);
                    gusset_ring_release(ptr::null());
                }
                _ => {
                    // Reap what is ready so the pipe never fills.
                    let mut sink = [0u8; 256];
                    while libc::read(self.read_fd, sink.as_mut_ptr().cast(), sink.len()) > 0 {}
                }
            }
        }
    }
}

fuzz_target!(|data: &[u8]| {
    let mut c = Cursor { data, at: 0 };
    // Buffer memory is counted by Gusset itself, so a sequence that leaves any
    // behind after every handle it opened is closed is a leak in the exports.
    let live_before = gusset::get_alloc_stats().live_bytes;
    run(&mut c, data);
    let live_after = gusset::get_alloc_stats().live_bytes;
    assert_eq!(
        live_after, live_before,
        "buffer bytes outlived every handle: {live_before} before, {live_after} after"
    );
});

fn run(c: &mut Cursor, data: &[u8]) {
    let Some(mut rig) = Rig::open() else {
        return;
    };
    for _ in 0..MAX_OPS {
        if c.done() {
            break;
        }
        let payload_at = c.at.min(data.len());
        let payload = &data[payload_at..(payload_at + 8).min(data.len())];
        rig.step(c, payload);
        // SAFETY: the handle is live until `close`.
        if unsafe { &*rig.handle }.is_poisoned() {
            // A poisoned handle refuses new work (that is I2); start over so
            // the rest of the sequence still exercises live paths.
            rig.close();
            match Rig::open() {
                Some(next) => rig = next,
                None => return,
            }
        }
    }
    rig.close();
}
