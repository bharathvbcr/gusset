//! The built-in diagnostic engine behind the panic zoo and pitfall suite.

use super::{sys, JobOutput, MAX_BUFFER_BYTES};
use crate::header::JobContext;
use std::thread;

/// Diagnostic mode 16: a generated output built through the buffer allocator.
///
/// `input[1..5]` is the output length (u32 LE), `input[5]` a seed; byte `i` of the
/// output is `(i as u8) ^ seed`. The output is grown a chunk at a time so every
/// resize path of [`crate::alloc::BufferAlloc`] runs. Built with the stable
/// `Allocator` trait it is adopted without a copy; on older toolchains the same
/// bytes come back as a plain vector, so the Go suite asserts identical results
/// on both.
pub const DIAG_MODE_ALLOCATED: u8 = 16;

pub(super) fn diagnostic_allocated(ctx: &JobContext, input: &[u8]) -> Result<JobOutput, String> {
    let len = match input.get(1..5) {
        Some(b) => u32::from_le_bytes([b[0], b[1], b[2], b[3]]) as usize,
        None => return Err("mode 16 needs a u32 LE length".to_string()),
    };
    if len > MAX_BUFFER_BYTES {
        return Err(format!(
            "output {} bytes exceeds maximum {} bytes",
            len, MAX_BUFFER_BYTES
        ));
    }
    let seed = input.get(5).copied().unwrap_or(0);

    #[cfg(gusset_allocator_api)]
    #[allow(
        clippy::incompatible_msrv,
        reason = "compiled only where allocator_probe.rs found the stable Allocator API"
    )]
    let mut out: Vec<u8, crate::alloc::BufferAlloc> = Vec::new_in(crate::alloc::BufferAlloc);
    #[cfg(not(gusset_allocator_api))]
    let mut out: Vec<u8> = Vec::new();

    const CHUNK: usize = 1 << 16;
    let mut i = 0usize;
    while i < len {
        ctx.check().map_err(|r| format!("cancelled: {:?}", r))?;
        let n = CHUNK.min(len - i);
        // Fallible growth: an infallible push that cannot allocate aborts the
        // process, Go included (see BufferAlloc).
        out.try_reserve(n).map_err(|e| e.to_string())?;
        out.extend((i..i + n).map(|j| (j as u8) ^ seed));
        i += n;
    }
    Ok(out.into())
}

