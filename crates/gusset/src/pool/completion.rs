//! Completion records: building, writing and publishing them (I4).

use super::{lock_recover, ring, sys, Handle, JobResult};
use std::sync::atomic::{AtomicBool, AtomicI32, Ordering};
use std::sync::{Arc, Mutex};

/// Writes a completion ticket, sleeping on a full pipe outside the exclusivity lock.
///
/// Retries until the write lands, the handle closes, or the pipe reports a hard
/// error (EPIPE, EBADF). It used to give up after 10 s and set the ticket aside
/// for a later submit or completion to retry, but Go holds a pool permit until
/// each ticket is delivered: once every permit was stranded that way, no submit
/// could reach Rust and no completion was left to retry them, so the waiters
/// hung forever even after the reader recovered. The worker holding the ticket
/// is the one thing guaranteed to still be there, so it keeps trying; `close`
/// sets `closed` first, so it never waits on this loop for longer than one
/// backoff step.
///
/// The descriptor is read under `lock` on every attempt, and `close` swaps it to
/// -1 and closes it under the same lock: a write can never land on a number
/// `close` has already released and the process has reused for another file.
fn write_completion(
    lock: &Mutex<()>,
    fd: &AtomicI32,
    closed: &AtomicBool,
    ticket: u64,
    record: &[u8],
) -> std::io::Result<()> {
    let started = std::time::Instant::now();
    let mut warned = false;
    let mut backoff = std::time::Duration::from_micros(50);
    loop {
        let attempt = {
            let _guard = lock_recover(lock);
            match fd.load(Ordering::Acquire) {
                -1 => Err(std::io::Error::new(
                    std::io::ErrorKind::NotConnected,
                    "handle closed before the completion was written",
                )),
                raw => sys::write_record_attempt(raw, record),
            }
        };
        match attempt {
            Ok(()) => return Ok(()),
            Err(err) if err.kind() == std::io::ErrorKind::WouldBlock => {
                if closed.load(Ordering::Acquire) {
                    return Err(std::io::Error::new(
                        std::io::ErrorKind::NotConnected,
                        "handle closed while the completion pipe was full",
                    ));
                }
                if !warned && started.elapsed() >= sys::WRITE_TICKET_TIMEOUT {
                    warned = true;
                    crate::ffi::log_event(&format!(
                        "gusset: completion pipe full for {:?}; reader is not draining \
                         (ticket {} still waiting)",
                        sys::WRITE_TICKET_TIMEOUT,
                        ticket
                    ));
                }
                std::thread::sleep(backoff);
                backoff = (backoff * 2).min(sys::WRITE_TICKET_MAX_BACKOFF);
            }
            Err(err) => return Err(err),
        }
    }
}

/// Largest successful result carried inline in the completion pipe
/// (see `GUSSET_FLAG_INLINE_COMPLETION`). With the ticket and length words a
/// record is at most [`INLINE_RECORD_MAX`] bytes, far below `PIPE_BUF`, so a
/// record is written atomically and records never interleave.
pub const INLINE_RESULT_MAX: usize = 48;

/// Set on a record's first word when an inline result follows. Tickets stay
/// below 2^63 (`ID_CEILING`), so the bit never collides with a ticket.
pub const INLINE_RECORD_FLAG: u64 = 1 << 63;

/// Largest completion record: ticket word, length word, payload padded to 8.
pub const INLINE_RECORD_MAX: usize = 16 + INLINE_RESULT_MAX;

/// Builds an inline completion record into `buf`, returning its length.
///
/// Layout, native-endian u64 words: `ticket | INLINE_RECORD_FLAG`, `len`, then
/// `len` bytes zero-padded to a multiple of 8.
fn inline_record(buf: &mut [u8; INLINE_RECORD_MAX], ticket: u64, data: &[u8]) -> usize {
    buf[..8].copy_from_slice(&(ticket | INLINE_RECORD_FLAG).to_ne_bytes());
    buf[8..16].copy_from_slice(&(data.len() as u64).to_ne_bytes());
    let padded = data.len().div_ceil(8) * 8;
    buf[16..16 + data.len()].copy_from_slice(data);
    buf[16 + data.len()..16 + padded].fill(0);
    16 + padded
}

