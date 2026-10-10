//! Byte-level properties of the Rust side of the Go/Rust boundary, shared by
//! the seeded-loop tests in this directory (`boundary_props.rs`,
//! `log_ring_props.rs`) and the libFuzzer targets under `fuzz/`, which include
//! this file by path. Every checker takes raw fuzz bytes and panics on a
//! violated property. `drain_logs_boundaries.rs` takes its log-ring drains
//! from here too.
//!
//! What crosses the boundary here, and why each property matters:
//!
//! * The log ring (`log_event` / `gusset_drain_logs`, `append_line`,
//!   `evict_cut` and `drain_cut`): Go decodes each drained chunk as text, so a
//!   chunk must end on a line or character boundary, the ring must stay
//!   bounded, eviction must drop exactly the oldest whole lines that make room
//!   (the model evicts line by line; `evict_cut` does it in one cut), and a
//!   drain must always make progress (0 reads as "empty" and strands the ring).
//! * Panic payload truncation (`truncate_payload` via `extract_panic_payload`):
//!   the message is copied into an `FfiStatus` Go reads (I2); it must stay
//!   valid UTF-8, bounded, and a prefix of what the engine said.
//! * The completion ring (`pool::ring::Ring`): the record words Go reads must
//!   be exactly the ones published, in order, never overwritten before the
//!   consumer frees the slot.
//! * `CallHeader` (40 bytes Go writes and Rust reads) and the completion record
//!   `inline_record` builds: any header bytes are either refused cleanly
//!   (unknown flags) or run and complete exactly once with the right record.

#![allow(dead_code, unsafe_code)]

use std::collections::VecDeque;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use gusset::ffi::guard::extract_panic_payload;
use gusset::ffi::{gusset_drain_logs, log_event};
use gusset::header::{CallHeader, JobContext};
use gusset::pool::ring::{Ring, RING_SLOT_BYTES, RING_SLOT_OFF_RECORD};
use gusset::pool::{Handle, JobResult, INLINE_RECORD_FLAG, INLINE_RESULT_MAX};

/// Small deterministic PRNG (xorshift64*) for the seeded loops.
pub struct Rng(pub u64);

impl Rng {
    pub fn next(&mut self) -> u64 {
        let mut x = self.0;
        x ^= x >> 12;
        x ^= x << 25;
        x ^= x >> 27;
        self.0 = x;
        x.wrapping_mul(0x2545_F491_4F6C_DD1D)
    }

    pub fn bytes(&mut self, n: usize) -> Vec<u8> {
        (0..n).map(|_| self.next() as u8).collect()
    }
}

/// Iterations for a seeded loop: `GUSSET_PROP_ITERS` or `default`.
pub fn iters(default: usize) -> usize {
    std::env::var("GUSSET_PROP_ITERS")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(default)
}

/// Turns fuzz bytes into a string mixing 1-, 2-, 3- and 4-byte characters,
/// newlines excluded (they are line separators for the log ring).
pub fn text_from(bytes: &[u8]) -> String {
    const CHARS: [char; 8] = ['a', 'Z', '0', 'é', '€', '𝄞', 'ß', '\u{7FF}'];
    bytes.iter().map(|&b| CHARS[(b & 7) as usize]).collect()
}

// ---------------------------------------------------------------- truncation

const MAX_PANIC_PAYLOAD_BYTES: usize = 32 * 1024;
const TRUNCATED_MARK: &str = "... [truncated]";

/// `truncate_payload`, through `extract_panic_payload`: a payload within the
/// cap comes back unchanged; a longer one is its longest char-boundary prefix
/// of at most the cap, followed by the marker.
pub fn check_truncate(fill: &[u8], len_hint: u32) {
    // Stretch the fuzz bytes to straddle the cap: lengths from 0 to cap + 8.
    let target = len_hint as usize % (MAX_PANIC_PAYLOAD_BYTES + 9);
    let unit = text_from(if fill.is_empty() { &[0] } else { fill });
    let mut s = String::with_capacity(target + 4);
    while s.len() < target {
        s.push_str(&unit);
    }
    // Trim back to near the target on a char boundary.
    let mut cut = target.min(s.len());
    while !s.is_char_boundary(cut) {
        cut -= 1;
    }
    s.truncate(cut);
    check_truncate_str(s);
}

