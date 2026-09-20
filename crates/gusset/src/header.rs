//! CallHeader and job context tracking trace context, timeout, and cancellation.

use static_assertions::{assert_eq_align, assert_eq_size};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

/// 40-byte call header passed from Go to Rust across FFI.
///
/// Contains trace ID, span ID, relative timeout in nanoseconds, flags, and reserved space.
#[repr(C)]
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct CallHeader {
    /// OpenTelemetry 16-byte trace ID (or zero if absent).
    pub trace_id: [u8; 16],
    /// OpenTelemetry 8-byte span ID (or zero if absent).
    pub span_id: [u8; 8],
    /// Relative timeout in nanoseconds from submission. 0 means no timeout.
    pub timeout_ns: u64,
    /// Bit flags for call options.
    pub flags: u32,
    /// Reserved for 64-bit alignment and future extensions.
    pub reserved: u32,
}

// Invariant: CallHeader must be exactly 40 bytes with 8-byte alignment (R9).
assert_eq_size!(CallHeader, [u8; 40]);
assert_eq_align!(CallHeader, u64);

/// Cancellation or timeout reason.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CancelReason {
    /// Explicit cancellation requested by caller.
    Explicit,
    /// Deadline exceeded relative to submit instant.
    DeadlineExceeded,
}

/// Context for a job executing in the worker pool.
#[derive(Debug, Clone)]
pub struct JobContext {
    header: CallHeader,
    submit_instant: Instant,
    deadline: Option<Instant>,
    cancel_flag: Arc<AtomicBool>,
}

impl JobContext {
    /// Creates a new job context with submit instant and cancel flag.
    pub fn new(header: CallHeader, cancel_flag: Arc<AtomicBool>) -> Self {
        let submit_instant = Instant::now();
        let deadline = if header.timeout_ns > 0 {
            submit_instant.checked_add(Duration::from_nanos(header.timeout_ns))
        } else {
            None
        };
        Self {
            header,
            submit_instant,
            deadline,
            cancel_flag,
        }
    }

    /// Returns a reference to the call header.
    pub fn header(&self) -> &CallHeader {
        &self.header
    }

    /// Checks whether the job has been cancelled or exceeded its deadline (R9).
    pub fn check(&self) -> Result<(), CancelReason> {
        if self.cancel_flag.load(Ordering::Relaxed) {
            return Err(CancelReason::Explicit);
        }
        if let Some(dl) = self.deadline {
            if Instant::now() >= dl {
                return Err(CancelReason::DeadlineExceeded);
            }
        }
        Ok(())
    }

    /// Returns the submission instant.
    pub fn submit_instant(&self) -> Instant {
        self.submit_instant
    }
}
