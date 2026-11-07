//! Reference Rust engine implementing UniFFI-compatible serialization and
//! execution on top of the Gusset runtime contract (`gusset::JobContext`, `register_engine`).
//!
//! This crate is also the umbrella staticlib (docs/adoption.md step 1): its
//! `libgusset.a` holds Gusset and this engine, and Go registers the engine
//! through the one C export below, [`init_uniffi_engine`].

use gusset_core::{register_engine, JobContext, JobOutput};
use std::sync::atomic::{AtomicI64, Ordering};
use std::time::{Duration, Instant};

/// C-ABI compatible RustBuffer layout matching UniFFI specifications.
#[repr(C)]
#[derive(Debug)]
pub struct RustBuffer {
    /// Total capacity in bytes.
    pub capacity: i64,
    /// Length of initialized data in bytes.
    pub len: i64,
    /// Raw pointer to byte buffer.
    pub data: *mut u8,
}

const FN_CALCULATE: u8 = 1;
const FN_TRANSFORM_TEXT: u8 = 2;
const FN_EXECUTE_SLOW_JOB: u8 = 3;
const FN_TRIGGER_PANIC: u8 = 4;

const STATUS_SUCCESS: u8 = 0;
const STATUS_ERROR: u8 = 1;

const MATH_ERR_UNKNOWN_OP: i32 = 1;
const MATH_ERR_DIVISION_BY_ZERO: i32 = 2;
const MATH_ERR_NEGATIVE_ROOT: i32 = 3;

static OPERATIONS_COUNTER: AtomicI64 = AtomicI64::new(0);

/// Registers the UniFFI engine with Gusset under `opcode`, for Go to call once
/// at startup before the first submission. A Go handle reaches it with
/// `gusset.WithOpcode(opcode)`.
///
/// Returns 0 when the engine is registered, and -1 when `register_engine`
/// refused: `opcode` is 0, or an engine already holds it. A nonzero opcode
/// rather than `set_engine_handler`'s global slot, so registering this engine
/// never displaces another one, nor the diagnostic engine Gusset's own tests
/// use on opcode 0, in the same process.
#[no_mangle]
pub extern "C" fn init_uniffi_engine(opcode: u32) -> i32 {
    match register_engine(opcode, uniffi_dispatch_handler) {
        Ok(()) => 0,
        Err(_) => -1,
    }
}

/// Dispatch handler registered with Gusset's worker pool.
pub fn uniffi_dispatch_handler(ctx: &JobContext, input: &[u8]) -> Result<JobOutput, String> {
    dispatch(ctx, input, &OPERATIONS_COUNTER)
}

/// The handler body with the operations counter passed in, so tests drive it
/// with their own counter instead of the process-wide one.
fn dispatch(ctx: &JobContext, input: &[u8], counter: &AtomicI64) -> Result<JobOutput, String> {
    if input.is_empty() {
        return Err("uniffi: empty payload".to_string());
    }

    let fn_id = input[0];
    let payload = &input[1..];

    match fn_id {
        FN_CALCULATE => handle_calculate(payload, counter),
        FN_TRANSFORM_TEXT => handle_transform_text(payload),
        FN_EXECUTE_SLOW_JOB => handle_slow_job(ctx, payload),
        FN_TRIGGER_PANIC => {
            // Deliberate panic to demonstrate Gusset's panic firewall containment (R2, R3).
            // Under raw cgo / unmanaged UniFFI, this aborts the Go process with SIGABRT.
            // Under Gusset, catch_unwind catches this, poisons the handle, and Go receives ErrPanic.
            panic!("uniffi engine: deliberate panic to verify Gusset panic firewall");
        }
        unknown => Err(format!("uniffi: unknown function opcode {}", unknown)),
    }
}