pub fn check_truncate_str(s: String) {
    let input = s.clone();
    let out = extract_panic_payload(Box::new(s));
    let out_static = describe_as_static(&input);
    assert_eq!(
        out, out_static,
        "&str and String payloads truncate differently"
    );
    if input.len() <= MAX_PANIC_PAYLOAD_BYTES {
        assert_eq!(out, input, "a payload within the cap was altered");
        return;
    }
    let prefix = out
        .strip_suffix(TRUNCATED_MARK)
        .unwrap_or_else(|| panic!("truncated payload lacks the marker ({} bytes)", out.len()));
    assert!(
        prefix.len() <= MAX_PANIC_PAYLOAD_BYTES,
        "prefix {} > cap",
        prefix.len()
    );
    assert!(
        input.starts_with(prefix),
        "output is not a prefix of the payload"
    );
    // Longest such prefix: the next char would not have fit.
    let next = input[prefix.len()..]
        .chars()
        .next()
        .map_or(0, char::len_utf8);
    assert!(
        prefix.len() + next > MAX_PANIC_PAYLOAD_BYTES,
        "cut at {} left room for the next {}-byte char",
        prefix.len(),
        next
    );
}

/// `&'static str` payloads come from `panic!("literal")`. Only the payload's
/// type matters to `describe_panic_payload`, and the box is consumed and
/// dropped before this returns, so a borrowed `&str` stands in for the
/// literal without leaking a copy per input (LeakSanitizer flags that).
fn describe_as_static(s: &str) -> String {
    // SAFETY: the extended reference lives only in a box that
    // `extract_panic_payload` drops before returning; `s` outlives the call.
    let fake: &'static str = unsafe { std::mem::transmute::<&str, &'static str>(s) };
    extract_panic_payload(Box::new(fake))
}

// ------------------------------------------------------------------ log ring

const LOG_RING_CAPACITY: usize = 65536;

/// Reference model of the log ring from its documentation (`log_event`,
/// `append_line`, `gusset_drain_logs`). It evicts one line at a time, the
/// specification `evict_cut` meets in a single cut.
#[derive(Default)]
struct LogModel {
    ring: Vec<u8>,
}

impl LogModel {
    fn append(&mut self, line: &str) {
        let max_payload = LOG_RING_CAPACITY - 1;
        let mut end = line.len().min(max_payload);
        while !line.is_char_boundary(end) {
            end -= 1;
        }
        let needed = end + 1;
        while self.ring.len() + needed > LOG_RING_CAPACITY {
            match self.ring.iter().position(|&b| b == b'\n') {
                Some(nl) => {
                    self.ring.drain(..=nl);
                }
                None => {
                    self.ring.clear();
                    break;
                }
            }
        }
        self.ring.extend_from_slice(&line.as_bytes()[..end]);
        self.ring.push(b'\n');
    }

    /// Everything that fits; else through the last newline that fits; else
    /// the longest prefix ending on a character boundary; else `cap` raw bytes.
    fn drain(&mut self, cap: usize) -> Vec<u8> {
        let n = if self.ring.len() <= cap {
            self.ring.len()
        } else if let Some(nl) = self.ring[..cap].iter().rposition(|&b| b == b'\n') {
            nl + 1
        } else {
            // A cut before byte `c` is on a boundary when ring[c] is not a
            // continuation byte.
            (1..=cap)
                .rev()
                .find(|&c| self.ring[c] & 0xC0 != 0x80)
                .unwrap_or(cap)
        };
        self.ring.drain(..n).collect()
    }
}

