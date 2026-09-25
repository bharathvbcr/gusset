//! Worker pool and Handle lifecycle with bounded concurrency and signal protection (I4, I5).

pub mod queue;
pub mod sys;

use crate::ffi::guard::{
    drop_panic_payload, extract_panic_payload, install_panic_hook, take_panic_location,
};
use crate::header::{
    CallHeader, CancelReason, JobContext, GUSSET_FLAGS_KNOWN, GUSSET_FLAG_DIAGNOSTIC_ENGINE,
};
use queue::{JobQueue, PushError, QueueSender};
use std::collections::HashMap;
use std::hash::{BuildHasherDefault, Hasher};
use std::panic::{catch_unwind, AssertUnwindSafe};
use std::sync::atomic::{AtomicBool, AtomicI32, AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, RwLock, Weak};
use std::thread;
use sys::RawBuffer;

/// Hasher for ids Gusset assigns itself (tickets, buffer ids, opcodes).
///
/// std's default SipHash-1-3 exists to resist HashDoS, and it cost ~470
/// instructions of a ~3,900-instruction round trip (callgrind) across the
/// results/cancel-flag inserts, lookups and removes each call makes. The keys
/// here are not attacker-chosen: tickets and buffer ids come from Rust's own
/// counters, and Go can only look them up. A Fibonacci multiply spreads
/// sequential ids across hashbrown's high control bits and low bucket bits.
#[derive(Default, Clone, Copy)]
pub(crate) struct IdHasher(u64);

impl Hasher for IdHasher {
    #[inline]
    fn finish(&self) -> u64 {
        self.0
    }
    #[inline]
    fn write(&mut self, bytes: &[u8]) {
        // Not used by u64/u32 keys; a correct fallback for anything else.
        for &b in bytes {
            self.0 = (self.0.rotate_left(8) ^ b as u64).wrapping_mul(0x9E37_79B9_7F4A_7C15);
        }
    }
    #[inline]
    fn write_u64(&mut self, n: u64) {
        self.0 = n.wrapping_mul(0x9E37_79B9_7F4A_7C15);
    }
    #[inline]
    fn write_u32(&mut self, n: u32) {
        self.write_u64(n as u64);
    }
}

/// A map keyed by a Gusset-assigned id.
pub(crate) type IdMap<K, V> = HashMap<K, V, BuildHasherDefault<IdHasher>>;

/// Largest input carried inside the work unit itself, with no heap allocation.
///
/// Request/response payloads are usually tiny (an opcode, a key, a few
/// fields); each used to cost a `to_vec` malloc on submit and a free on the
/// worker, ~600 instructions of allocator work per call with the result's.
const SMALL_INPUT: usize = 56;

/// Task payload for work units (R16 zero-copy guarantee).
enum TaskPayload {
    /// Up to SMALL_INPUT bytes, copied into the unit: no allocation.
    Small { len: u8, bytes: [u8; SMALL_INPUT] },
    /// Inlined payload for small inputs (<= 4 KiB).
    Inline(Vec<u8>),
    /// Reference-counted shared buffer for large inputs (> 4 KiB).
    Shared(Arc<RawBuffer>),
}

/// Work unit sent to worker threads.
struct WorkUnit {
    ticket: u64,
    ctx: JobContext,
    payload: TaskPayload,
    /// Buffer id this unit's input came from, or 0 for an inline payload.
    ///
    /// Carried so the worker can refuse an engine that returns its own input as
    /// its output. `TaskPayload::Shared` holds an `Arc`, not the id, and the id
    /// is what Go frees.
    input_buffer_id: u64,
}

/// Output produced by an engine execution.
///
/// `#[non_exhaustive]` because the set of variants depends on the compiler:
/// `Allocated` exists only with the stable `Allocator` trait (Rust 1.100+). An
/// exhaustive `match` in an adopter's crate would otherwise compile on one
/// toolchain and fail with E0004 on the next, with no change to either crate.
#[derive(Debug, Clone)]
#[non_exhaustive]
pub enum JobOutput {
    /// Standard vector of output bytes.
    Bytes(Vec<u8>),
    /// Pre-allocated or engine-allocated Rust buffer ID for zero-copy egress (R16).
    Buffer(u64),
    /// Output built directly in buffer memory (Rust 1.100+, stable `Allocator`).
    ///
    /// Adopted as the result buffer without a copy: the bytes Go reads are the
    /// ones the engine wrote. See [`crate::alloc::BufferAlloc`].
    #[cfg(gusset_allocator_api)]
    Allocated(Vec<u8, crate::alloc::BufferAlloc>),
}

#[cfg(gusset_allocator_api)]
impl From<Vec<u8, crate::alloc::BufferAlloc>> for JobOutput {
    fn from(v: Vec<u8, crate::alloc::BufferAlloc>) -> Self {
        JobOutput::Allocated(v)
    }
}

impl From<Vec<u8>> for JobOutput {
    fn from(v: Vec<u8>) -> Self {
        JobOutput::Bytes(v)
    }
}

impl From<u64> for JobOutput {
    fn from(buf_id: u64) -> Self {
        JobOutput::Buffer(buf_id)
    }
}

/// Result of job execution.
#[derive(Debug)]
pub enum JobResult {
    /// Succeeded with output bytes.
    Ok(Vec<u8>),
    /// Succeeded with a Rust-owned buffer id (zero-copy egress, R16).
    Buffer(u64),
    /// Returned error.
    Err(String),
    /// Panic caught by firewall with source location.
    Panic {
        /// Panic message string.
        msg: String,
        /// Source file path where panic occurred.
        file: Option<&'static str>,
        /// Source line number where panic occurred.
        line: u32,
    },
    /// Job cancelled or timed out.
    Cancelled(CancelReason),
}

/// Set by [`begin_shutdown`]; cleared by [`rearm`].
///
/// While set, `Handle::submit` refuses new work. Draining is meaningless if fresh
/// submissions keep arriving behind the drain loop.
static SHUTTING_DOWN: AtomicBool = AtomicBool::new(false);

/// Every handle opened in this process, weakly held.
///
/// Weak so the registry never keeps a handle alive past its own close, and so a
/// leaked registry entry costs one pointer rather than a worker pool.
static LIVE_HANDLES: Mutex<Vec<Weak<Handle>>> = Mutex::new(Vec::new());

/// Reports whether `gusset_shutdown` has been called and not yet re-armed.
pub fn is_shutting_down() -> bool {
    SHUTTING_DOWN.load(Ordering::Acquire)
}

/// Re-arms the runtime after a shutdown, allowing submissions again.
///
/// Called from `gusset_init`, making init/shutdown a reversible pair rather than a
/// one-way door that a second run inside one process could never recover from.
pub fn rearm() {
    let _serial = lock_recover(&INIT_SHUTDOWN);
    SHUTTING_DOWN.store(false, Ordering::Release);
}

/// Serialises `rearm` against an in-progress `shutdown` drain.
static INIT_SHUTDOWN: Mutex<()> = Mutex::new(());

/// Marks the runtime as shutting down and cancels every job on every live handle.
///
/// Returns the number of handles signalled.
pub fn begin_shutdown() -> usize {
    SHUTTING_DOWN.store(true, Ordering::Release);

    let mut registry = lock_recover(&LIVE_HANDLES);
    registry.retain(|w| w.strong_count() > 0);

    let live: Vec<Arc<Handle>> = registry.iter().filter_map(Weak::upgrade).collect();
    drop(registry);

    for h in &live {
        h.cancel_all();
    }
    live.len()
}

/// Total work units still queued or running across every live handle.
pub fn total_in_flight() -> usize {
    // Upgrade under the registry lock, count after releasing it. Dropping an
    // upgraded Arc inside the iterator could be the last reference: Drop then
    // ran `close`, joining every worker with LIVE_HANDLES held, which blocked
    // every Handle::open and shutdown in the process for the whole join.
    let live: Vec<Arc<Handle>> = {
        let mut registry = lock_recover(&LIVE_HANDLES);
        registry.retain(|w| w.strong_count() > 0);
        registry.iter().filter_map(Weak::upgrade).collect()
    };
    live.iter().map(|h| h.in_flight()).sum()
}

/// Drains the runtime: refuses new work, cancels in-flight jobs, and waits up to
/// `drain` for them to finish.
///
/// Returns `Ok(remaining)` where `remaining` is the number of work units still in
/// flight when the budget expired — zero means a clean drain. Cancellation is
/// cooperative, so a work unit that never calls `JobContext::check` cannot be
/// drained; reporting the count is how the caller learns that rather than being
/// told the drain succeeded.
pub fn shutdown(drain: std::time::Duration) -> usize {
    // Held for the whole drain so a concurrent `rearm` (gusset_init) waits for
    // it rather than re-enabling submissions halfway through, which let new,
    // uncancelled work count against the drain.
    let _serial = lock_recover(&INIT_SHUTDOWN);
    begin_shutdown();

    let deadline = std::time::Instant::now() + drain;
    let mut backoff = std::time::Duration::from_micros(200);

    loop {
        let remaining = total_in_flight();
        if remaining == 0 {
            return 0;
        }
        if std::time::Instant::now() >= deadline {
            return remaining;
        }
        thread::sleep(backoff);
        backoff = (backoff * 2).min(std::time::Duration::from_millis(5));
    }
}

/// Locks one of this module's mutexes, recovering from poisoning.
///
/// Every mutex here guards a plain collection, so a panic while one is held leaves
/// the collection structurally valid. *Skipping* the guarded operation does not:
/// a dropped result leaves the Go caller waiting on that ticket forever, a skipped
/// `sender.take()` leaves `close` blocked in `join`, and a lost cancel flag disables
/// cancellation without telling anyone. Recovering is strictly safer than treating
/// "could not lock" the same as "done".
fn lock_recover<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

