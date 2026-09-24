//! Phase 2 second-app validation, actually executed.
//!
//! `init_example_engine` had no callers anywhere in the repository. The Go test
//! named `TestPhase2_CpuBoundEngine` drove Gusset's *built-in* dispatcher — which
//! carried its own near-copy of the example's opcode 10 — so the adopter path this
//! crate exists to demonstrate was never run by anything.
//!
//! This test registers the example engine and drives it through the real worker
//! pool, which is what "a second app adopted the runtime contract" has to mean.
//!
//! Its own test binary: `set_engine_handler` installs a process-global handler, and
//! a parallel test expecting the diagnostic engine would silently get this one.

use gusset::header::{CallHeader, GUSSET_FLAG_DIAGNOSTIC_ENGINE};
use gusset::pool::{Handle, JobResult};
use gusset_example::init_example_engine;

mod common;
use common::{make_pipe, read_ticket};

fn run(handle: &Handle, read_fd: i32, header: CallHeader, input: &[u8]) -> JobResult {
    // R16: inline submit copies on the caller thread. The 10_000-byte opcode-10
    // payload is past the 4 KiB ceiling, so it has to travel as a Buffer — the
    // same rule the Go side now enforces. Leaving this as an inline slice made
    // the test pass against a submit that memcpy'd any size.
    let (submit_input, buffer_id) = if input.len() > 4096 {
        let (id, ptr) = match handle.buf_alloc(input.len()) {
            Ok(v) => v,
            Err(e) => panic!("buf_alloc failed: {}", e),
        };
        unsafe {
            std::ptr::copy_nonoverlapping(input.as_ptr(), ptr, input.len());
        }
        (&[][..], id)
    } else {
        (input, 0)
    };
    let ticket = match handle.submit(header, submit_input, buffer_id) {
        Ok(t) => t,
        Err(e) => panic!("submit failed: {}", e),
    };
    let completed = read_ticket(read_fd);
    assert_eq!(completed, ticket, "completion ticket must match the submit");
    match handle.take(ticket) {
        Ok(r) => r,
        Err(e) => panic!("take failed: {}", e),
    }
}

#[test]
fn adopter_engine_runs_and_displaces_the_diagnostic_engine() {
    // Registration used to swallow a poisoned registry lock and return `()`
    // regardless, so an adopter could believe its engine was installed when it was
    // not — and then quietly get the diagnostic engine instead. Assert the
    // transition rather than trusting the call.
    assert!(
        !gusset::pool::has_engine_handler(),
        "this test binary must start with no engine registered"
    );
    init_example_engine();
    assert!(
        gusset::pool::has_engine_handler(),
        "set_engine_handler must actually install the handler"
    );

    let (r, w) = make_pipe();
    let handle = match Handle::open(4, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };

    // Opcode 10: sum of squares over the payload tail.
    const N: usize = 10_000;
    let mut payload = vec![0u8; N + 1];
    payload[0] = 10;
    let mut expected: u64 = 0;
    for (i, slot) in payload.iter_mut().enumerate().take(N + 1).skip(1) {
        let v = (i % 127) as u8;
        *slot = v;
        expected = expected.wrapping_add((v as u64).wrapping_mul(v as u64));
    }

    match run(&handle, r, CallHeader::default(), &payload) {
        JobResult::Ok(out) => {
            assert_eq!(out.len(), 8, "opcode 10 returns a u64");
            let mut b = [0u8; 8];
            b.copy_from_slice(&out);
            assert_eq!(u64::from_le_bytes(b), expected, "sum-of-squares mismatch");
        }
        other => panic!("expected Ok from the adopter engine, got {:?}", other),
    }

    // Opcode 12: echo the tail.
    match run(&handle, r, CallHeader::default(), &[12, 1, 2, 3]) {
        JobResult::Ok(out) => assert_eq!(out, vec![1, 2, 3]),
        other => panic!("expected echo, got {:?}", other),
    }

    // The dependent's gate is the runtime's answer, via DEP_GUSSET_ALLOCATOR_API.
    assert_eq!(
        gusset_example::ALLOCATOR_OPCODE,
        gusset::ALLOCATOR_API,
        "the example engine's allocator gate disagrees with the gusset crate"
    );

    // Opcode 13 (Rust 1.100+): reversed tail built in BufferAlloc memory and
    // adopted zero-copy as the result buffer, 64-byte aligned.
    #[cfg(gusset_allocator_api)]
    match run(&handle, r, CallHeader::default(), &[13, 1, 2, 3, 4, 5]) {
        JobResult::Buffer(id) => {
            let (ptr, len) = match handle.buf_get(id) {
                Ok(v) => v,
                Err(e) => panic!("buf_get: {e}"),
            };
            assert_eq!(ptr as usize % gusset::BUFFER_ALIGN, 0);
            let got = unsafe { std::slice::from_raw_parts(ptr, len) };
            assert_eq!(got, &[5, 4, 3, 2, 1]);
            assert!(handle.buf_free(id).is_ok());
        }
        other => panic!("expected an adopted buffer from opcode 13, got {:?}", other),
    }

    // An unknown opcode is an engine error, not a panic and not a silent echo.
    match run(&handle, r, CallHeader::default(), &[99]) {
        JobResult::Err(msg) => assert!(
            msg.contains("unknown opcode"),
            "expected an unknown-opcode error, got: {}",
            msg
        ),
        other => panic!("expected Err from the adopter engine, got {:?}", other),
    }

    // A registered engine outranks the diagnostic flag. Byte 1 is `panic!("plain
    // panic")` in the diagnostic engine; here it must reach the adopter engine and
    // come back as an ordinary unknown-opcode error. Otherwise a stray flag could
    // swap a production engine for one that panics on attacker-chosen input.
    let flagged = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
        ..Default::default()
    };
    match run(&handle, r, flagged, &[1]) {
        JobResult::Err(msg) => assert!(
            msg.contains("unknown opcode"),
            "the registered engine must win over the diagnostic flag, got: {}",
            msg
        ),
        other => panic!(
            "diagnostic flag must not displace a registered engine, got {:?}",
            other
        ),
    }
    assert!(
        !handle.is_poisoned(),
        "no panic should have occurred, so the handle must be healthy"
    );

    // Opcode 11 is the adopter's own deliberate panic: the firewall catches it,
    // reports the location, and poisons the handle (I2, R10).
    match run(&handle, r, CallHeader::default(), &[11]) {
        JobResult::Panic { msg, file, .. } => {
            assert!(
                msg.contains("example engine induced panic"),
                "panic message should survive the boundary, got: {}",
                msg
            );
            match file {
                Some(f) => assert!(
                    f.contains("gusset-example"),
                    "panic location should point at the adopter engine, got: {}",
                    f
                ),
                None => panic!("panic location was lost"),
            }
        }
        other => panic!("expected a caught panic, got {:?}", other),
    }
    assert!(
        handle.is_poisoned(),
        "a caught panic must poison the handle (I2)"
    );

    handle.close();
    unsafe {
        libc::close(r);
    }
}