/// Built-in diagnostic engine driving the panic zoo and the pitfall suite.
///
/// Reachable only through `GUSSET_FLAG_DIAGNOSTIC_ENGINE` and only when no adopter
/// engine is registered. It selects behaviour from `input[0]`, which is precisely
/// why untrusted payloads must never reach it.
pub fn diagnostic_dispatch(ctx: &JobContext, input: &[u8]) -> Result<Vec<u8>, String> {
    if input.is_empty() {
        return Ok(Vec::new());
    }

    match input[0] {
        // Mode 0: Echo input back
        0 => Ok(input.to_vec()),
        // Mode 1: Plain panic
        1 => panic!("plain panic"),
        // Mode 2: Panic with embedded NUL byte
        2 => panic!("panic with embedded NUL \0 byte"),
        // Mode 3: Non-string panic
        3 => std::panic::panic_any(42i32),
        // Mode 4: Error with panic in Display / formatting
        4 => {
            struct PanicOnDisplay;
            impl std::fmt::Display for PanicOnDisplay {
                fn fmt(&self, _f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                    panic!("panic inside Display implementation");
                }
            }
            Err(PanicOnDisplay.to_string())
        }
        // Mode 5: Timeout / sleep loop checking ctx.check()
        5 => {
            let iterations = if input.len() >= 2 {
                input[1] as usize
            } else {
                100
            };
            for _ in 0..iterations {
                ctx.check().map_err(|e| format!("cancelled: {:?}", e))?;
                thread::sleep(std::time::Duration::from_millis(10));
            }
            Ok(vec![5, 0])
        }
        // Mode 7: Echo the header's trace and span ids, so R9 trace propagation is
        // verifiable end to end rather than assumed.
        7 => {
            let h = ctx.header();
            let mut out = Vec::with_capacity(24);
            out.extend_from_slice(&h.trace_id);
            out.extend_from_slice(&h.span_id);
            Ok(out)
        }
        // Mode 6: Deep recursion to prove stack size (R8)
        6 => {
            fn recurse(depth: u32) -> u32 {
                if depth == 0 {
                    0
                } else {
                    std::hint::black_box(recurse(depth - 1) + 1)
                }
            }
            // The depth comes from the payload, so it must be bounded like any
            // other input-derived quantity. Uncapped, `[6, 0xFF, 0xFF, 0xFF, 0xFF]`
            // recurses 4.29 billion frames and overflows even an 8 MiB stack — an
            // abort, not a catchable panic, so the firewall cannot contain it. The
            // fuzz targets reach this mode constantly; 100_000 frames is well under
            // the stack budget and still an order past the 128 KiB musl default
            // that the mode exists to disprove.
            const MAX_RECURSION_DEPTH: u32 = 100_000;
            let requested = if input.len() >= 5 {
                u32::from_le_bytes([input[1], input[2], input[3], input[4]])
            } else {
                50_000
            };
            let res = recurse(requested.min(MAX_RECURSION_DEPTH));
            Ok(res.to_le_bytes().to_vec())
        }
        // Mode 8: report the executing worker's real stack size (I5/R8).
        //
        // Mode 6 recurses to prove the stack is deep enough, but a worker Gusset
        // did not size explicitly would still get Rust's own std default of 2 MiB
        // (see RUST_MIN_STACK) rather than the platform's pthread default, so any
        // recursion that fits in 2 MiB passes whether or not Gusset set the size.
        // Reading the size back from the thread that actually ran the job is what
        // distinguishes "Gusset sized this stack" from "the default happened to be
        // enough", which is the claim a musl host would otherwise be needed to test.
        8 => Ok(sys::current_thread_stack_size()
            .unwrap_or(0)
            .to_le_bytes()
            .to_vec()),
        // Mode 9: sleep without ever calling `ctx.check()` — a deliberately
        // *non-cooperative* engine.
        //
        // Mode 5 is the cooperative twin: it checks every iteration, so Rust
        // notices the deadline itself and posts a `Cancelled` completion, and it
        // is that completion which wakes the waiting Go caller. Every real
        // adopter engine — a tokenizer, a regex scan, a proof verifier, a SIMD
        // transform — looks like mode 9, not mode 5: it runs a tight loop that
        // never asks whether it should stop.
        //
        // Without this mode the deadline suite only ever measured engines that
        // cancel themselves, so it could not tell "Gusset enforced the caller's
        // deadline" apart from "the engine happened to stop on its own".
        //
        // Duration is `input[1]` units of 10 ms, so the byte caps it at 2.55 s —
        // exactly mode 5's existing fuzz exposure.
        //
        // Mode 9 is a *delay prefix*: after sleeping it dispatches the remainder
        // of the payload. `[9, 10, 1]` is a panic 100 ms late, `[9, 10, 0, ..]`
        // is a slow echo of a large payload. Without that, proving that an
        // abandoned *large* result is reclaimed, or that an abandoned *panic*
        // still poisons, is impossible: an instant result is taken by the caller
        // before it can be abandoned, so those paths were never reached.
        //
        // A remainder that starts with 9 echoes instead of recursing. Nesting
        // would let `[9,255,9,255,...]` chain 2.55 s sleeps one per two bytes,
        // turning a 2 KiB fuzz input into a 43-minute work unit.
        9 => {
            let units = if input.len() >= 2 {
                input[1] as u64
            } else {
                10
            };
            thread::sleep(std::time::Duration::from_millis(units * 10));
            match input.get(2) {
                None | Some(9) => Ok(input.to_vec()),
                Some(_) => diagnostic_dispatch(ctx, &input[2..]),
            }
        }
        // Mode 10: Vector sum-and-square computation (Phase 2 CPU-bound engine)
        10 => {
            // Cancellation is checked per chunk, outside the hot loop, so the
            // inner fold vectorizes (see the example engine's opcode 10).
            let mut acc = 0u64;
            for chunk in input[1..].chunks(4096) {
                ctx.check().map_err(|e| format!("cancelled: {:?}", e))?;
                // 4096 * 255^2 < 2^32: a chunk sums exactly in u32, which
                // packs twice as many lanes per vector as u64. The widest
                // vector unit is chosen at run time (sys::sum_squares_chunk).
                let chunk_sum = sys::sum_squares_chunk(chunk);
                acc = acc.wrapping_add(chunk_sum as u64);
            }
            Ok(acc.to_le_bytes().to_vec())
        }
        // Mode 11: fixed-cost CPU work, `u32` iterations little-endian at
        // input[1..5]. The arithmetic is byte-identical to `rs_spin` in
        // bench/seed/rs, which is the raw-cgo half of the same measurement: a
        // difference between the two is transport cost and nothing else.
        //
        // This is what makes the crossover measurable — the work duration at
        // which Gusset's coordination stops mattering against a blocking cgo
        // call. A noop benchmark cannot answer that, and a noop is not why
        // anyone embeds Rust in Go.
        //
        // Capped like mode 6's recursion depth: the count is input-derived, and
        // uncapped `[11, 0xFF, 0xFF, 0xFF, 0xFF]` is 4.29 billion iterations.
        // 50 million is roughly 50 ms here, well under mode 5's 2.55 s fuzz
        // exposure, and two orders past the crossover the benchmark looks for.
        11 => {
            const MAX_SPIN_ITERS: u32 = 50_000_000;
            let requested = if input.len() >= 5 {
                u32::from_le_bytes([input[1], input[2], input[3], input[4]])
            } else {
                1_000
            };
            let iters = requested.min(MAX_SPIN_ITERS) as u64;
            let mut acc: u64 = 0;
            for i in 0..iters {
                acc = acc.wrapping_add(i.wrapping_mul(i) ^ acc.rotate_left(7));
            }
            Ok(std::hint::black_box(acc).to_le_bytes().to_vec())
        }
        // Mode 12: Megabyte string panic to test payload truncation (R3 hardening)
        12 => {
            panic!("{}", "A".repeat(1024 * 1024));
        }
        // Mode 13: Various non-string primitive panics
        13 => match input.get(1).copied().unwrap_or(0) {
            0 => std::panic::panic_any(12345u32),
            1 => std::panic::panic_any(-9876543210i64),
            2 => std::panic::panic_any(true),
            3 => std::panic::panic_any(999999usize),
            _ => std::panic::panic_any(-42isize),
        },
        // Mode 14: Log burst from worker thread to test log ring concurrency
        14 => {
            let count = input.get(1).copied().unwrap_or(10) as usize;
            for i in 0..count {
                crate::ffi::log_event(&format!("worker log event {}", i));
            }
            Ok(vec![14, count as u8])
        }
        // Mode 15: panic with a payload whose destructor panics again, input[1]
        // times over (capped). Disposing of a caught payload runs its `Drop`
        // outside the engine's `catch_unwind`; this mode is how the Go suite
        // proves that disposal cannot kill the worker and strand the ticket.
        15 => {
            struct DropBomb(u8);
            impl Drop for DropBomb {
                fn drop(&mut self) {
                    if self.0 == 0 {
                        panic!("diagnostic payload destructor panicked");
                    }
                    std::panic::panic_any(DropBomb(self.0 - 1));
                }
            }
            std::panic::panic_any(DropBomb(input.get(1).copied().unwrap_or(0).min(8)))
        }
        // Default: echo
        _ => Ok(input.to_vec()),
    }
}
