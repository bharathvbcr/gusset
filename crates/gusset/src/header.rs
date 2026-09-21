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
    /// Engine dispatch opcode (`WithOpcode` / `ContextWithOpcode`). Zero means
    /// the global handler. The field still pads the header to 40 bytes.
    pub reserved: u32,
}

// Invariant: CallHeader must be exactly 40 bytes with 8-byte alignment (R9).
assert_eq_size!(CallHeader, [u8; 40]);
assert_eq_align!(CallHeader, u64);

/// Header flag: the caller explicitly opts this submission into the built-in
/// diagnostic engine (the panic zoo and pitfall work units).
///
/// The diagnostic engine selects its behaviour from the first input byte, so it
/// must never be reachable from untrusted payload data. Gusset therefore runs it
/// only when the *caller* sets this bit in `CallHeader::flags` and no real engine
/// handler is registered. A production caller never sets it, so a hostile first
/// byte cannot steer a submission into a panic.
pub const GUSSET_FLAG_DIAGNOSTIC_ENGINE: u32 = 1 << 0;

/// Mask of every `flags` bit this ABI version understands.
///
/// A submission carrying an unknown bit is rejected rather than silently ignored,
/// so a newer caller cannot believe a flag took effect against an older library.
pub const GUSSET_FLAGS_KNOWN: u32 = GUSSET_FLAG_DIAGNOSTIC_ENGINE;

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
    dequeued_at: Option<Instant>,
    finished_at: Option<Instant>,
}

impl JobContext {
    /// Creates a new job context with submit instant and cancel flag.
    pub fn new(header: CallHeader, cancel_flag: Arc<AtomicBool>) -> Self {
        let submit_instant = Instant::now();
        let deadline = resolve_deadline(submit_instant, header.timeout_ns);
        Self {
            header,
            submit_instant,
            deadline,
            cancel_flag,
            dequeued_at: None,
            finished_at: None,
        }
    }

    /// Returns a reference to the call header.
    pub fn header(&self) -> &CallHeader {
        &self.header
    }

    /// Returns the engine dispatch opcode passed in CallHeader.reserved (R9).
    pub fn opcode(&self) -> u32 {
        self.header.reserved
    }

    /// Marks the instant when this job was dequeued by a worker thread.
    pub fn mark_dequeued(&mut self) {
        self.dequeued_at = Some(Instant::now());
    }

    /// Marks the instant when job execution finished on the worker thread.
    pub fn mark_finished(&mut self) {
        self.finished_at = Some(Instant::now());
    }

    /// Time spent waiting in the worker queue before execution began.
    pub fn queue_delay(&self) -> Option<Duration> {
        self.dequeued_at
            .map(|d| d.saturating_duration_since(self.submit_instant))
    }

    /// Duration of engine compute on the worker thread.
    pub fn compute_duration(&self) -> Option<Duration> {
        match (self.dequeued_at, self.finished_at) {
            (Some(d), Some(f)) => Some(f.saturating_duration_since(d)),
            _ => None,
        }
    }

    /// Total duration elapsed since job submission.
    pub fn total_duration(&self) -> Duration {
        match self.finished_at {
            Some(f) => f.saturating_duration_since(self.submit_instant),
            None => Instant::now().saturating_duration_since(self.submit_instant),
        }
    }