/// Engine handler function type.
pub type EngineFn =
    Arc<dyn Fn(&JobContext, &[u8]) -> Result<JobOutput, String> + Send + Sync + 'static>;

static GLOBAL_ENGINE: RwLock<Option<EngineFn>> = RwLock::new(None);
static ENGINE_REGISTRY: RwLock<Option<IdMap<u32, EngineFn>>> = RwLock::new(None);

/// Sets the global engine execution handler.
///
/// Lock poisoning is recovered from rather than swallowed: a previous panic while
/// the registry was held must not leave the process permanently unable to register
/// an engine, and it must never be reported to the caller as a successful install.
pub fn set_engine_handler<F, R>(f: F)
where
    F: Fn(&JobContext, &[u8]) -> Result<R, String> + Send + Sync + 'static,
    R: Into<JobOutput> + 'static,
{
    let mut w = GLOBAL_ENGINE.write().unwrap_or_else(|e| e.into_inner());
    *w = Some(Arc::new(move |ctx, input| f(ctx, input).map(Into::into)));
}

/// Registers an engine execution handler for a specific opcode (R9).
///
/// Dispatches calls matching `ctx.opcode() == opcode` directly to this handler.
pub fn register_engine<F, R>(opcode: u32, f: F)
where
    F: Fn(&JobContext, &[u8]) -> Result<R, String> + Send + Sync + 'static,
    R: Into<JobOutput> + 'static,
{
    let mut w = ENGINE_REGISTRY.write().unwrap_or_else(|e| e.into_inner());
    let map = w.get_or_insert_with(IdMap::default);
    map.insert(
        opcode,
        Arc::new(move |ctx, input| f(ctx, input).map(Into::into)),
    );
}

/// Clears all registered engine handlers (global and opcode-specific). Diagnostic/test use.
pub fn clear_engine_handlers() {
    let mut g = GLOBAL_ENGINE.write().unwrap_or_else(|e| e.into_inner());
    *g = None;
    let mut reg = ENGINE_REGISTRY.write().unwrap_or_else(|e| e.into_inner());
    *reg = None;
}

/// Reports whether an adopter engine handler is currently registered.
pub fn has_engine_handler() -> bool {
    let global_has = GLOBAL_ENGINE
        .read()
        .unwrap_or_else(|e| e.into_inner())
        .is_some();
    if global_has {
        return true;
    }
    let reg = ENGINE_REGISTRY.read().unwrap_or_else(|e| e.into_inner());
    match *reg {
        Some(ref m) => !m.is_empty(),
        None => false,
    }
}

/// Dispatches one work unit (R9).
///
/// Resolution order, and why it is this order:
///
/// 1. An opcode-specific registered engine handler matching `ctx.opcode()` wins.
/// 2. A registered adopter global engine wins next. `GUSSET_FLAG_DIAGNOSTIC_ENGINE`
///    cannot displace it, so a stray flag can never silently swap a production
///    engine for the diagnostic one.
/// 3. Otherwise, if the *caller* set `GUSSET_FLAG_DIAGNOSTIC_ENGINE`, run the
///    built-in diagnostic engine. Only Gusset's own tests set that bit.
/// 4. Otherwise fail closed. Running an implicit echo-or-panic engine because
///    registration was forgotten, or lost a startup race, is how a payload's
///    first byte comes to select `panic!` in a production process.
pub fn default_dispatch(ctx: &JobContext, input: &[u8]) -> Result<JobOutput, String> {
    let opcode = ctx.opcode();
    if opcode != 0 {
        let engine = {
            let reg = ENGINE_REGISTRY.read().unwrap_or_else(|e| e.into_inner());
            reg.as_ref().and_then(|map| map.get(&opcode).cloned())
        };
        if let Some(engine) = engine {
            return engine(ctx, input);
        }
        return Err(format!(
            "gusset: no engine handler registered for opcode {} (submission refused)",
            opcode
        ));
    }

    // 2. Opcode 0: Check global registration
    let global_engine = {
        let r = GLOBAL_ENGINE.read().unwrap_or_else(|e| e.into_inner());
        r.clone()
    };
    if let Some(engine) = global_engine {
        return engine(ctx, input);
    }

    // 3. Diagnostic engine fallback (only for opcode 0)
    if ctx.header().flags & GUSSET_FLAG_DIAGNOSTIC_ENGINE != 0 {
        if input.first() == Some(&DIAG_MODE_ALLOCATED) {
            return diagnostic_allocated(ctx, input);
        }
        return diagnostic_dispatch(ctx, input).map(JobOutput::Bytes);
    }

    Err(
        "gusset: no engine handler registered; call gusset::set_engine_handler() before \
         submitting work (submission refused rather than run against a built-in engine)"
            .to_string(),
    )
}

/// Diagnostic mode 16: a generated output built through the buffer allocator.
///
/// `input[1..5]` is the output length (u32 LE), `input[5]` a seed; byte `i` of the
/// output is `(i as u8) ^ seed`. The output is grown a chunk at a time so every
/// resize path of [`crate::alloc::BufferAlloc`] runs. Built with the stable
/// `Allocator` trait it is adopted without a copy; on older toolchains the same
/// bytes come back as a plain vector, so the Go suite asserts identical results
/// on both.
pub const DIAG_MODE_ALLOCATED: u8 = 16;