/// One `gusset_drain_logs` into a `cap`-byte buffer: the bytes it reported
/// writing, after checking it reported no more than `cap`.
pub fn drain_real(cap: usize) -> Vec<u8> {
    let mut buf = vec![0xEEu8; cap.max(1)];
    let mut written = usize::MAX;
    unsafe { gusset_drain_logs(buf.as_mut_ptr(), cap, &mut written) };
    assert!(written <= cap, "drain wrote {written} bytes into {cap}");
    buf.truncate(written);
    buf
}

/// Empties the process-global log ring.
pub fn drain_everything() {
    while !drain_real(1 << 17).is_empty() {}
}

/// Interleaved `log_event` and `gusset_drain_logs` with fuzz-chosen line
/// lengths and drain capacities, against the model. The ring is process
/// global: callers own the process (a dedicated test binary or fuzz target).
pub fn check_log_ring(ops: &[u8]) {
    drain_everything();
    let mut model = LogModel::default();
    let mut i = 0;
    while i + 2 < ops.len() {
        let (op, a, b) = (ops[i], ops[i + 1], ops[i + 2]);
        i += 3;
        if op & 1 == 0 {
            // Line length: mostly short, sometimes around the whole budget.
            let len = match op >> 5 {
                0..=4 => (a as usize) | ((b as usize & 1) << 8),
                5 => LOG_RING_CAPACITY - 4 + (a as usize % 8),
                6 => 20_000 + (a as usize) * 64,
                _ => (a as usize) * 256 + b as usize,
            };
            let unit = text_from(&[a, b, op, a ^ b]);
            let mut line = String::with_capacity(len + 4);
            while line.len() < len {
                line.push_str(&unit);
            }
            log_event(&line);
            model.append(&line);
        } else {
            // Drain capacity: tiny (smaller than one character) to huge.
            let cap = match op >> 5 {
                0 => 1 + (a as usize % 4),
                1..=3 => 1 + a as usize,
                4..=5 => 1 + ((a as usize) << 8 | b as usize),
                _ => 1 << 17,
            };
            let before_front_ok = model.ring.first().is_none_or(|&c| c & 0xC0 != 0x80);
            let want = model.drain(cap);
            let got = drain_real(cap);
            assert_eq!(
                got.len(),
                want.len(),
                "drain({cap}) returned {} bytes, model {}",
                got.len(),
                want.len()
            );
            assert!(got == want, "drain({cap}) bytes differ from the model");
            // Progress: a non-empty ring always hands over something.
            if got.is_empty() {
                assert!(
                    model.ring.is_empty(),
                    "drain({cap}) returned 0 with bytes buffered"
                );
            }
            // Whole characters whenever the cap can hold one and the ring was
            // not already left mid-character by an earlier sub-character cap.
            if cap >= 4 && before_front_ok {
                assert!(
                    std::str::from_utf8(&got).is_ok(),
                    "drain({cap}) split a UTF-8 character"
                );
            }
        }
    }
    // Final: the rest drains exactly and the ring is bounded.
    let rest = std::mem::take(&mut model.ring);
    assert!(rest.len() <= LOG_RING_CAPACITY);
    let mut got = Vec::new();
    loop {
        let chunk = drain_real(1 << 17);
        if chunk.is_empty() {
            break;
        }
        got.extend(chunk);
    }
    assert!(
        got == rest,
        "final drain differs: {} vs {} bytes",
        got.len(),
        rest.len()
    );
}

// ------------------------------------------------------------ completion ring