/// Sizes the completion pipe for `pool_size` unread completions and reports
/// whether inline records fit (`Ok(true)`) or only bare tickets do.
///
/// `grow` makes the pipe hold at least the given number of bytes. Where the
/// capacity can be queried and grown (`can_grow`, Linux), records are tried
/// first and tickets are the fallback; only a pipe too small for tickets is an
/// error. Elsewhere nothing can be grown, and every BSD-derived pipe holds at
/// least 16 KiB, so records are allowed only while they fit there.
pub(super) fn size_completion_pipe(
    pool_size: usize,
    can_grow: bool,
    grow: impl Fn(usize) -> Result<(), String>,
) -> Result<bool, String> {
    let records = pool_size.saturating_mul(INLINE_RECORD_MAX);
    if !can_grow {
        grow(pool_size.saturating_mul(8))?;
        return Ok(records <= 16 * 1024);
    }
    if grow(records).is_ok() {
        return Ok(true);
    }
    grow(pool_size.saturating_mul(8))?;
    Ok(false)
}

impl Handle {
    /// Attaches the shared-memory completion ring (see [`ring`]). From here
    /// on completions are published there, and the pipe carries only wake
    /// tokens and overflow. Refused if a ring is already attached: there is
    /// exactly one reader.
    pub fn attach_ring(&self) -> Result<Arc<ring::Ring>, String> {
        let r = Arc::new(ring::Ring::new(self.pool_size));
        self.ring
            .set(Arc::clone(&r))
            .map_err(|_| "a completion ring is already attached".to_string())?;
        Ok(r)
    }

    /// Hands a finished job's outcome to the reader: in the ring if one is
    /// attached and has room, else through the pipe. A success small enough,
    /// for a caller that asked for inline records, travels in the record and
    /// is never stored; anything else is stored for `take` and announced by
    /// a bare ticket.
    pub(super) fn complete(&self, ticket: u64, result: JobResult, wants_inline: bool) {
        let ring = self.ring.get();
        let mut record = [0u8; INLINE_RECORD_MAX];
        let (len, stored) = match result {
            // A ring slot holds a full record whatever the pipe could, so the
            // ring does not depend on inline_ok; only its overflow does.
            JobResult::Ok(ref data)
                if wants_inline
                    && (self.inline_ok || ring.is_some())
                    && data.len() <= INLINE_RESULT_MAX =>
            {
                (inline_record(&mut record, ticket, data), Some(result))
            }
            other => {
                lock_recover(&self.results).insert(ticket, other);
                record[..8].copy_from_slice(&ticket.to_ne_bytes());
                (8, None)
            }
        };
        let Some(r) = ring else {
            self.publish_completion(ticket, &record[..len]);
            return;
        };
        if r.try_publish(&record[..len]) {
            if r.take_waiter() {
                // Ticket 0 is never issued: the reader drops it as a wake.
                self.publish_completion(ticket, &0u64.to_ne_bytes());
            }
            return;
        }
        // Full, which the Go side's permits rule out: fall back to the pipe.
        // An inline record the pipe was not sized for becomes a stored result.
        let landed = if len > 8 && !self.inline_ok {
            if let Some(res) = stored {
                lock_recover(&self.results).insert(ticket, res);
            }
            self.publish_completion(ticket, &ticket.to_ne_bytes())
        } else {
            self.publish_completion(ticket, &record[..len])
        };
        // Counted only once the record is in the pipe. The reader polls,
        // rather than parks, while the counter is ahead of what it has read,
        // so a spill abandoned on close (or lost to a hard error) counted here
        // kept it spinning until the descriptor was finally closed.
        if landed {
            r.note_overflow();
        }
    }

    /// Writes a completion ticket (see [`write_completion`] for the retry rule).
    ///
    /// Returns whether the record reached the pipe.
    fn publish_completion(&self, ticket: u64, record: &[u8]) -> bool {
        if let Err(e) = write_completion(
            &self.pipe_write_lock,
            &self.pipe_write_fd,
            &self.closed,
            ticket,
            record,
        ) {
            if e.kind() == std::io::ErrorKind::NotConnected {
                return false; // closing: the waiter is told "closed" by Go
            }
            crate::ffi::log_event(&format!(
                "gusset: completion write failed for ticket {}: {}",
                ticket, e
            ));
            // EPIPE/EBADF: the reader is gone for good. Refuse new work
            // instead of accepting jobs whose completions cannot be delivered.
            self.poisoned.store(true, Ordering::Release);
            return false;
        }
        true
    }
}