    /// Checks whether the job has been cancelled or exceeded its deadline (R9).
    pub fn check(&self) -> Result<(), CancelReason> {
        if self.cancel_flag.load(Ordering::Acquire) {
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

/// Turns a relative `timeout_ns` into a Rust `Instant` deadline.
///
/// A non-zero timeout must never become "no deadline": `Instant::checked_add`
/// returns `None` when the duration cannot be represented, and treating that as
/// `None` made `u64::MAX` nanoseconds mean "run forever" instead of "already
/// expired". Overflow expires immediately by using the submit instant.
fn resolve_deadline(submit_instant: Instant, timeout_ns: u64) -> Option<Instant> {
    if timeout_ns == 0 {
        None
    } else {
        Some(
            submit_instant
                .checked_add(Duration::from_nanos(timeout_ns))
                .unwrap_or(submit_instant),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicBool;

    /// Miri runs these: they are pure, allocation-light, and free of FFI and
    /// threads. The nightly Miri job used to invoke `cargo miri test -p gusset
    /// --lib alloc header`, but the library carried no unit tests at all, so it
    /// reported success having checked nothing.
    #[test]
    fn header_layout_is_the_documented_40_bytes() {
        assert_eq!(core::mem::size_of::<CallHeader>(), 40);
        assert_eq!(core::mem::align_of::<CallHeader>(), 8);
        // Reserved exists to pad to 40; a default header must be all zeros so an
        // unset field never reads as a set flag on the other side.
        let h = CallHeader::default();
        assert_eq!(h.flags, 0);
        assert_eq!(h.reserved, 0);
        assert_eq!(h.timeout_ns, 0);
        assert_eq!(h.trace_id, [0u8; 16]);
        assert_eq!(h.span_id, [0u8; 8]);
    }

    #[test]
    fn diagnostic_flag_is_bit_zero_and_is_the_only_known_bit() {
        assert_eq!(GUSSET_FLAG_DIAGNOSTIC_ENGINE, 1);
        assert_eq!(GUSSET_FLAGS_KNOWN, GUSSET_FLAG_DIAGNOSTIC_ENGINE);
        // Anything outside the known mask must be detectable as unknown.
        assert_ne!((1u32 << 31) & !GUSSET_FLAGS_KNOWN, 0);
    }

    #[test]
    fn zero_timeout_means_no_deadline() {
        let ctx = JobContext::new(CallHeader::default(), Arc::new(AtomicBool::new(false)));
        assert_eq!(ctx.check(), Ok(()));
        assert!(ctx.header().timeout_ns == 0);
    }

    #[test]
    fn expired_relative_timeout_reports_deadline_exceeded() {
        let header = CallHeader {
            timeout_ns: 1, // one nanosecond: expired by the time it is checked
            ..Default::default()
        };
        let ctx = JobContext::new(header, Arc::new(AtomicBool::new(false)));
        // Burn a little real time without sleeping, so this stays fast under Miri.
        for _ in 0..1000 {
            core::hint::black_box(());
        }
        assert_eq!(ctx.check(), Err(CancelReason::DeadlineExceeded));
    }

    #[test]
    fn cancel_flag_outranks_an_unexpired_deadline() {
        let flag = Arc::new(AtomicBool::new(false));
        let header = CallHeader {
            timeout_ns: 60_000_000_000, // 60s: nowhere near expiry
            ..Default::default()
        };
        let ctx = JobContext::new(header, Arc::clone(&flag));
        assert_eq!(ctx.check(), Ok(()));

        flag.store(true, Ordering::Release);
        assert_eq!(
            ctx.check(),
            Err(CancelReason::Explicit),
            "an explicit cancel must be reported as Explicit, not as a timeout"
        );
    }

    #[test]
    fn nonzero_timeout_always_installs_a_deadline() {
        // `Instant::checked_add` returning None used to be stored as "no
        // deadline", so a timeout the clock cannot represent ran forever.
        assert!(
            resolve_deadline(Instant::now(), 0).is_none(),
            "timeout_ns == 0 is the documented no-deadline encoding"
        );
        assert!(
            resolve_deadline(Instant::now(), 1).is_some(),
            "a one-nanosecond timeout must still be a deadline"
        );
        assert!(
            resolve_deadline(Instant::now(), u64::MAX).is_some(),
            "an unrepresentable timeout must expire, not disable the deadline"
        );

        let header = CallHeader {
            timeout_ns: u64::MAX,
            ..Default::default()
        };
        let ctx = JobContext::new(header, Arc::new(AtomicBool::new(false)));
        assert!(
            ctx.deadline.is_some(),
            "JobContext must not drop a non-zero timeout_ns on Instant overflow"
        );
    }
}