/// Reads the next published record the way the Go reader does (gusset.h).
fn ring_pop(ring: &Ring, head: &mut u64) -> Option<[u64; 8]> {
    let cap = ring.shared().capacity;
    let base = ring.slots_ptr() as *const u8;
    let slot = unsafe { base.add(((*head & (cap - 1)) as usize) * RING_SLOT_BYTES) };
    let seq = unsafe { &*(slot as *const AtomicU64) };
    if seq.load(Ordering::Acquire) != *head + 1 {
        return None;
    }
    let mut words = [0u64; 8];
    for (i, w) in words.iter_mut().enumerate() {
        let a = unsafe { &*(slot.add(RING_SLOT_OFF_RECORD + i * 8) as *const AtomicU64) };
        *w = a.load(Ordering::Relaxed);
    }
    seq.store(*head + cap, Ordering::Release);
    *head += 1;
    Some(words)
}

/// Encodes a completion record from gusset.h: `ticket | flag`, `len`, data
/// padded to 8 — or a bare ticket.
pub fn encode_record(ticket: u64, data: Option<&[u8]>) -> Vec<u8> {
    let mut out = Vec::with_capacity(64);
    match data {
        None => out.extend_from_slice(&ticket.to_ne_bytes()),
        Some(d) => {
            out.extend_from_slice(&(ticket | INLINE_RECORD_FLAG).to_ne_bytes());
            out.extend_from_slice(&(d.len() as u64).to_ne_bytes());
            out.extend_from_slice(d);
            while out.len() % 8 != 0 {
                out.push(0);
            }
        }
    }
    out
}

/// A native-endian u64 from exactly 8 bytes.
fn word(b: &[u8]) -> u64 {
    let mut w = [0u8; 8];
    w.copy_from_slice(b);
    u64::from_ne_bytes(w)
}

fn words_of(record: &[u8]) -> Vec<u64> {
    record.chunks(8).map(word).collect()
}

/// Publish/consume sequences on a small ring against a FIFO of capacity
/// `capacity`: a publish succeeds exactly when the model has room, and every
/// consumed record is exactly the oldest published one.
pub fn check_ring(min_slots: u8, ops: &[u8]) {
    let ring = Ring::new(min_slots as usize % 9);
    let cap = ring.shared().capacity as usize;
    assert!(cap.is_power_of_two() && cap >= 2 && cap >= min_slots as usize % 9);
    let mut model: VecDeque<Vec<u64>> = VecDeque::new();
    let mut head = 0u64;
    let mut next_ticket = 1u64;
    for &op in ops {
        if op & 1 == 0 {
            let n = (op >> 1) as usize % (INLINE_RESULT_MAX + 2);
            let rec = if n == INLINE_RESULT_MAX + 1 {
                encode_record(next_ticket, None)
            } else {
                let data: Vec<u8> = (0..n).map(|i| op.wrapping_add(i as u8)).collect();
                encode_record(next_ticket, Some(&data))
            };
            next_ticket += 1;
            let ok = ring.try_publish(&rec);
            assert_eq!(
                ok,
                model.len() < cap,
                "publish with {} of {cap} queued",
                model.len()
            );
            if ok {
                model.push_back(words_of(&rec));
            }
        } else {
            let got = ring_pop(&ring, &mut head);
            match model.pop_front() {
                None => assert!(got.is_none(), "popped a record from an empty ring"),
                Some(want) => {
                    let Some(got) = got else {
                        panic!("published record not visible")
                    };
                    assert_eq!(&got[..want.len()], &want[..], "record words differ");
                }
            }
        }
    }
}

// ------------------------------------------------------ header and records

/// A header from 40 fuzz bytes (every bit pattern is a valid `CallHeader`).
pub fn header_from(bytes: &[u8]) -> CallHeader {
    let mut raw = [0u8; 40];
    let n = bytes.len().min(40);
    raw[..n].copy_from_slice(&bytes[..n]);
    unsafe { std::ptr::read_unaligned(raw.as_ptr() as *const CallHeader) }
}

