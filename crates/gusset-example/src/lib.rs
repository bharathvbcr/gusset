//! Reference engine demonstrating how an adopter uses the Gusset runtime contract.
//!
//! Demonstrates engine handler registration, cooperative cancellation checks,
//! safe buffer mutations, and deliberate failure modes for testing.

#![deny(missing_docs)]

#[cfg(gusset_allocator_api)]
use gusset::BufferAlloc;
use gusset::{set_engine_handler, CancelReason, JobContext, JobOutput};

/// Whether opcode 13 (output built in `BufferAlloc` memory) is compiled in.
///
/// Decided by `build.rs` from `DEP_GUSSET_ALLOCATOR_API`, so it must always equal
/// [`gusset::ALLOCATOR_API`]; the adopter test asserts that it does.
pub const ALLOCATOR_OPCODE: bool = cfg!(gusset_allocator_api);

/// Initializes the example engine and registers its execution handler with Gusset.
pub fn init_example_engine() {
    set_engine_handler(
        |ctx: &JobContext, input: &[u8]| -> Result<JobOutput, String> {
            if input.is_empty() {
                return Ok(Vec::new().into());
            }

            match input[0] {
                // Opcode 10: Vector sum-and-square computation
                10 => {
                    // Check cancellation once per chunk, not inside the inner
                    // loop (I3, R9). A branch on `i % 1024` in the hot loop
                    // keeps LLVM from vectorizing it; this shape runs the same
                    // kernel about 2x faster and still observes a cancel
                    // within one 4 KiB chunk.
                    let mut acc = 0u64;
                    for chunk in input[1..].chunks(4096) {
                        ctx.check().map_err(|e| match e {
                            CancelReason::Explicit => "cancelled explicitly".to_string(),
                            CancelReason::DeadlineExceeded => "deadline exceeded".to_string(),
                        })?;
                        // 4096 * 255^2 < 2^32: a chunk sums exactly in u32, which
                        // packs twice as many lanes per vector as u64.
                        let chunk_sum: u32 = chunk.iter().map(|&b| (b as u32) * (b as u32)).sum();
                        acc = acc.wrapping_add(chunk_sum as u64);
                    }
                    Ok(acc.to_le_bytes().to_vec().into())
                }
                // Opcode 11: Deliberate panic with custom message
                11 => {
                    panic!("example engine induced panic");
                }
                // Opcode 12: Echo payload
                12 => Ok(input[1..].to_vec().into()),
                // Opcode 13: Reverse the payload, built directly in buffer memory.
                //
                // With Rust 1.100's stable Allocator trait the output lives in
                // `gusset::BufferAlloc` memory from the first byte, and Gusset
                // hands that allocation to Go as-is: no copy on the worker, none
                // on the cgo thread. `try_reserve` sizes it fallibly — an
                // infallible push that cannot allocate aborts the whole process.
                #[cfg(gusset_allocator_api)]
                13 => {
                    let mut out: Vec<u8, BufferAlloc> = Vec::new_in(BufferAlloc);
                    out.try_reserve_exact(input.len() - 1)
                        .map_err(|e| e.to_string())?;
                    out.extend(input[1..].iter().rev());
                    Ok(JobOutput::from(out))
                }
                // Default: unrecognized opcode
                op => Err(format!("unknown opcode: {}", op)),
            }
        },
    );
}
