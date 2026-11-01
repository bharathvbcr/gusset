//! Reference Rust engine implementing UniFFI-compatible serialization and
//! execution on top of the Gusset runtime contract (`gusset::JobContext`, `set_engine_handler`).

#![allow(dead_code)]

use gusset::{set_engine_handler, JobContext, JobOutput};
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

/// Initializes the UniFFI engine handler with Gusset.
/// In an adopter service, this is called once at process startup.
pub fn init_uniffi_engine() {
    set_engine_handler(uniffi_dispatch_handler);
}

/// Dispatch handler registered with Gusset's worker pool.
pub fn uniffi_dispatch_handler(ctx: &JobContext, input: &[u8]) -> Result<JobOutput, String> {
    if input.is_empty() {
        return Err("uniffi: empty payload".to_string());
    }

    let fn_id = input[0];
    let payload = &input[1..];

    match fn_id {
        FN_CALCULATE => handle_calculate(payload),
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

fn handle_calculate(mut input: &[u8]) -> Result<JobOutput, String> {
    let op = read_string(&mut input)?;
    let a = read_f64(&mut input)?;
    let b = read_f64(&mut input)?;

    let mut out = Vec::with_capacity(32);

    match op.as_str() {
        "Add" => {
            OPERATIONS_COUNTER.fetch_add(1, Ordering::Relaxed);
            write_u8(&mut out, STATUS_SUCCESS);
            write_f64(&mut out, a + b);
            write_i64(&mut out, OPERATIONS_COUNTER.load(Ordering::Relaxed));
        }
        "Subtract" => {
            OPERATIONS_COUNTER.fetch_add(1, Ordering::Relaxed);
            write_u8(&mut out, STATUS_SUCCESS);
            write_f64(&mut out, a - b);
            write_i64(&mut out, OPERATIONS_COUNTER.load(Ordering::Relaxed));
        }
        "Multiply" => {
            OPERATIONS_COUNTER.fetch_add(1, Ordering::Relaxed);
            write_u8(&mut out, STATUS_SUCCESS);
            write_f64(&mut out, a * b);
            write_i64(&mut out, OPERATIONS_COUNTER.load(Ordering::Relaxed));
        }
        "Divide" => {
            if b == 0.0 {
                write_u8(&mut out, STATUS_ERROR);
                write_i32(&mut out, MATH_ERR_DIVISION_BY_ZERO);
                write_string(&mut out, "division by zero");
            } else {
                OPERATIONS_COUNTER.fetch_add(1, Ordering::Relaxed);
                write_u8(&mut out, STATUS_SUCCESS);
                write_f64(&mut out, a / b);
                write_i64(&mut out, OPERATIONS_COUNTER.load(Ordering::Relaxed));
            }
        }
        "Sqrt" => {
            if a < 0.0 {
                write_u8(&mut out, STATUS_ERROR);
                write_i32(&mut out, MATH_ERR_NEGATIVE_ROOT);
                write_string(&mut out, "cannot compute square root of negative number");
            } else {
                OPERATIONS_COUNTER.fetch_add(1, Ordering::Relaxed);
                write_u8(&mut out, STATUS_SUCCESS);
                write_f64(&mut out, a.sqrt());
                write_i64(&mut out, OPERATIONS_COUNTER.load(Ordering::Relaxed));
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

fn write_bool(out: &mut Vec<u8>, v: bool) {
    out.push(if v { 1 } else { 0 });
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