/// `JobContext` from any header: never panics; opcode is the raw `reserved`;
/// timeout 0 is no deadline; a non-zero timeout is a deadline (expired at
/// once when it cannot be represented), never "no deadline" (I3).
pub fn check_job_context(h: CallHeader) {
    let flag = Arc::new(AtomicBool::new(false));
    let ctx = JobContext::new(h, Arc::clone(&flag));
    assert_eq!(ctx.opcode(), h.reserved);
    assert_eq!(*ctx.header(), h);
    let r = ctx.check();
    if h.timeout_ns == 0 || h.timeout_ns > 60_000_000_000 {
        assert!(r.is_ok(), "timeout {} expired at once: {r:?}", h.timeout_ns);
    }
    if h.timeout_ns != 0 && h.timeout_ns <= 1000 {
        std::thread::sleep(Duration::from_micros(2));
        assert!(
            ctx.check().is_err(),
            "a {} ns deadline never expired",
            h.timeout_ns
        );
    }
    flag.store(true, Ordering::Release);
    assert!(ctx.check().is_err(), "cancel flag ignored");
}

struct Rig {
    handle: Arc<Handle>,
    read_fd: i32,
    ring: Option<Arc<Ring>>,
    head: u64,
}

fn rig(with_ring: bool) -> &'static Mutex<Rig> {
    static PIPE_RIG: OnceLock<Mutex<Rig>> = OnceLock::new();
    static RING_RIG: OnceLock<Mutex<Rig>> = OnceLock::new();
    let cell = if with_ring { &RING_RIG } else { &PIPE_RIG };
    cell.get_or_init(|| {
        let mut fds = [0i32; 2];
        assert_eq!(unsafe { libc::pipe(fds.as_mut_ptr()) }, 0);
        let handle = Handle::open(1, fds[1]).unwrap_or_else(|e| panic!("open: {e}"));
        let ring = with_ring.then(|| handle.attach_ring().unwrap_or_else(|e| panic!("ring: {e}")));
        Mutex::new(Rig {
            handle,
            read_fd: fds[0],
            ring,
            head: 0,
        })
    })
}

/// Reads exactly `n` bytes from the completion pipe, failing after 10 s.
fn read_exact(fd: i32, n: usize) -> Vec<u8> {
    let mut out = vec![0u8; n];
    let mut got = 0;
    let deadline = Instant::now() + Duration::from_secs(10);
    while got < n {
        let mut pfd = libc::pollfd {
            fd,
            events: libc::POLLIN,
            revents: 0,
        };
        let left = deadline.saturating_duration_since(Instant::now());
        assert!(!left.is_zero(), "no completion record within 10 s");
        let rc = unsafe { libc::poll(&mut pfd, 1, left.as_millis() as i32 + 1) };
        if rc <= 0 {
            continue;
        }
        let r = unsafe { libc::read(fd, out[got..].as_mut_ptr().cast(), n - got) };
        assert!(r > 0, "completion pipe read returned {r}");
        got += r as usize;
    }
    out
}

/// The next completion as `(ticket, inline data)`, from the ring when one is
/// attached (Go never announces a park here, so no wake tokens), else from
/// the pipe.
fn next_completion(rig: &mut Rig) -> (u64, Option<Vec<u8>>) {
    let words: Vec<u64> = if let Some(ring) = rig.ring.clone() {
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            if let Some(w) = ring_pop(&ring, &mut rig.head) {
                break w.to_vec();
            }
            assert!(Instant::now() < deadline, "no ring completion within 10 s");
            std::thread::yield_now();
        }
    } else {
        let w0 = word(&read_exact(rig.read_fd, 8));
        if w0 & INLINE_RECORD_FLAG == 0 {
            vec![w0]
        } else {
            let n = word(&read_exact(rig.read_fd, 8));
            assert!(n as usize <= INLINE_RESULT_MAX, "inline length {n} > max");
            let body = read_exact(rig.read_fd, (n as usize).div_ceil(8) * 8);
            let mut w = vec![w0, n];
            w.extend(words_of(&body));
            w
        }
    };
    let w0 = words[0];
    if w0 & INLINE_RECORD_FLAG == 0 {
        return (w0, None);
    }
    let n = words[1] as usize;
    assert!(n <= INLINE_RESULT_MAX, "inline length {n} > max");
    let bytes: Vec<u8> = words[2..].iter().flat_map(|w| w.to_ne_bytes()).collect();
    let padded = n.div_ceil(8) * 8;
    assert!(
        bytes[n..padded].iter().all(|&b| b == 0),
        "inline record padding is not zero"
    );
    (w0 & !INLINE_RECORD_FLAG, Some(bytes[..n].to_vec()))
}