fn handle_calculate(mut input: &[u8], counter: &AtomicI64) -> Result<JobOutput, String> {
    let op = read_string(&mut input)?;
    let a = read_f64(&mut input)?;
    let b = read_f64(&mut input)?;

    let mut out = Vec::with_capacity(32);

    match op.as_str() {
        "Add" => {
            counter.fetch_add(1, Ordering::Relaxed);
            write_u8(&mut out, STATUS_SUCCESS);
            write_f64(&mut out, a + b);
            write_i64(&mut out, counter.load(Ordering::Relaxed));
        }
        "Subtract" => {
            counter.fetch_add(1, Ordering::Relaxed);
            write_u8(&mut out, STATUS_SUCCESS);
            write_f64(&mut out, a - b);
            write_i64(&mut out, counter.load(Ordering::Relaxed));
        }
        "Multiply" => {
            counter.fetch_add(1, Ordering::Relaxed);
            write_u8(&mut out, STATUS_SUCCESS);
            write_f64(&mut out, a * b);
            write_i64(&mut out, counter.load(Ordering::Relaxed));
        }
        "Divide" => {
            if b == 0.0 {
                write_u8(&mut out, STATUS_ERROR);
                write_i32(&mut out, MATH_ERR_DIVISION_BY_ZERO);
                write_string(&mut out, "division by zero");
            } else {
                counter.fetch_add(1, Ordering::Relaxed);
                write_u8(&mut out, STATUS_SUCCESS);
                write_f64(&mut out, a / b);
                write_i64(&mut out, counter.load(Ordering::Relaxed));
            }
        }
        "Sqrt" => {
            if a < 0.0 {
                write_u8(&mut out, STATUS_ERROR);
                write_i32(&mut out, MATH_ERR_NEGATIVE_ROOT);
                write_string(&mut out, "cannot compute square root of negative number");
            } else {
                counter.fetch_add(1, Ordering::Relaxed);
                write_u8(&mut out, STATUS_SUCCESS);
                write_f64(&mut out, a.sqrt());
                write_i64(&mut out, counter.load(Ordering::Relaxed));
            }
        }
        unknown => {
            write_u8(&mut out, STATUS_ERROR);
            write_i32(&mut out, MATH_ERR_UNKNOWN_OP);
            write_string(&mut out, &format!("unsupported operation: {}", unknown));
        }
    }

    Ok(out.into())
}

fn handle_transform_text(mut input: &[u8]) -> Result<JobOutput, String> {
    let text = read_string(&mut input)?;
    let to_uppercase = read_bool(&mut input)?;
    let reverse = read_bool(&mut input)?;

    let mut transformed = text;
    if to_uppercase {
        transformed = transformed.to_uppercase();
    }
    if reverse {
        transformed = transformed.chars().rev().collect();
    }

    let mut out = Vec::with_capacity(transformed.len() + 16);
    write_u8(&mut out, STATUS_SUCCESS);
    write_string(&mut out, &transformed);
    write_i32(&mut out, transformed.len() as i32);

    Ok(out.into())
}

fn handle_slow_job(ctx: &JobContext, mut input: &[u8]) -> Result<JobOutput, String> {
    let delay_ms = read_i32(&mut input)?;
    let start = Instant::now();

    // Check cancellation between sleep slices (I3, R9).
    // If the Go context deadline expires or is cancelled, ctx.check() fails immediately,
    // aborting execution cleanly without holding the worker thread.
    let slice = Duration::from_millis(10);
    let total = Duration::from_millis(delay_ms.max(0) as u64);

    while Instant::now().duration_since(start) < total {
        ctx.check()?;
        std::thread::sleep(slice);
    }

    let mut out = Vec::with_capacity(32);
    write_u8(&mut out, STATUS_SUCCESS);
    write_i64(&mut out, start.elapsed().as_millis() as i64);
    write_string(&mut out, "completed");

    Ok(out.into())
}

// Helper serialization functions for UniFFI big-endian wire protocol:

fn read_u8(input: &mut &[u8]) -> Result<u8, String> {
    if input.is_empty() {
        return Err("underflow reading u8".to_string());
    }
    let b = input[0];
    *input = &input[1..];
    Ok(b)
}

fn read_bool(input: &mut &[u8]) -> Result<bool, String> {
    Ok(read_u8(input)? != 0)
}

fn read_i32(input: &mut &[u8]) -> Result<i32, String> {
    if input.len() < 4 {
        return Err("underflow reading i32".to_string());
    }
    let b = [input[0], input[1], input[2], input[3]];
    *input = &input[4..];
    Ok(i32::from_be_bytes(b))
}

fn read_f64(input: &mut &[u8]) -> Result<f64, String> {
    if input.len() < 8 {
        return Err("underflow reading f64".to_string());
    }
    let mut b = [0u8; 8];
    b.copy_from_slice(&input[..8]);
    *input = &input[8..];
    Ok(f64::from_be_bytes(b))
}

fn read_string(input: &mut &[u8]) -> Result<String, String> {
    let len = read_i32(input)? as usize;
    if input.len() < len {
        return Err("underflow reading string bytes".to_string());
    }
    let s = std::str::from_utf8(&input[..len])
        .map_err(|e| format!("invalid UTF-8 in string: {}", e))?
        .to_string();
    *input = &input[len..];
    Ok(s)
}

fn write_u8(out: &mut Vec<u8>, v: u8) {
    out.push(v);
}

fn write_i32(out: &mut Vec<u8>, v: i32) {
    out.extend_from_slice(&v.to_be_bytes());
}

fn write_i64(out: &mut Vec<u8>, v: i64) {
    out.extend_from_slice(&v.to_be_bytes());
}

fn write_f64(out: &mut Vec<u8>, v: f64) {
    out.extend_from_slice(&v.to_be_bytes());
}

fn write_string(out: &mut Vec<u8>, s: &str) {
    let bytes = s.as_bytes();
    write_i32(out, bytes.len() as i32);
    out.extend_from_slice(bytes);
}

#[cfg(test)]
mod tests {
    use super::*;
    use gusset_core::{clear_engine_handlers, CallHeader};
    use std::sync::atomic::AtomicBool;
    use std::sync::Arc;

    const GOLDEN: &str = include_str!("../../testdata/golden.txt");

    fn ctx() -> JobContext {
        JobContext::new(CallHeader::default(), Arc::new(AtomicBool::new(false)))
    }

    fn unhex(s: &str) -> Vec<u8> {
        assert!(s.len().is_multiple_of(2), "odd-length hex {s:?}");
        (0..s.len())
            .step_by(2)
            .map(|i| {
                u8::from_str_radix(&s[i..i + 2], 16)
                    .unwrap_or_else(|e| panic!("bad hex digit in {s:?}: {e}"))
            })
            .collect()
    }

    /// Runs one request and returns the response bytes, failing the test on an
    /// engine error. (clippy.toml forbids unwrap/expect, tests included.)
    fn run(req: &[u8], counter: &AtomicI64) -> Vec<u8> {
        match dispatch(&ctx(), req, counter) {
            Ok(out) => bytes(out),
            Err(e) => panic!("engine error for request {req:?}: {e}"),
        }
    }

    fn bytes(out: JobOutput) -> Vec<u8> {
        match out {
            JobOutput::Bytes(v) => v,
            other => panic!("unexpected output kind {other:?}"),
        }
    }

    /// Every response column of testdata/golden.txt, byte for byte. The Go
    /// tests read the same file, so a wire change here fails both sides.
    #[test]
    fn golden_vectors_match_engine_bytes() {
        let counter = AtomicI64::new(0);
        let mut compared = 0;
        for line in GOLDEN
            .lines()
            .filter(|l| !l.is_empty() && !l.starts_with('#'))
        {
            let cols: Vec<&str> = line.split_whitespace().collect();
            assert_eq!(cols.len(), 3, "malformed golden line {line:?}");
            if cols[2] == "-" {
                continue;
            }
            let got = run(&unhex(cols[1]), &counter);
            assert_eq!(got, unhex(cols[2]), "vector {}", cols[0]);
            compared += 1;
        }
        assert!(compared >= 10, "only {compared} vectors compared");
    }

