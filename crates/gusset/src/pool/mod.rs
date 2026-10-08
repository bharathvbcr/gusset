//! Worker pool and Handle lifecycle with bounded concurrency and signal protection (I4, I5).

// The types every part of the pool shares live here: `Handle`, the work unit
// and its results, and the id space. Each concern is a private child module
// adding its own `impl Handle` block, so all of them see the handle's private
// fields: `worker` (threads and the engine firewall), `completion` (records to
// the reader), `buffers` (the buffer registry), `engine` and `diagnostic`
// (dispatch), and `lifecycle` (shutdown and close). Their public items are
// re-exported below, so every `gusset::pool::` path is the one it always was.

mod buffers;
mod completion;
mod diagnostic;
mod engine;
mod lifecycle;
pub mod queue;
pub mod ring;
pub mod sys;
mod worker;

pub use completion::{INLINE_RECORD_FLAG, INLINE_RECORD_MAX, INLINE_RESULT_MAX};
pub use diagnostic::{diagnostic_dispatch, DIAG_MODE_ALLOCATED};
pub use engine::{
    clear_engine_handlers, default_dispatch, has_engine_handler, register_engine,
    set_engine_handler, EngineFn,
};
pub use lifecycle::{begin_shutdown, is_shutting_down, rearm, shutdown, total_in_flight};
pub use worker::WORKER_STACK_SIZE;

use crate::ffi::guard::install_panic_hook;
use crate::header::{CallHeader, CancelReason, JobContext, GUSSET_FLAGS_KNOWN};
use buffers::BufferSlot;
use completion::size_completion_pipe;
use lifecycle::LIVE_HANDLES;
use queue::{JobQueue, PushError, QueueSender};
use std::collections::HashMap;
use std::hash::{BuildHasherDefault, Hasher};
use std::sync::atomic::{AtomicBool, AtomicI32, AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, Weak};
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
    /// Set while the pool may hold fewer than `pool_size` workers for a reason
    /// `exited` no longer records: a respawn that failed (thread limit) or
    /// unwound partway. The reap consumes `exited` before it spawns, so without
    /// this the fast path in `ensure_workers` saw 0 and never retried; with no
    /// workers left, every later submission queued and never ran (I4).
    respawn_pending: AtomicBool,
    /// Whether the completion pipe holds `pool_size` inline records. If it
    /// could only be sized for bare tickets, inline completions are off and
    /// every caller gets tickets, whatever it asked for.
    inline_ok: bool,
    /// The shared-memory completion ring, once a reader attaches one
    /// (`gusset_handle_ring`). Until then, and for any host that never does,
    /// completions go through the pipe.
    ring: std::sync::OnceLock<Arc<ring::Ring>>,
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
/// behind the completion pipe, which is what keeps `write_completion` from ever facing
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

impl Handle {
    /// Opens a new handle with a dedicated worker pool of pool_size threads.
    ///
    /// Returns `Err` for a pool size above [`MAX_POOL_SIZE`] rather than attempting
    /// the spawn: refusing loudly beats discovering the ceiling as a partial spawn
    /// failure halfway through creating thousands of threads.
    ///
    /// `pipe_write_fd` stays the caller's until `open` succeeds. A refusal closes
    /// nothing and leaves its status flags as they were; on success Gusset owns
    /// it and has made it non-blocking (and NOSIGPIPE on Darwin).
    pub fn open(pool_size: u32, pipe_write_fd: i32) -> Result<Arc<Self>, String> {
        install_panic_hook();

        let pool_size = if pool_size == 0 {
            DEFAULT_POOL_SIZE
        } else {
            pool_size as usize
        };

        // Every argument is checked before anything changes. The descriptor is
        // the caller's until the pool is up, so a refusal must leave it exactly
        // as it was handed in: set_nonblocking used to run first, and an open
        // refused for its pool size still left the caller's pipe O_NONBLOCK
        // (and NOSIGPIPE on Darwin).
        if pipe_write_fd < 0 {
            return Err("pipe_write_fd must be non-negative".to_string());
        }
        if pool_size > MAX_POOL_SIZE {
            return Err(format!(
                "pool_size {} exceeds maximum {} (each worker is an OS thread with an 8 MiB stack)",
                pool_size, MAX_POOL_SIZE
            ));
        }
        sys::check_open(pipe_write_fd)
            .map_err(|e| format!("pipe write fd {} is not open: {}", pipe_write_fd, e))?;
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
        //
        // Growing the pipe is the one change made before the pool is up. It only
        // ever adds capacity, so a later failure leaves the caller a pipe that
        // holds more, never one that behaves differently.
        let inline_ok = size_completion_pipe(pool_size, cfg!(target_os = "linux"), |bytes| {
            sys::ensure_pipe_capacity(pipe_write_fd, bytes)
        })?;
        #[cfg(test)]
        let inline_ok = inline_ok && !tests::force_ticket_only();

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
            respawn_pending: AtomicBool::new(false),
            inline_ok,
            ring: std::sync::OnceLock::new(),
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

        // Only now that nothing else can fail does the descriptor change mode.
        // Workers write it only after the store below, so nothing writes it
        // blocking in between. Should this fail, dropping `handle` joins the
        // workers and closes nothing: the descriptor is not published yet.
        sys::set_nonblocking(pipe_write_fd)
            .map_err(|e| format!("failed to set non-blocking on pipe write fd: {}", e))?;

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

    /// Latches the poison flag: a panic was caught on this handle's behalf (I2).
    pub(crate) fn poison(&self) {
        self.poisoned.store(true, Ordering::Release);
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

#[cfg(test)]
// These tests drive pipe(2)/read(2)/write(2) directly, because the descriptor
// ownership they check is only observable at the syscall level. The allow is
// scoped to the test module: `pool` itself stays under the crate-wide
// `deny(unsafe_code)`, with `pool::sys` the only module permitted unsafe.
#[allow(unsafe_code)]
mod tests;