/// Any header bytes, submitted on a real handle with an echo payload: unknown
/// flag bits are refused; everything else completes exactly once, inline iff
/// the caller asked for records and the result fits, with the exact bytes.
pub fn check_submit(header_bytes: &[u8], payload: &[u8], with_ring: bool) {
    let mut h = header_from(header_bytes);
    // Keep the flags space interesting: mostly known bits, sometimes others.
    if header_bytes.get(40).is_none_or(|b| b & 7 != 0) {
        h.flags &= 3;
    }
    // Long deadlines only: a short one races the pre-dispatch check, which is
    // correct behaviour but not what this property is about.
    if h.timeout_ns != 0 && h.timeout_ns < 1_000_000_000 {
        h.timeout_ns = 0;
    }
    // Opcode 0 half the time, so the diagnostic engine actually runs.
    if header_bytes.get(40).is_none_or(|b| b & 8 == 0) {
        h.reserved = 0;
    }
    let mut input = payload[..payload.len().min(200)].to_vec();
    if let Some(first) = input.first_mut() {
        *first = 0; // diagnostic echo
    }
    check_job_context(h);

    let mut rig = rig(with_ring).lock().unwrap_or_else(|e| e.into_inner());
    let ticket = match rig.handle.submit(h, &input, 0) {
        Err(e) => {
            assert!(h.flags & !3 != 0, "known flags {:#x} refused: {e}", h.flags);
            assert!(e.contains("unknown CallHeader flag"), "{e}");
            return;
        }
        Ok(t) => {
            assert!(h.flags & !3 == 0, "unknown flags {:#x} accepted", h.flags);
            t
        }
    };
    assert!(
        ticket != 0 && ticket & INLINE_RECORD_FLAG == 0,
        "bad ticket {ticket:#x}"
    );
    // The diagnostic engine is the opcode-0 fallback only (default_dispatch).
    let diag = h.flags & 1 != 0 && h.reserved == 0;
    let wants_inline = h.flags & 2 != 0;
    let (got, inline) = next_completion(&mut rig);
    assert_eq!(got, ticket, "completion for another ticket");
    let expect_inline = diag && wants_inline && input.len() <= INLINE_RESULT_MAX;
    match inline {
        Some(data) => {
            assert!(
                expect_inline,
                "unexpected inline record ({} bytes)",
                data.len()
            );
            assert_eq!(data, input, "inline echo differs");
            assert!(
                rig.handle.take(ticket).is_err(),
                "inline result also stored for take"
            );
        }
        None => {
            assert!(!expect_inline, "a {}-byte echo came back bare", input.len());
            match rig.handle.take(ticket) {
                Ok(JobResult::Ok(out)) => {
                    assert!(diag, "ran without an engine");
                    assert_eq!(out, input, "stored echo differs");
                }
                Ok(JobResult::Err(e)) => {
                    assert!(!diag, "diagnostic echo failed: {e}");
                    assert!(e.contains("no engine handler registered"), "{e}");
                    if h.reserved != 0 {
                        assert!(e.contains(&format!("opcode {}", h.reserved)), "{e}");
                    }
                }
                Ok(_) => panic!("unexpected result kind for a small echo"),
                Err(e) => panic!("take: {e}"),
            }
            assert!(rig.handle.take(ticket).is_err(), "result taken twice");
        }
    }
}