    #[test]
    fn empty_payload_and_unknown_opcode_are_errors() {
        let counter = AtomicI64::new(0);
        assert_eq!(
            dispatch(&ctx(), &[], &counter).unwrap_err(),
            "uniffi: empty payload"
        );
        assert_eq!(
            dispatch(&ctx(), &[9], &counter).unwrap_err(),
            "uniffi: unknown function opcode 9"
        );
    }

    #[test]
    fn invalid_utf8_string_is_rejected() {
        let counter = AtomicI64::new(0);
        let req = [FN_TRANSFORM_TEXT, 0, 0, 0, 2, 0xff, 0xfe, 0, 0];
        let err = dispatch(&ctx(), &req, &counter).unwrap_err();
        assert!(err.starts_with("invalid UTF-8 in string"), "{err}");
    }

    #[test]
    fn truncated_payloads_underflow() {
        let counter = AtomicI64::new(0);
        for req in [
            vec![FN_CALCULATE],
            vec![FN_CALCULATE, 0, 0, 0, 3, b'A', b'd', b'd'],
            vec![FN_TRANSFORM_TEXT, 0, 0, 0, 9, b'a'],
            vec![FN_EXECUTE_SLOW_JOB, 0, 0],
        ] {
            let err = dispatch(&ctx(), &req, &counter).unwrap_err();
            assert!(err.starts_with("underflow"), "{req:?}: {err}");
        }
    }

    #[test]
    fn negative_string_length_is_underflow_not_a_huge_read() {
        let counter = AtomicI64::new(0);
        let req = [FN_TRANSFORM_TEXT, 0xff, 0xff, 0xff, 0xff, 0, 0];
        let err = dispatch(&ctx(), &req, &counter).unwrap_err();
        assert!(err.starts_with("underflow"), "{err}");
    }

    #[test]
    fn slow_job_completes_and_honours_cancel() {
        let counter = AtomicI64::new(0);
        let out = run(&[FN_EXECUTE_SLOW_JOB, 0, 0, 0, 0], &counter);
        assert_eq!(out[0], STATUS_SUCCESS);
        assert!(out.ends_with(&[0, 0, 0, 9, b'c', b'o', b'm', b'p', b'l', b'e', b't', b'e', b'd']));

        let flag = Arc::new(AtomicBool::new(true));
        let cancelled = JobContext::new(CallHeader::default(), flag);
        let err = dispatch(
            &cancelled,
            &[FN_EXECUTE_SLOW_JOB, 0, 0, 0x27, 0x10],
            &counter,
        )
        .unwrap_err();
        assert_eq!(err, "cancelled: Explicit");
    }

    /// The C export reports `register_engine`'s answer: 0 for a registration,
    /// -1 for opcode 0 and for an opcode already held. The only test in this
    /// binary that touches the process-global registry; it clears it after.
    #[test]
    fn init_registers_once_per_opcode_and_refuses_opcode_zero() {
        assert_eq!(init_uniffi_engine(0), -1, "opcode 0 must be refused");
        assert_eq!(init_uniffi_engine(0x55), 0, "first registration");
        assert_eq!(init_uniffi_engine(0x55), -1, "duplicate must be refused");
        assert_eq!(init_uniffi_engine(0x56), 0, "another opcode is free");
        clear_engine_handlers();
    }

    #[test]
    #[should_panic(expected = "deliberate panic")]
    fn panic_opcode_panics() {
        let counter = AtomicI64::new(0);
        let _ = dispatch(&ctx(), &[FN_TRIGGER_PANIC], &counter);
    }
}
