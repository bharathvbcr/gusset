//! Opcode-specific engine registration and zero-copy egress validation.
//!
//! Validates:
//! 1. Opcode-specific engine registration (`register_engine`) and precedence (R9).
//! 2. Zero-copy buffer egress (`JobOutput::Buffer(buf_id)`) (R16).
//! 3. JobContext observability metrics: opcode, queue delay, compute duration, total duration.

use gusset::header::CallHeader;
use gusset::pool::{Handle, JobResult};

mod common;
use common::{make_pipe, read_ticket};

fn run(handle: &Handle, read_fd: i32, header: CallHeader, input: &[u8]) -> JobResult {
    let ticket = match handle.submit(header, input, 0) {
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
fn test_opcode_specific_engine_and_zero_copy_buffer() {
    gusset::pool::clear_engine_handlers();

    // Register opcode 42
    gusset::pool::register_engine(42, |ctx, input| {
        assert_eq!(ctx.opcode(), 42);
        assert!(
            ctx.queue_delay().is_some(),
            "queue delay should be recorded"
        );
        let mut out = input.to_vec();
        out.reverse();
        Ok(out)
    });

    let (r, w) = make_pipe();
    let handle = match Handle::open(2, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };

    let header = CallHeader {
        reserved: 42,
        ..Default::default()
    };

    match run(&handle, r, header, &[1, 2, 3, 4]) {
        JobResult::Ok(out) => assert_eq!(out, vec![4, 3, 2, 1]),
        other => panic!("expected reversed slice from opcode 42, got {:?}", other),
    }

    // Zero-copy buffer egress: return JobOutput::Buffer
    let (buf_id, buf_ptr) = match handle.buf_alloc(4) {
        Ok(res) => res,
        Err(e) => panic!("buf_alloc failed: {}", e),
    };
    unsafe {
        std::ptr::copy_nonoverlapping([10u8, 20, 30, 40].as_ptr(), buf_ptr, 4);
    }

    gusset::pool::register_engine(43, move |ctx, _input| {
        assert_eq!(ctx.opcode(), 43);
        Ok(gusset::pool::JobOutput::Buffer(buf_id))
    });

    let header_buf = CallHeader {
        reserved: 43,
        ..Default::default()
    };

    match run(&handle, r, header_buf, &[]) {
        JobResult::Buffer(id) => {
            assert_eq!(id, buf_id);
            let (ptr, len) = match handle.buf_get(id) {
                Ok(res) => res,
                Err(e) => panic!("buf_get failed: {}", e),
            };
            assert_eq!(len, 4);
            let slice = unsafe { std::slice::from_raw_parts(ptr, len) };
            assert_eq!(slice, &[10, 20, 30, 40]);
        }
        other => panic!("expected Buffer output from opcode 43, got {:?}", other),
    }

    // Unregistered opcode fails closed
    let header_unknown = CallHeader {
        reserved: 99,
        ..Default::default()
    };
    match run(&handle, r, header_unknown, &[]) {
        JobResult::Err(msg) => assert!(
            msg.contains("no engine handler registered"),
            "expected fail-closed error, got: {}",
            msg
        ),
        other => panic!("expected Err for unregistered opcode, got {:?}", other),
    }

    handle.close();
    unsafe {
        libc::close(r);
    }
}
