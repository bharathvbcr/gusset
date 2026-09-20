//! Reference engine demonstrating how an adopter uses the Gusset runtime contract.
//!
//! Demonstrates engine handler registration, cooperative cancellation checks,
//! safe buffer mutations, and deliberate failure modes for testing.

#![deny(missing_docs)]

use gusset::{set_engine_handler, CancelReason, JobContext};

/// Initializes the example engine and registers its execution handler with Gusset.
pub fn init_example_engine() {
    set_engine_handler(|ctx: &JobContext, input: &[u8]| -> Result<Vec<u8>, String> {
        if input.is_empty() {
            return Ok(Vec::new());
        }

        match input[0] {
            // Opcode 10: Vector sum-and-square computation
            10 => {
                let mut acc = 0u64;
                for (i, &b) in input[1..].iter().enumerate() {
                    // Check cancellation periodically (I3, R9)
                    if i % 1024 == 0 {
                        ctx.check().map_err(|e| match e {
                            CancelReason::Explicit => "cancelled explicitly".to_string(),
                            CancelReason::DeadlineExceeded => "deadline exceeded".to_string(),
                        })?;
                    }
                    let val = b as u64;
                    acc = acc.wrapping_add(val.wrapping_mul(val));
                }
                Ok(acc.to_le_bytes().to_vec())
            }
            // Opcode 11: Deliberate panic with custom message
            11 => {
                panic!("example engine induced panic");
            }
            // Opcode 12: Echo payload
            12 => Ok(input[1..].to_vec()),
            // Default: unrecognized opcode
            op => Err(format!("unknown opcode: {}", op)),
        }
    });
}