fn diagnostic_allocated(ctx: &JobContext, input: &[u8]) -> Result<JobOutput, String> {
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
                // packs twice as many lanes per vector as u64.
                let chunk_sum: u32 = chunk.iter().map(|&b| (b as u32) * (b as u32)).sum();
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

/// Runs one dequeued work unit to a result: early cancellation, the engine call
/// behind its panic firewall, and the refusal of aliased output buffers.
///
/// The worker calls this under a second `catch_unwind`, so the unit — and the
/// input payload it owns — is consumed and dropped in here, inside that firewall.
fn execute_unit(weak: &Weak<Handle>, mut unit: WorkUnit) -> JobResult {
    unit.ctx.mark_dequeued();

    // Check cancellation before starting
    if let Err(reason) = unit.ctx.check() {
        return JobResult::Cancelled(reason);
    }

    let slice: &[u8] = match &unit.payload {
        TaskPayload::Small { len, bytes } => &bytes[..*len as usize],
        TaskPayload::Inline(vec) => vec.as_slice(),
        TaskPayload::Shared(buf) => buf.as_slice(),
    };

    // Discard any location a previous, already-handled panic left for this
    // thread. Entries are keyed by thread id only, and a panic propagated with
    // `resume_unwind` (rayon, cross-thread joins) never runs the hook, so the
    // stale entry was reported as this job's panic site.
    let _ = take_panic_location();
    let dispatch_res = catch_unwind(AssertUnwindSafe(|| default_dispatch(&unit.ctx, slice)));
    unit.ctx.mark_finished();

    match dispatch_res {
        Ok(Ok(JobOutput::Bytes(out))) => JobResult::Ok(out),
        // An engine that returns its *input* buffer as its output aliases memory
        // the caller still owns. Go frees a take buffer once it has copied the
        // result out, so the caller's live `*Buffer` would be released underneath
        // it and its `Bytes()` slice left pointing at freed pages — a
        // use-after-free an adopter engine can open by writing the obvious echo.
        // Refusing it turns that into a returned error.
        //
        // Id 0 is refused for the same reason it can never be valid: ids start at
        // 1, so a zero here is an engine returning a default rather than a buffer
        // it allocated.
        Ok(Ok(JobOutput::Buffer(buf_id))) if buf_id == 0 || buf_id == unit.input_buffer_id => {
            JobResult::Err(format!(
                "engine returned buffer id {} as its output: {}; \
                 allocate a new buffer for the result",
                buf_id,
                if buf_id == 0 {
                    "id 0 is never a live buffer"
                } else {
                    "that is the input buffer, which the caller still owns"
                }
            ))
        }
        #[cfg(gusset_allocator_api)]
        Ok(Ok(JobOutput::Allocated(out))) => match weak.upgrade() {
            Some(h) => h.adopt_output(out),
            None => JobResult::Err("handle is closed".to_string()),
        },
        Ok(Ok(JobOutput::Buffer(buf_id))) => match weak.upgrade() {
            Some(h) => {
                let live: &Handle = &h;
                match live.claim_output(buf_id) {
                    Ok(()) => JobResult::Buffer(buf_id),
                    Err(e) => JobResult::Err(e),
                }
            }
            None => JobResult::Err("handle is closed".to_string()),
        },
        Ok(Err(err)) => JobResult::Err(err),
        Err(payload) => {
            // Caught panic: poison handle (I2)
            if let Some(h) = weak.upgrade() {
                h.poisoned.store(true, Ordering::Release);
            }
            // Location first: disposing of the payload can panic again and
            // record the destructor's location over the engine's.
            let loc = take_panic_location();
            let msg = extract_panic_payload(payload);
            JobResult::Panic {
                msg,
                file: loc.map(|l| l.file),
                line: loc.map(|l| l.line).unwrap_or(0),
            }
        }
    }
}

/// Registry entry for one Rust-owned buffer.
struct BufferSlot {
    buf: Arc<RawBuffer>,
    /// Set once the buffer has become a job's output.
    ///
    /// Go frees an output buffer when its waiter is done with it. Two results
    /// naming one buffer therefore free it under each other: the drain loop
    /// takes both completions back to back and both resolve the same pointer.
    /// An engine returning a captured, pre-allocated id does exactly that on
    /// its second call, so a buffer may become an output once per lifetime.
    output_claimed: bool,
    /// Set when the pointer was handed to Go (`NewBuffer` / `gusset_buf_alloc`).
    ///
    /// Go still holds a view. The result path frees an output buffer when the
    /// waiter is done, which would release this memory under that view.
    caller_held: bool,
}

impl BufferSlot {
    fn new(buf: RawBuffer, output_claimed: bool, caller_held: bool) -> Self {
        Self {
            buf: Arc::new(buf),
            output_claimed,
            caller_held,
        }
    }
}

/// Represents an active Gusset runtime handle (I4).
pub struct Handle {
    pool_size: usize,
    /// Completion-pipe write descriptor, or -1 when this handle does not own one.
    ///
    /// Held as an atomic so ownership transfers at a single point: `open` publishes
    /// the descriptor only after the handle is fully constructed, and `close` takes
    /// it back with a swap. A handle that fails to open therefore never closes a
    /// descriptor the caller is still responsible for, and no descriptor is closed
    /// twice — which would otherwise shut an unrelated file that reused the number.
    pipe_write_fd: AtomicI32,
    pipe_write_lock: Mutex<()>,
    poisoned: AtomicBool,
    closed: AtomicBool,
    sender: Mutex<Option<QueueSender<WorkUnit>>>,
    results: Mutex<IdMap<u64, JobResult>>,
    cancel_flags: Mutex<IdMap<u64, Arc<AtomicBool>>>,
    buffers: Mutex<IdMap<u64, BufferSlot>>,
    next_buffer_id: AtomicU64,
    next_worker_id: AtomicU64,
    workers: Mutex<Vec<thread::JoinHandle<()>>>,
    receiver: Arc<JobQueue<WorkUnit>>,
    self_weak: Mutex<Weak<Handle>>,
    /// Workers that have exited since the last reap. Bumped by a drop guard
    /// in each worker (normal exit or unwind), read by `ensure_workers` so the
    /// common case — every worker alive — costs one atomic load per submit
    /// instead of a mutex, a scan of every JoinHandle and two Vec allocations.
    exited: Arc<AtomicUsize>,
    /// Whether the completion pipe holds `pool_size` inline records. If it
    /// could only be sized for bare tickets, inline completions are off and
    /// every caller gets tickets, whatever it asked for.
    inline_ok: bool,
}

/// Default worker count when the caller passes 0.
pub const DEFAULT_POOL_SIZE: usize = 4;

/// R16 copy ceiling: inputs this size and under are memcpy'd during submit.
/// Larger inputs must arrive as a Rust-owned Buffer. Copying a megabyte on the
/// cgo thread is the long call the submit-and-return contract forbids.
pub const MAX_INLINE_INPUT: usize = 4096;

/// Re-export of the per-buffer ceiling enforced at allocation (R16).
pub const MAX_BUFFER_BYTES: usize = sys::MAX_BUFFER_BYTES;

/// Hard ceiling on workers per handle.
///
/// Each worker is a real OS thread with an 8 MiB stack, so an unbounded pool size
/// is an unbounded thread and address-space request driven straight from a caller
/// argument. It also bounds the number of completion tickets that can be in flight
/// behind the completion pipe, which is what keeps `write_ticket` from ever facing
/// a full pipe under the documented bounded-concurrency contract (I4, R11).
pub const MAX_POOL_SIZE: usize = 1024;

/// Tickets and buffer ids occupy the low 63 bits and start at 1.
///
/// Zero means "no buffer" on the submit header. A counter that wraps, or that
/// advances while refusing, later reissues an id that is still live.
const ID_CEILING: u64 = TAKE_OWNED_FLAG;

/// Set on `gusset_take`'s buffer id when that id is the result's own buffer,
/// which the caller frees once consumed (`GUSSET_TAKE_OWNED_FLAG`). Every id is
/// below `ID_CEILING`, which is this bit, so the flag never collides.
pub const TAKE_OWNED_FLAG: u64 = 1 << 63;

/// Ticket counter shared by every handle in the process.
///
/// Per-handle counters all started at 1, so two handles issued the same ticket
/// numbers. Go keys its bookkeeping by ticket per handle, and `Wait` on handle
/// B with a ticket from handle A found B's own ticket of that number: B's
/// result went to A's caller with a nil error, and B's real waiter then got
/// `ErrUnknownTicket`. Unique tickets make a foreign ticket unknown everywhere
/// but its own handle, which is what `ErrUnknownTicket` promises. 2^63 tickets
/// is ~292,000 years at a million submissions a second.
static NEXT_TICKET: AtomicU64 = AtomicU64::new(1);

/// Reserves the next id, or refuses without advancing once the space is exhausted.
fn reserve_id(counter: &AtomicU64) -> Result<u64, String> {
    loop {
        let cur = counter.load(Ordering::Relaxed);
        if cur == 0 || cur >= ID_CEILING {
            return Err(
                "id space exhausted; refusing to wrap onto a live ticket or buffer".to_string(),
            );
        }
        match counter.compare_exchange_weak(cur, cur + 1, Ordering::Relaxed, Ordering::Relaxed) {
            Ok(_) => return Ok(cur),
            Err(_) => continue,
        }
    }
}

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

/// Explicit worker stack size (R8).
///
/// cgo-created threads inherit the pthread default, which is 128 KiB on musl. Heavy
/// work runs here, not on the caller's g0 stack, so the size is set explicitly
/// rather than inherited.
pub const WORKER_STACK_SIZE: usize = 8 * 1024 * 1024;

impl Handle {
    /// Opens a new handle with a dedicated worker pool of pool_size threads.
    ///
    /// Returns `Err` for a pool size above [`MAX_POOL_SIZE`] rather than attempting
    /// the spawn: refusing loudly beats discovering the ceiling as a partial spawn
    /// failure halfway through creating thousands of threads.
    pub fn open(pool_size: u32, pipe_write_fd: i32) -> Result<Arc<Self>, String> {
        install_panic_hook();

        let pool_size = if pool_size == 0 {
            DEFAULT_POOL_SIZE
        } else {
            pool_size as usize
        };

        if pipe_write_fd < 0 {
            return Err("pipe_write_fd must be non-negative".to_string());
        }

        sys::set_nonblocking(pipe_write_fd)
            .map_err(|e| format!("failed to set non-blocking on pipe write fd: {}", e))?;

        if pool_size > MAX_POOL_SIZE {
            return Err(format!(
                "pool_size {} exceeds maximum {} (each worker is an OS thread with an 8 MiB stack)",
                pool_size, MAX_POOL_SIZE
            ));
        }
        // I4 keeps unread tickets at or under pool_size, which is only a
        // "the pipe never fills" guarantee if the pipe holds that many. Linux
        // shrinks new pipes to one page (512 tickets) once a user passes
        // pipe-user-pages-soft, so a 1024-worker pool could stall its
        // completions for the 10 s write timeout and then drop them. Grow the
        // pipe to fit, or refuse the pool size instead of discovering it later.
        //
        // Inline completion records (up to INLINE_RECORD_MAX bytes each) need
        // pool_size of those. When the pipe cannot grow that far, fall back to
        // 8-byte tickets for every result rather than refuse the pool.
        let inline_ok = if pipe_write_fd < 0 {
            true
        } else if cfg!(target_os = "linux") {
            if sys::ensure_pipe_capacity(pipe_write_fd, pool_size.saturating_mul(INLINE_RECORD_MAX))
                .is_ok()
            {
                true
            } else {
                sys::ensure_pipe_capacity(pipe_write_fd, pool_size.saturating_mul(8))?;
                false
            }
        } else {
            // Elsewhere the capacity cannot be queried. Every BSD-derived pipe
            // holds at least 16 KiB, so allow records only while they fit there.
            pool_size.saturating_mul(INLINE_RECORD_MAX) <= 16 * 1024
        };

        let (sender, receiver) = JobQueue::new(pool_size * 2);

        let handle = Arc::new(Self {
            pool_size,
            // Not owned yet: published below, only once the pool is fully up.
            pipe_write_fd: AtomicI32::new(-1),
            pipe_write_lock: Mutex::new(()),
            poisoned: AtomicBool::new(false),
            closed: AtomicBool::new(false),
            sender: Mutex::new(Some(sender)),
            results: Mutex::new(IdMap::default()),
            cancel_flags: Mutex::new(IdMap::default()),
            buffers: Mutex::new(IdMap::default()),
            next_buffer_id: AtomicU64::new(1),
            next_worker_id: AtomicU64::new(0),
            workers: Mutex::new(Vec::with_capacity(pool_size)),
            receiver,
            self_weak: Mutex::new(Weak::new()),
            exited: Arc::new(AtomicUsize::new(0)),
            inline_ok,
        });

        // Store weak self reference for worker threads. If this were skipped the
        // workers could never upgrade, so every result would be computed and then
        // silently discarded.
        *lock_recover(&handle.self_weak) = Arc::downgrade(&handle);

        // Spawn pool worker threads. Workers park in recv() until the first submit,
        // which cannot happen before this function returns, so publishing the
        // descriptor after the spawn cannot race a completion write.
        let pool: &Handle = &handle;
        pool.spawn_workers(pool_size)?;

        // Take ownership of the completion pipe only now that opening has
        // succeeded. On any error path above, the descriptor stays the caller's and
        // Drop closes nothing.
        handle.pipe_write_fd.store(pipe_write_fd, Ordering::Release);

        // Register for gusset_shutdown. Registered last so a handle that failed to
        // open never appears, and pruned here so the list cannot grow without bound
        // across many open/close cycles.
        {
            let mut registry = lock_recover(&LIVE_HANDLES);
            registry.retain(|w| w.strong_count() > 0);
            registry.push(Arc::downgrade(&handle));
        }

        Ok(handle)
    }

    /// Work units queued or running on this handle.
    ///
    /// A cancel flag is registered at submit and removed by the worker once the
    /// result is stored, so the flag count is exactly the in-flight count.
    pub fn in_flight(&self) -> usize {
        lock_recover(&self.cancel_flags).len()
    }

    fn spawn_workers(&self, count: usize) -> Result<(), String> {
        let mut workers = lock_recover(&self.workers);
        self.spawn_workers_locked(&mut workers, count)
    }

    fn spawn_workers_locked(
        &self,
        workers: &mut Vec<thread::JoinHandle<()>>,
        count: usize,
    ) -> Result<(), String> {
        let weak_handle = lock_recover(&self.self_weak).clone();

        for _ in 0..count {
            // Deterministic trigger for the failure path below. The fd-ownership
            // fix has no natural reproducer — a real spawn failure needs the OS to
            // be out of threads — so without injection the regression test could
            // only assert the success path and would pass against the pre-fix code.
            #[cfg(test)]
            if tests::spawn_should_fail() {
                return Err("gusset: injected spawn failure (test)".to_string());
            }

            let weak_clone = weak_handle.clone();
            let exited = Arc::clone(&self.exited);
            let receiver_clone = Arc::clone(&self.receiver);
            // Monotonic, so a respawned worker never reuses a retired worker's name
            // in a thread dump (workers.len() shrinks when dead entries are reaped).
            let worker_id = self.next_worker_id.fetch_add(1, Ordering::Relaxed);

            let builder = thread::Builder::new()
                .name(format!("gusset-w{}", worker_id))
                .stack_size(WORKER_STACK_SIZE);

            let join_handle = builder
                .spawn(move || {
                    // Counted on any exit, unwinding included, so a dead worker
                    // is always noticed by the next submit.
                    struct ExitMark(Arc<AtomicUsize>);
                    impl Drop for ExitMark {
                        fn drop(&mut self) {
                            self.0.fetch_add(1, Ordering::Release);
                        }
                    }
                    let _exit_mark = ExitMark(exited);
                    // A write to a pipe whose read end is gone raises SIGPIPE on
                    // the writing thread. On a thread Go did not create, Go's
                    // handler re-raises it with the default action and the whole
                    // process exits (status 141) — from a completion write, or
                    // from the panic hook printing to a closed stderr. Blocked
                    // here, the write returns EPIPE and is handled as an error.
                    if !sys::block_sigpipe_on_this_thread() {
                        crate::ffi::log_event(&format!(
                            "gusset: worker gusset-w{} could not block SIGPIPE; a write to a \
                             closed pipe from this thread will terminate the process",
                            worker_id
                        ));
                    }

                    // I5/R8: a staticlib never runs std::rt::init, so nothing has
                    // installed an alternate signal stack for this thread. Go
                    // requires one on threads it did not create: without it, a
                    // signal arriving while this thread's stack is nearly used up
                    // has nowhere to run Go's handler. (An overflow is still fatal
                    // either way; see sys::sigaltstack_size.) A failure here is a
                    // real loss of protection and is reported, not shrugged off.
                    let _sig_guard = match sys::install_sigaltstack() {
                        Some(g) => Some(g),
                        None => {
                            crate::ffi::log_event(&format!(
                                "gusset: worker gusset-w{} started WITHOUT sigaltstack; \
                                 a Rust stack overflow on this thread will abort the process (I5/R8)",
                                worker_id
                            ));
                            None
                        }
                    };

                    // I5/R8: `Builder::stack_size` is a request. A platform that
                    // rounded it down, or a future refactor that dropped the call,
                    // would leave heavy work running on whatever the pthread default
                    // happens to be — 128 KiB on musl — and nothing would say so
                    // until a deep engine blew the stack in production.
                    match sys::current_thread_stack_size() {
                        Some(got) if got < WORKER_STACK_SIZE => {
                            crate::ffi::log_event(&format!(
                                "gusset: worker gusset-w{} has a {} byte stack, below the \
                                 requested {} bytes; deep engine recursion may overflow (I5/R8)",
                                worker_id, got, WORKER_STACK_SIZE
                            ));
                        }
                        Some(_) => {}
                        None => {
                            crate::ffi::log_event(&format!(
                                "gusset: worker gusset-w{} could not read back its stack size; \
                                 the 8 MiB guarantee is unverified on this platform (I5/R8)",
                                worker_id
                            ));
                        }
                    }

                    loop {
                        // Spin briefly, then park (see pool::queue). None once
                        // the handle closed and the queue is drained.
                        let unit = match receiver_clone.pop() {
                            Some(u) => u,
                            None => break,
                        };
                        let ticket = unit.ticket;
                        let wants_inline = unit.ctx.header().flags
                            & crate::header::GUSSET_FLAG_INLINE_COMPLETION
                            != 0;

                        // The engine call has its own firewall inside execute_unit;
                        // this one covers everything around it, including dropping
                        // the unit. Whatever unwinds here, the ticket still gets a
                        // completion: a worker that died between dequeue and write
                        // left its Go waiter parked forever on a permit that never
                        // came back (I2, I4).
                        let result = match catch_unwind(AssertUnwindSafe(|| {
                            execute_unit(&weak_clone, unit)
                        })) {
                            Ok(r) => r,
                            Err(payload) => {
                                if let Some(h) = weak_clone.upgrade() {
                                    h.poisoned.store(true, Ordering::Release);
                                }
                                let loc = take_panic_location();
                                let msg = extract_panic_payload(payload);
                                JobResult::Panic {
                                    msg: format!("worker fault outside the engine call: {}", msg),
                                    file: loc.map(|l| l.file),
                                    line: loc.map(|l| l.line).unwrap_or(0),
                                }
                            }
                        };

                        // Store result and wake netpoller if handle still alive
                        if let Some(h) = weak_clone.upgrade() {
                            let result = materialize_result(&h, result);
                            let mut record = [0u8; INLINE_RECORD_MAX];
                            match result {
                                // Small success: the bytes ride in the record;
                                // nothing is stored for a gusset_take.
                                JobResult::Ok(ref data)
                                    if wants_inline
                                        && h.inline_ok
                                        && data.len() <= INLINE_RESULT_MAX =>
                                {
                                    let n = inline_record(&mut record, ticket, data);
                                    drop(result);
                                    h.publish_completion(ticket, &record[..n]);
                                }
                                other => {
                                    lock_recover(&h.results).insert(ticket, other);
                                    h.publish_completion(ticket, &ticket.to_ne_bytes());
                                }
                            }
                            // Only now is the unit out of flight: shutdown's drain
                            // counts cancel flags, and removing this one before the
                            // write let `gusset_shutdown` report a clean drain while
                            // a worker still sat in the up-to-10 s write backoff.
                            lock_recover(&h.cancel_flags).remove(&ticket);
                        }
                    }
                })
                .map_err(|e| format!("failed to spawn worker thread: {}", e))?;

            workers.push(join_handle);
        }

        Ok(())
    }

    /// Verifies worker health and respawns replacement workers if any died (I5).
    fn ensure_workers(&self) -> Result<(), String> {
        // Fast path: no worker has exited, so there is nothing to reap.
        if self.exited.load(Ordering::Acquire) == 0 {
            return Ok(());
        }
        let mut workers = lock_recover(&self.workers);
        // Re-check under the lock: close drains this vec and then joins. A spawn
        // that raced past a pre-lock closed check would leave JoinHandles nobody
        // joins, detaching OS threads on a handle that is already shut down.
        if self.closed.load(Ordering::Acquire) {
            return Ok(());
        }
        // Join finished workers rather than dropping their JoinHandles. A
        // worker that died with a panic payload whose destructor panics would
        // otherwise hit std's "thread result panicked on drop" abort here, on
        // the submitting (Go) thread; joining routes the payload through the
        // same containment close uses.
        let (done, live): (Vec<_>, Vec<_>) = workers.drain(..).partition(|h| h.is_finished());
        *workers = live;
        // Only the reaped ones: an exit racing this reap stays counted and is
        // picked up by the next submit.
        self.exited.fetch_sub(done.len(), Ordering::AcqRel);
        for h in done {
            if let Err(payload) = h.join() {
                drop_panic_payload(payload);
            }
        }

        if workers.len() < self.pool_size {
            let needed = self.pool_size - workers.len();
            self.spawn_workers_locked(&mut workers, needed)?;
        }

        Ok(())
    }

    /// Submits a task to the pool (R16 zero-copy for buffers).
    pub fn submit(&self, header: CallHeader, input: &[u8], buffer_id: u64) -> Result<u64, String> {
        if self.poisoned.load(Ordering::Acquire) {
            return Err("handle is poisoned".to_string());
        }
        if self.closed.load(Ordering::Acquire) {
            return Err("handle is closed".to_string());
        }
        if is_shutting_down() {
            return Err("gusset runtime is shutting down".to_string());
        }

        // Reject unknown flag bits instead of ignoring them, so a caller built
        // against a newer header cannot believe a flag took effect here.
        let unknown = header.flags & !GUSSET_FLAGS_KNOWN;
        if unknown != 0 {
            return Err(format!(
                "unknown CallHeader flag bits set: {:#010x} (known mask {:#010x})",
                unknown, GUSSET_FLAGS_KNOWN
            ));
        }

        // Auto-respawn replacement workers if any died (I5)
        self.ensure_workers()?;

        // R16: inputs up to 4 KiB copied during submit; larger inputs live in Buffer.
        // Shared buffers are passed as Arc<RawBuffer> without extra byte copy.
        let payload = if buffer_id > 0 {
            let buffers = lock_recover(&self.buffers);
            let rec = buffers
                .get(&buffer_id)
                .ok_or_else(|| format!("buffer id {} not found", buffer_id))?;
            TaskPayload::Shared(Arc::clone(&rec.buf))
        } else if input.len() > MAX_INLINE_INPUT {
            return Err(format!(
                "inline input {} bytes exceeds {}-byte copy limit; use a Buffer",
                input.len(),
                MAX_INLINE_INPUT
            ));
        } else {
            if input.len() <= SMALL_INPUT {
                let mut bytes = [0u8; SMALL_INPUT];
                bytes[..input.len()].copy_from_slice(input);
                TaskPayload::Small {
                    len: input.len() as u8,
                    bytes,
                }
            } else {
                TaskPayload::Inline(input.to_vec())
            }
        };

        let ticket = reserve_id(&NEXT_TICKET)?;
        let cancel_flag = Arc::new(AtomicBool::new(false));

        let ctx = JobContext::new(header, Arc::clone(&cancel_flag));
        let unit = WorkUnit {
            ticket,
            ctx,
            payload,
            input_buffer_id: buffer_id,
        };

        // try_send under the sender lock. `send` blocks when the queue is full,
        // and holding the lock across that block stops `close` from disconnecting
        // the workers. Cloning the sender and sending after the lock drops keeps
        // the channel alive across `close`, so `join` waits on a recv that will
        // not see a disconnect. A full queue is a contract break (the Go
        // semaphore is the bound); refuse it instead of blocking.
        {
            let sender_guard = lock_recover(&self.sender);
            let sender = match sender_guard.as_ref() {
                Some(s) => s,
                None => return Err("handle is closed".to_string()),
            };
            {
                let mut flags = lock_recover(&self.cancel_flags);
                flags.insert(ticket, Arc::clone(&cancel_flag));
                // begin_shutdown sets SHUTTING_DOWN and then cancels every
                // flag under this lock. A submit that passed the check above
                // before the store, but inserts after that cancel_all, would
                // run uncancelled and eat the whole drain budget. Under the
                // lock, the store is visible here if cancel_all already ran.
                if is_shutting_down() {
                    cancel_flag.store(true, Ordering::Release);
                }
            }
            if let Err(err) = sender.try_send(unit) {
                lock_recover(&self.cancel_flags).remove(&ticket);
                return Err(match err {
                    PushError::Full(_) => "submission queue is full".to_string(),
                    PushError::Closed(_) => "handle is closed".to_string(),
                });
            }
        }

        Ok(ticket)
    }

    /// Moves out the result of a completed job (moves out exactly once).
    pub fn take(&self, ticket: u64) -> Result<JobResult, String> {
        let mut results = lock_recover(&self.results);
        results
            .remove(&ticket)
            .ok_or_else(|| format!("ticket {} not found or already taken", ticket))
    }

    /// Cancels a specific job by ticket (I3).
    ///
    /// Returns whether a live flag was found. A caller that cancels an unknown or
    /// already-completed ticket learns so instead of being told nothing.
    pub fn cancel(&self, ticket: u64) -> bool {
        let flags = lock_recover(&self.cancel_flags);
        match flags.get(&ticket) {
            Some(flag) => {
                flag.store(true, Ordering::Release);
                true
            }
            None => false,
        }
    }

    /// Cancels all currently pending jobs (I3).
    ///
    /// Returns how many flags were set. `close` relies on this actually running:
    /// a silently skipped cancel leaves cooperative jobs running while `close`
    /// waits for them in `join`.
    pub fn cancel_all(&self) -> usize {
        let flags = lock_recover(&self.cancel_flags);
        for flag in flags.values() {
            flag.store(true, Ordering::Release);
        }
        flags.len()
    }

    /// Allocates 64-byte aligned Rust-owned buffer memory (R16).
    ///
    /// The pointer is not published to Go. An engine may return this id as its
    /// output once. Buffers Go can already see are [`Handle::buf_alloc_published`].
    pub fn buf_alloc(&self, len: usize) -> Result<(u64, *mut u8), String> {
        self.allocate_buffer(len, false)
    }

    /// Allocates a buffer and records that Go holds the pointer (`NewBuffer`).
    ///
    /// `gusset_buf_alloc` is this path. An engine that returns the id as
    /// `JobOutput::Buffer` is handing back memory the caller still views; the
    /// result path would free it when the waiter finishes.
    pub fn buf_alloc_published(&self, len: usize) -> Result<(u64, *mut u8), String> {
        self.allocate_buffer(len, true)
    }

    fn allocate_buffer(&self, len: usize, caller_held: bool) -> Result<(u64, *mut u8), String> {
        if self.poisoned.load(Ordering::Acquire) {
            return Err("handle is poisoned".to_string());
        }
        if self.closed.load(Ordering::Acquire) {
            return Err("handle is closed".to_string());
        }
        let id = reserve_id(&self.next_buffer_id)?;
        let buf = RawBuffer::allocate(len)?;
        let ptr = buf.as_mut_ptr();

        let mut map = lock_recover(&self.buffers);
        map.insert(id, BufferSlot::new(buf, false, caller_held));

        Ok((id, ptr))
    }

    /// Copies `data` into a new buffer and returns its id and pointer.
    ///
    /// The memcpy runs on the calling thread. Workers use this to promote a
    /// large `JobResult::Ok` off the cgo take path, and `gusset_take` uses it for
    /// the small results it still copies (R16 egress).
    ///
    /// Deliberately not poison-checked, unlike [`Handle::buf_alloc`]. Poison
    /// refuses new work (I2); this carries out a result that already exists. A
    /// check here lost a sibling's finished result the moment another job
    /// panicked, and only for results small enough not to have been promoted.
    pub(crate) fn buf_from_bytes(&self, data: &[u8]) -> Result<(u64, *mut u8), String> {
        if self.closed.load(Ordering::Acquire) {
            return Err("handle is closed".to_string());
        }
        let id = reserve_id(&self.next_buffer_id)?;
        let buf = RawBuffer::from_bytes(data)?;
        let ptr = buf.as_mut_ptr();
        // Born as an output: nothing else may return it as one.
        lock_recover(&self.buffers).insert(id, BufferSlot::new(buf, true, false));
        Ok((id, ptr))
    }

    /// Registers an engine's `BufferAlloc` output as a result buffer, zero-copy.
    ///
    /// Runs on the worker after the engine's own `catch_unwind`, under the
    /// worker's outer firewall. An empty output is an empty
    /// result; one over the 1 GiB ceiling is refused (and freed) exactly as a
    /// `Vec<u8>` of that size is; memory that cannot be adopted as-is is copied.
    #[cfg(gusset_allocator_api)]
    fn adopt_output(&self, out: Vec<u8, crate::alloc::BufferAlloc>) -> JobResult {
        if out.is_empty() {
            return JobResult::Ok(Vec::new());
        }
        if out.len() > MAX_BUFFER_BYTES {
            return JobResult::Err(format!(
                "output {} bytes exceeds maximum {} bytes",
                out.len(),
                MAX_BUFFER_BYTES
            ));
        }
        if self.closed.load(Ordering::Acquire) {
            return JobResult::Err("handle is closed".to_string());
        }
        let id = match reserve_id(&self.next_buffer_id) {
            Ok(id) => id,
            Err(e) => return JobResult::Err(e),
        };
        let buf = match RawBuffer::adopt(out) {
            Ok(buf) => buf,
            Err(foreign) => match RawBuffer::from_bytes(&foreign) {
                Ok(buf) => buf,
                Err(e) => return JobResult::Err(e),
            },
        };
        // Born as an output, like buf_from_bytes: nothing may return it again.
        lock_recover(&self.buffers).insert(id, BufferSlot::new(buf, true, false));
        JobResult::Buffer(id)
    }

    /// Transfers a live buffer to a job's result, refusing any second owner.
    ///
    /// Refused when the buffer is already some result's output, when Go still
    /// holds the pointer (`caller_held`), or when another work unit still holds
    /// it as its input. In each case the waiter freeing this result would
    /// release memory someone else is reading. Checked under the registry lock,
    /// which is also where `submit` clones an input's `Arc`, so the strong
    /// count cannot grow between the check and the claim.
    fn claim_output(&self, id: u64) -> Result<(), String> {
        let mut map = lock_recover(&self.buffers);
        let slot = map.get_mut(&id).ok_or_else(|| {
            format!(
                "engine returned buffer id {} as its output: no such live buffer",
                id
            )
        })?;
        if slot.output_claimed {
            return Err(format!(
                "engine returned buffer id {} as its output: it is already another call's \
                 output; allocate a new buffer for each result",
                id
            ));
        }
        // A published buffer still has a Go view. Moving it to a result would
        // let the waiter free it under that view — the same use-after-free as
        // returning the input, for every buffer `NewBuffer` handed out rather
        // than only the one this unit was given. Vale refuses the move while
        // another owner is live; this is that check, stored at alloc time
        // because Rust cannot see the Go pointer.
        if slot.caller_held {
            return Err(format!(
                "engine returned buffer id {} as its output: the caller still holds it; \
                 allocate a new buffer for the result",
                id
            ));
        }
        if Arc::strong_count(&slot.buf) > 1 {
            return Err(format!(
                "engine returned buffer id {} as its output: another work unit is still \
                 reading it as its input",
                id
            ));
        }
        slot.output_claimed = true;
        Ok(())
    }

    /// Looks up a live buffer by id, returning its mutable pointer and byte length.
    pub fn buf_get(&self, id: u64) -> Result<(*mut u8, usize), String> {
        let map = lock_recover(&self.buffers);
        if let Some(slot) = map.get(&id) {
            Ok((slot.buf.as_mut_ptr(), slot.buf.len()))
        } else {
            Err(format!("buffer id {} not found", id))
        }
    }

    /// Frees a Rust-owned buffer by id (R4, R16).
    ///
    /// Missing ids are success: `Free` and the `AddCleanup` backstop can race, and
    /// treating a second free as an error turns a safety net into a user-visible
    /// failure on the path that already released the memory.
    pub fn buf_free(&self, id: u64) -> Result<(), String> {
        let mut map = lock_recover(&self.buffers);
        map.remove(&id);
        Ok(())
    }

    /// Closes the handle, disconnects workers, joins worker threads, and closes write fd.
    pub fn close(&self) {
        if !self.closed.swap(true, Ordering::SeqCst) {
            // 1. Disconnect sender so workers unblock from recv(). Skipping this
            // would leave every worker parked forever and hang step 3.
            lock_recover(&self.sender).take();

            // 2. Signal cooperative cancellation to all active jobs
            self.cancel_all();

            // 3. Join all worker threads to guarantee zero leaked threads.
            // The workers mutex stays held across join so ensure_workers cannot
            // spawn replacements onto a handle that is already shutting down.
            let mut workers = lock_recover(&self.workers);
            let handles: Vec<_> = workers.drain(..).collect();
            let me = thread::current().id();
            for handle in handles {
                // A worker can run this close itself: it upgrades its Weak to
                // publish a completion, and if every other Arc was dropped
                // meanwhile, its upgrade is the last one and Drop runs here, on
                // that worker. Joining our own thread fails with EDEADLK and
                // std panics outside any firewall, detaching the rest of the
                // pool and leaking the pipe. The sender is already gone, so this
                // worker exits on its next recv; the others are still joined.
                if handle.thread().id() == me {
                    continue;
                }
                // A worker that died hands back its panic payload here, and
                // `close` runs under ffi_guard inside an extern "C" export:
                // a payload whose destructor panics must not unwind from it.
                if let Err(payload) = handle.join() {
                    drop_panic_payload(payload);
                }
            }
            drop(workers);

            // 4. Release the completion pipe now that every worker has finished.
            // The swap makes this a once-only transfer: a second close, or a Drop
            // following an explicit close, finds -1 and closes nothing.
            // Under pipe_write_lock, so no writer holds a loaded copy of the
            // number while it is closed and possibly reused.
            let _w = lock_recover(&self.pipe_write_lock);
            let fd = self.pipe_write_fd.swap(-1, Ordering::AcqRel);
            sys::close_fd(fd);
        }
    }

    /// Writes a completion ticket (see [`write_completion`] for the retry rule).
    fn publish_completion(&self, ticket: u64, record: &[u8]) {
        if let Err(e) = write_completion(
            &self.pipe_write_lock,
            &self.pipe_write_fd,
            &self.closed,
            ticket,
            record,
        ) {
            if e.kind() == std::io::ErrorKind::NotConnected {
                return; // closing: the waiter is told "closed" by Go
            }
            crate::ffi::log_event(&format!(
                "gusset: completion write failed for ticket {}: {}",
                ticket, e
            ));
            // EPIPE/EBADF: the reader is gone for good. Refuse new work
            // instead of accepting jobs whose completions cannot be delivered.
            self.poisoned.store(true, Ordering::Release);
        }
    }

    /// Checks whether the handle is currently poisoned.
    pub fn is_poisoned(&self) -> bool {
        self.poisoned.load(Ordering::Acquire)
    }

    /// Returns pool size.
    pub fn pool_size(&self) -> usize {
        self.pool_size
    }
}

/// Moves a large `JobResult::Ok` onto a Buffer so `gusset_take` does not
/// memcpy on the cgo thread.
///
/// The worker holds an upgraded `Arc`, not `&self`, so this is a function on
/// `&Handle` rather than a method.
fn materialize_result(handle: &Handle, result: JobResult) -> JobResult {
    match result {
        JobResult::Ok(data) if data.len() > MAX_BUFFER_BYTES => JobResult::Err(format!(
            "output {} bytes exceeds maximum {} bytes",
            data.len(),
            MAX_BUFFER_BYTES
        )),
        JobResult::Ok(data) if data.len() > MAX_INLINE_INPUT => {
            match handle.buf_from_bytes(&data) {
                Ok((id, _)) => JobResult::Buffer(id),
                Err(e) => JobResult::Err(e),
            }
        }
        other => other,
    }
}

impl Drop for Handle {
    fn drop(&mut self) {
        self.close();
    }
}

#[cfg(test)]
// These tests drive pipe(2)/read(2)/write(2) directly, because the descriptor
// ownership they check is only observable at the syscall level. The allow is
// scoped to the test module: `pool` itself stays under the crate-wide
// `deny(unsafe_code)`, with `pool::sys` the only module permitted unsafe.
#[allow(unsafe_code)]
mod tests {
    use super::*;

    /// Number of further worker spawns to allow before failing, or -1 to disable.
    ///
    /// Compiled only under `cfg(test)`, so the injection point in
    /// `spawn_workers_locked` does not exist in a shipped `libgusset.a`.
    static SPAWN_FAIL_COUNTDOWN: std::sync::atomic::AtomicI64 =
        std::sync::atomic::AtomicI64::new(-1);

    /// The thread that armed the injector. Only its spawns are failed.
    ///
    /// The countdown is process-global and `cargo test` runs unit tests on
    /// parallel threads. `INJECT_LOCK` serialised the tests that *arm* it, but a
    /// test that merely opens a handle took no lock, so its `open` could consume
    /// the armed failure and fail with "injected spawn failure" (roughly one run
    /// in three under `--release`). Scoping the injection to the arming thread
    /// removes the cross-talk rather than asking every future test to lock.
    static ARMED_BY: Mutex<Option<thread::ThreadId>> = Mutex::new(None);

    /// Serialises the tests that arm the injector.
    static INJECT_LOCK: Mutex<()> = Mutex::new(());

    fn arm_spawn_failure(after: i64) {
        *lock_recover(&ARMED_BY) = Some(thread::current().id());
        SPAWN_FAIL_COUNTDOWN.store(after, Ordering::Release);
    }

    fn disarm_spawn_failure() {
        SPAWN_FAIL_COUNTDOWN.store(-1, Ordering::Release);
        *lock_recover(&ARMED_BY) = None;
    }

    pub(super) fn spawn_should_fail() -> bool {
        if *lock_recover(&ARMED_BY) != Some(thread::current().id()) {
            return false;
        }
        let remaining = SPAWN_FAIL_COUNTDOWN.load(Ordering::Acquire);
        if remaining < 0 {
            return false;
        }
        if remaining == 0 {
            return true;
        }
        SPAWN_FAIL_COUNTDOWN.store(remaining - 1, Ordering::Release);
        false
    }

    /// Opens a real pipe, returning `(read_fd, write_fd)`.
    fn make_pipe() -> (i32, i32) {
        let mut fds = [0i32; 2];
        // SAFETY: `fds` is a valid two-element array for pipe(2) to fill.
        let rc = unsafe { libc::pipe(fds.as_mut_ptr()) };
        assert_eq!(rc, 0, "pipe() failed");
        (fds[0], fds[1])
    }

    /// True when `fd` is open *and still refers to the same pipe as `read_fd`*.
    ///
    /// `fcntl(F_GETFD)` alone is not enough: a descriptor number that Gusset closed
    /// can be handed straight back out by a later `open` in another thread, and the
    /// check would then pass against exactly the bug it exists to catch. Writing a
    /// sentinel and reading it off the far end tests identity, not just liveness.
    fn still_the_same_pipe(write_fd: i32, read_fd: i32) -> bool {
        let out: [u8; 1] = [0xA5];
        // SAFETY: writing one byte from a valid stack buffer to a candidate fd.
        let n = unsafe { libc::write(write_fd, out.as_ptr() as *const libc::c_void, 1) };
        if n != 1 {
            return false;
        }
        let mut back = [0u8; 1];
        // SAFETY: reading one byte into a valid stack buffer.
        let n = unsafe { libc::read(read_fd, back.as_mut_ptr() as *mut libc::c_void, 1) };
        n == 1 && back[0] == 0xA5
    }

    /// An `open` that fails after the `Arc` exists must not close the caller's fd.
    ///
    /// `Handle` used to take the write descriptor in its constructor, so the `Arc`
    /// drop on a late failure path ran `close()` on a descriptor ownership had never
    /// transferred for. The caller then closed it again, and in a process that had
    /// meanwhile opened a file, the second close landed on an unrelated descriptor.
    ///
    /// Miri cannot run pipe(2) or the worker threads, so this is skipped there; the
    /// fd-ownership logic it covers is not the kind Miri checks for.
    #[test]
    #[cfg_attr(miri, ignore)]
    fn failed_open_leaves_the_completion_fd_with_the_caller() {
        let _serialise = lock_recover(&INJECT_LOCK);
        let (r, w) = make_pipe();

        // Allow two workers, then fail: the failure has to land *after* the Arc and
        // its Drop exist, which is the only window in which the old code could
        // close a descriptor it did not own.
        arm_spawn_failure(2);
        let result = Handle::open(4, w);
        disarm_spawn_failure();

        match result {
            Ok(_) => panic!("injected spawn failure did not fail the open"),
            Err(e) => assert!(
                e.contains("injected spawn failure"),
                "open failed for an unexpected reason: {}",
                e
            ),
        }

        assert!(
            still_the_same_pipe(w, r),
            "open() failed but closed the caller's completion descriptor: ownership \
             must transfer only once the pool is fully up"
        );

        // SAFETY: both descriptors are still owned by this test.
        unsafe {
            libc::close(w);
            libc::close(r);
        }
    }

    /// A worker's stack is 8 MiB because Gusset asked for it (I5, R8).
    ///
    /// Read back from the thread that actually ran the job, through the real pool.
    /// A deep-recursion probe cannot distinguish this from the platform default on
    /// glibc or darwin, which is why R8 previously had no gate outside a musl host.
    #[test]
    #[cfg_attr(miri, ignore)]
    fn worker_stack_is_explicitly_sized_not_inherited() {
        let _serialise = lock_recover(&INJECT_LOCK);
        let (r, w) = make_pipe();
        // R3 forbids `expect`, in test code too since clippy gained --all-targets.
        let handle = match Handle::open(1, w) {
            Ok(h) => h,
            Err(e) => panic!("open failed: {}", e),
        };

        let header = CallHeader {
            flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
            ..Default::default()
        };
        let ticket = match handle.submit(header, &[8], 0) {
            Ok(t) => t,
            Err(e) => panic!("submit failed: {}", e),
        };

        // Drain the completion ticket so the worker never blocks writing it.
        let mut buf = [0u8; 8];
        let mut got = 0usize;
        while got < buf.len() {
            // SAFETY: reading into a valid stack buffer from the pipe's read end.
            let n = unsafe {
                libc::read(
                    r,
                    buf.as_mut_ptr().add(got) as *mut libc::c_void,
                    buf.len() - got,
                )
            };
            assert!(n > 0, "completion pipe read failed");
            got += n as usize;
        }
        assert_eq!(u64::from_ne_bytes(buf), ticket);

        let result = match handle.take(ticket) {
            Ok(r) => r,
            Err(e) => panic!("take failed: {}", e),
        };
        let reported = match result {
            JobResult::Ok(out) => {
                assert_eq!(out.len(), 8, "mode 8 returns a u64");
                let mut b = [0u8; 8];
                b.copy_from_slice(&out);
                u64::from_le_bytes(b) as usize
            }
            other => panic!("expected Ok from diagnostic mode 8, got {:?}", other),
        };

        assert_ne!(
            reported, 0,
            "the worker could not read its own stack size, so I5's 8 MiB guarantee \
             is unverified on this platform rather than confirmed"
        );
        assert!(
            reported >= WORKER_STACK_SIZE,
            "worker ran on a {} byte stack, below the {} bytes Gusset requests; on musl \
             the inherited default is 128 KiB (I5/R8)",
            reported,
            WORKER_STACK_SIZE
        );

        handle.close();
        // SAFETY: the read end is still owned by this test; close() took the write end.
        unsafe {
            libc::close(r);
        }
    }

    /// Reads exactly `n` bytes from the pipe's read end.
    fn read_exact_fd(r: i32, n: usize) -> Vec<u8> {
        let mut buf = vec![0u8; n];
        let mut got = 0usize;
        while got < n {
            // SAFETY: reading into the unfilled tail of a valid buffer.
            let m =
                unsafe { libc::read(r, buf.as_mut_ptr().add(got) as *mut libc::c_void, n - got) };
            assert!(m > 0, "completion pipe read failed");
            got += m as usize;
        }
        buf
    }

    fn word(b: &[u8]) -> u64 {
        let mut w = [0u8; 8];
        w.copy_from_slice(&b[..8]);
        u64::from_ne_bytes(w)
    }

    /// GUSSET_FLAG_INLINE_COMPLETION: a success of up to INLINE_RESULT_MAX
    /// bytes arrives in the record and is never stored; one byte more, an
    /// error, or a caller without the flag gets a bare ticket and a take.
    #[test]
    #[cfg_attr(miri, ignore)]
    fn inline_completion_records_carry_small_results_only_when_asked() {
        use crate::header::GUSSET_FLAG_INLINE_COMPLETION;
        let (r, w) = make_pipe();
        let handle = match Handle::open(1, w) {
            Ok(h) => h,
            Err(e) => panic!("open failed: {}", e),
        };
        assert!(
            handle.inline_ok,
            "a 1-worker pool always fits inline records"
        );
        let inline = CallHeader {
            flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE | GUSSET_FLAG_INLINE_COMPLETION,
            ..Default::default()
        };
        let plain = CallHeader {
            flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
            ..Default::default()
        };
        let submit = |h: CallHeader, input: &[u8]| match handle.submit(h, input, 0) {
            Ok(t) => t,
            Err(e) => panic!("submit failed: {}", e),
        };

        // Mode 0 echoes its input, mode byte included.
        for len in [1usize, 7, 8, 9, INLINE_RESULT_MAX - 1, INLINE_RESULT_MAX] {
            let mut input: Vec<u8> = (0..len as u8).collect();
            input[0] = 0;
            let ticket = submit(inline, &input);
            let head = read_exact_fd(r, 16);
            assert_eq!(word(&head), ticket | INLINE_RECORD_FLAG, "len {len}");
            assert_eq!(word(&head[8..]), len as u64);
            let body = read_exact_fd(r, len.div_ceil(8) * 8);
            assert_eq!(&body[..len], &input[..], "len {len}");
            assert!(body[len..].iter().all(|&b| b == 0), "padding is zeroed");
            assert!(
                handle.take(ticket).is_err(),
                "an inline result is not stored"
            );
        }

        let bare = |ticket: u64| {
            let rec = read_exact_fd(r, 8);
            assert_eq!(word(&rec), ticket, "expected a bare ticket");
        };
        // One byte over the limit: stored, bare ticket.
        let big = vec![0u8; INLINE_RESULT_MAX + 1];
        let t = submit(inline, &big);
        bare(t);
        assert!(matches!(handle.take(t), Ok(JobResult::Ok(ref v)) if v == &big));
        // Without the flag, even a one-byte result is a bare ticket.
        let t = submit(plain, &[0]);
        bare(t);
        assert!(matches!(handle.take(t), Ok(JobResult::Ok(ref v)) if v == &[0]));
        // A panic is never inlined (last: it poisons the handle).
        let t = submit(inline, &[1]);
        bare(t);
        assert!(matches!(handle.take(t), Ok(JobResult::Panic { .. })));

        handle.close();
        // SAFETY: the read end is still owned by this test.
        unsafe {
            libc::close(r);
        }
    }

    /// A submit that cannot queue the work unit must not leave a cancel flag
    /// behind. `in_flight` is the flag count; `gusset_shutdown` waits on it, so a
    /// leaked flag after a failed send makes a clean drain impossible.
    ///
    /// The production path inserts the flag and then looks up the sender. If the
    /// sender is already gone, that insert is a leak the worker will never clear.
    #[test]
    #[cfg_attr(miri, ignore)]
    fn submit_does_not_leave_a_cancel_flag_when_the_sender_is_gone() {
        let _serialise = lock_recover(&INJECT_LOCK);
        let (r, w) = make_pipe();
        let handle = match Handle::open(1, w) {
            Ok(h) => h,
            Err(e) => panic!("open failed: {}", e),
        };

        lock_recover(&handle.sender).take();

        let header = CallHeader {
            flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
            ..Default::default()
        };
        if let Ok(ticket) = handle.submit(header, &[0u8], 0) {
            panic!(
                "submit must fail once the sender is gone, queued ticket {}",
                ticket
            );
        }

        assert_eq!(
            handle.in_flight(),
            0,
            "a failed submit must not leak a cancel flag; shutdown drain waits on in_flight"
        );

        handle.close();
        // SAFETY: the read end is still owned by this test; close() took the write end.
        unsafe {
            libc::close(r);
        }
    }

    /// Advancing the id counter past the 63-bit ceiling wraps it onto ids that
    /// are still live. `fetch_add` does that even when the call then returns an
    /// error, so the next successful allocation reuses buffer 1.
    #[test]
    #[cfg_attr(miri, ignore)]
    fn buffer_ids_stop_at_the_ceiling_instead_of_wrapping() {
        let _serialise = lock_recover(&INJECT_LOCK);
        let (r, w) = make_pipe();
        let handle = match Handle::open(1, w) {
            Ok(h) => h,
            Err(e) => panic!("open failed: {}", e),
        };

        let (live_id, _) = match handle.buf_alloc(32) {
            Ok(v) => v,
            Err(e) => panic!("first buffer must allocate: {}", e),
        };
        assert_eq!(live_id, 1, "ids start at 1; 0 means no buffer");

        handle.next_buffer_id.store(1 << 63, Ordering::Relaxed);
        if let Ok((id, _)) = handle.buf_alloc(32) {
            panic!("id {id} is past the ceiling and must be refused");
        }
        assert_eq!(
            handle.next_buffer_id.load(Ordering::Relaxed),
            1 << 63,
            "a refused id must not advance the counter; the next success would wrap onto buffer 1"
        );
        assert!(
            handle.buf_get(live_id).is_ok(),
            "the live buffer must still be the one issued before the ceiling"
        );

        handle.close();
        unsafe {
            libc::close(r);
        }
    }

    /// Same ceiling for tickets. A wrapped ticket id aliases an in-flight job,
    /// so the completion pipe wakes the wrong waiter.
    #[test]
    #[cfg_attr(miri, ignore)]
    fn ticket_ids_stop_at_the_ceiling_instead_of_wrapping() {
        // Tickets come from a process-wide counter now, which a parallel test
        // must not push to the ceiling; the ceiling rule lives in reserve_id.
        let counter = AtomicU64::new(ID_CEILING);
        assert!(
            reserve_id(&counter).is_err(),
            "an id at the ceiling must be refused"
        );
        assert_eq!(
            counter.load(Ordering::Relaxed),
            ID_CEILING,
            "a refused id must not advance the counter"
        );
        let counter = AtomicU64::new(ID_CEILING - 1);
        assert_eq!(reserve_id(&counter).ok(), Some(ID_CEILING - 1));
        assert!(
            reserve_id(&counter).is_err(),
            "the last id is below the ceiling"
        );
    }

    #[test]
    #[cfg_attr(miri, ignore)]
    fn tickets_are_unique_across_handles() {
        let _serialise = lock_recover(&INJECT_LOCK);
        let header = CallHeader {
            flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
            ..Default::default()
        };
        let (ra, wa) = make_pipe();
        let (rb, wb) = make_pipe();
        let a = match Handle::open(1, wa) {
            Ok(h) => h,
            Err(e) => panic!("open a: {e}"),
        };
        let b = match Handle::open(1, wb) {
            Ok(h) => h,
            Err(e) => panic!("open b: {e}"),
        };
        let mut seen = std::collections::HashSet::new();
        for _ in 0..8 {
            for (h, r) in [(&a, ra), (&b, rb)] {
                let t = match h.submit(header, &[0u8], 0) {
                    Ok(t) => t,
                    Err(e) => panic!("submit: {e}"),
                };
                assert_eq!(drain_ticket(r), t);
                assert!(seen.insert(t), "ticket {t} was issued by two handles");
            }
        }
        a.close();
        b.close();
        unsafe {
            libc::close(ra);
            libc::close(rb);
        }
    }

    /// `SyncSender::send` blocks when the queue is full. Held across the sender
    /// lock, that blocks `close` forever; released before the send, it reopens
    /// the cancel-flag leak. `try_send` fails in microseconds and leaves
    /// `in_flight` unchanged.
    #[test]
    #[cfg_attr(miri, ignore)]
    fn submit_on_a_full_queue_fails_without_blocking_or_leaking() {
        let _serialise = lock_recover(&INJECT_LOCK);
        let (r, w) = make_pipe();
        let handle = match Handle::open(1, w) {
            Ok(h) => h,
            Err(e) => panic!("open failed: {}", e),
        };

        let header = CallHeader {
            flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
            ..Default::default()
        };
        // Every accepted job sleeps. One worker cannot drain them before the
        // bounded queue fills, so a correct submit refuses instead of blocking
        // inside `send` until a worker finishes.
        let mut accepted = 0usize;
        let overall = std::time::Instant::now();
        loop {
            if overall.elapsed() > std::time::Duration::from_millis(500) {
                panic!("submit never refused a full queue; accepted {accepted}");
            }
            let started = std::time::Instant::now();
            match handle.submit(header, &[9, 30], 0) {
                Ok(_) => {
                    let elapsed = started.elapsed();
                    assert!(
                        elapsed < std::time::Duration::from_millis(200),
                        "submit blocked for {:?} instead of queueing or refusing",
                        elapsed
                    );
                    accepted += 1;
                }
                Err(e) => {
                    let elapsed = started.elapsed();
                    assert!(
                        elapsed < std::time::Duration::from_millis(200),
                        "a full queue must fail without blocking; took {:?}",
                        elapsed
                    );
                    assert!(
                        e.contains("full"),
                        "expected a full-queue refusal, got: {e}"
                    );
                    assert_eq!(
                        handle.in_flight(),
                        accepted,
                        "the refused submit must not leave a cancel flag"
                    );
                    break;
                }
            }
        }
        assert!(accepted > 0, "the queue should accept at least one job");

        handle.close();
        unsafe {
            libc::close(r);
        }
    }

    /// Drain one 8-byte ticket from the completion pipe.
    fn drain_ticket(read_fd: i32) -> u64 {
        let mut buf = [0u8; 8];
        let mut got = 0usize;
        while got < buf.len() {
            // SAFETY: reading into a valid stack buffer from the pipe's read end.
            let n = unsafe {
                libc::read(
                    read_fd,
                    buf.as_mut_ptr().add(got) as *mut libc::c_void,
                    buf.len() - got,
                )
            };
            assert!(n > 0, "completion pipe read failed");
            got += n as usize;
        }
        u64::from_ne_bytes(buf)
    }

    /// Large `JobResult::Ok` used to be copied into a fresh `RawBuffer` inside
    /// `gusset_take` — on the cgo thread, with an M pinned for the memcpy.
    /// Promoting on the worker keeps take() a pointer return (R16 egress).
    #[test]
    #[cfg_attr(miri, ignore)]
    fn large_ok_result_is_promoted_off_the_cgo_thread() {
        let _serialise = lock_recover(&INJECT_LOCK);
        let (r, w) = make_pipe();
        let handle = match Handle::open(1, w) {
            Ok(h) => h,
            Err(e) => panic!("open failed: {}", e),
        };

        let len = MAX_INLINE_INPUT + 1;
        let (buf_id, ptr) = match handle.buf_alloc(len) {
            Ok(v) => v,
            Err(e) => panic!("buf_alloc failed: {}", e),
        };
        unsafe {
            std::ptr::write(ptr, 0u8);
            for i in 1..len {
                std::ptr::write(ptr.add(i), (i % 256) as u8);
            }
        }

        let header = CallHeader {
            flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
            ..Default::default()
        };
        let ticket = match handle.submit(header, &[], buf_id) {
            Ok(t) => t,
            Err(e) => panic!("submit failed: {}", e),
        };
        assert_eq!(drain_ticket(r), ticket);

        let result = match handle.take(ticket) {
            Ok(r) => r,
            Err(e) => panic!("take failed: {}", e),
        };
        match result {
            JobResult::Buffer(id) => {
                assert_ne!(id, buf_id, "echo must not alias the input buffer id");
                let (out_ptr, out_len) = match handle.buf_get(id) {
                    Ok(v) => v,
                    Err(e) => panic!("buf_get failed: {}", e),
                };
                assert_eq!(out_len, len);
                unsafe {
                    assert_eq!(*out_ptr, 0);
                    assert_eq!(*out_ptr.add(100), 100u8);
                }
                let _ = handle.buf_free(id);
            }
            JobResult::Ok(data) => panic!(
                "large JobResult::Ok must be promoted to Buffer on the worker, still Ok ({} bytes)",
                data.len()
            ),
            other => panic!(
                "large JobResult::Ok must be promoted to Buffer on the worker, got {:?}",
                std::mem::discriminant(&other)
            ),
        }

        let _ = handle.buf_free(buf_id);
        handle.close();
        unsafe {
            libc::close(r);
        }
    }

    /// `NewBuffer` reaches Rust through `gusset_buf_alloc`. That export, not
    /// `Handle::buf_alloc`, is what must mark the slot caller-held: an engine
    /// returning the id is otherwise accepted, and the waiter's free releases
    /// memory Go still views.
    #[test]
    #[cfg_attr(miri, ignore)]
    fn gusset_buf_alloc_marks_the_buffer_caller_held() {
        use crate::ffi::gusset_buf_alloc;
        use crate::ffi::status::{FfiStatus, FFI_OK};

        const OPCODE_RETURN_EXPORTED: u32 = 9201;
        register_engine(OPCODE_RETURN_EXPORTED, |_ctx, input: &[u8]| {
            let mut raw = [0u8; 8];
            let n = input.len().min(8);
            raw[..n].copy_from_slice(&input[..n]);
            Ok::<_, String>(JobOutput::Buffer(u64::from_le_bytes(raw)))
        });

        let (r, w) = make_pipe();
        let handle = match Handle::open(1, w) {
            Ok(h) => h,
            Err(e) => panic!("open failed: {}", e),
        };
        // SAFETY: the Arc stays alive for the call. The export only reads the handle.
        let raw = std::sync::Arc::as_ptr(&handle) as *mut Handle;
        let mut id = 0u64;
        let mut ptr = std::ptr::null_mut();
        let mut status = FfiStatus::ok();
        // SAFETY: `raw` is a live handle; the out-params are local and writable.
        let rc = unsafe { gusset_buf_alloc(raw, 32, &mut id, &mut ptr, &mut status) };
        assert_eq!(
            rc, FFI_OK,
            "gusset_buf_alloc failed with code {}",
            status.code
        );
        assert!(!ptr.is_null());
        // SAFETY: the export just returned a 32-byte buffer.
        unsafe {
            std::ptr::write(ptr, 0x5A);
        }

        let header = CallHeader {
            reserved: OPCODE_RETURN_EXPORTED,
            ..Default::default()
        };
        let ticket = match handle.submit(header, &id.to_le_bytes(), 0) {
            Ok(t) => t,
            Err(e) => panic!("submit failed: {}", e),
        };
        assert_eq!(drain_ticket(r), ticket);

        match handle.take(ticket) {
            Ok(JobResult::Err(msg)) => assert!(
                msg.contains("caller still holds"),
                "refusal must name the caller's view, got: {}",
                msg
            ),
            Ok(JobResult::Buffer(got)) => {
                panic!("gusset_buf_alloc buffer {} was accepted as an output", got)
            }
            Ok(other) => panic!("unexpected result {:?}", std::mem::discriminant(&other)),
            Err(e) => panic!("take failed: {}", e),
        }

        match handle.buf_get(id) {
            Ok((p, len)) => {
                assert_eq!(len, 32);
                // SAFETY: buf_get just confirmed the allocation is live.
                let first = unsafe { std::ptr::read(p) };
                assert_eq!(first, 0x5A, "the exported buffer was disturbed");
            }
            Err(e) => panic!("exported buffer was freed despite the refusal: {}", e),
        }

        let _ = handle.buf_free(id);
        handle.close();
        unsafe {
            libc::close(r);
        }
    }

    /// R16: a raw inline slice above 4 KiB must be refused, not memcpy'd on the
    /// submit path. The Go side parks an M for the whole cgo call; a 1 MiB copy
    /// here is exactly the long cgo call the submit-and-return contract forbids.
    #[test]
    #[cfg_attr(miri, ignore)]
    fn submit_rejects_inline_input_over_4kib() {
        let _serialise = lock_recover(&INJECT_LOCK);
        let (r, w) = make_pipe();
        let handle = match Handle::open(1, w) {
            Ok(h) => h,
            Err(e) => panic!("open failed: {}", e),
        };

        let header = CallHeader {
            flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE,
            ..Default::default()
        };
        let over = vec![0u8; 4096 + 1];
        match handle.submit(header, &over, 0) {
            Ok(ticket) => panic!(
                "inline submit of {} bytes must be refused, queued ticket {}",
                over.len(),
                ticket
            ),
            Err(e) => assert!(
                e.contains("copy limit"),
                "refusal must name the copy limit, got: {}",
                e
            ),
        }

        handle.close();
        // SAFETY: the read end is still owned by this test; close() took the write end.
        unsafe {
            libc::close(r);
        }
    }
}
