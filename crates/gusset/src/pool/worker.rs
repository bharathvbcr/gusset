//! Worker threads: running a unit behind its firewall, and respawning (I2, I4, I5).

use super::buffers::materialize_result;
use super::engine::default_dispatch;
use super::{lock_recover, sys, Handle, JobOutput, JobResult, TaskPayload, WorkUnit};
use crate::ffi::guard::{drop_panic_payload, extract_panic_payload, take_panic_location};
use std::panic::{catch_unwind, AssertUnwindSafe};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Weak};
use std::thread;

/// Explicit worker stack size (R8).
///
/// cgo-created threads inherit the pthread default, which is 128 KiB on musl. Heavy
/// work runs here, not on the caller's g0 stack, so the size is set explicitly
/// rather than inherited.
pub const WORKER_STACK_SIZE: usize = 8 * 1024 * 1024;

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
        // An engine that stops because `ctx.check()` failed reports it in its
        // own words ("deadline exceeded"), and Go classifies a cancel only by the
        // runtime's exact message. Re-checking here makes the runtime, not the
        // engine's wording, decide: the documented I3 pattern now surfaces as
        // context.DeadlineExceeded / context.Canceled. An unrelated engine error
        // that lands after the deadline is reported as the deadline — the
        // caller's deadline had passed either way, and Go's ctx says the same.
        Ok(Err(err)) => match unit.ctx.check() {
            Err(reason) => JobResult::Cancelled(reason),
            Ok(()) => JobResult::Err(err),
        },
        Err(payload) => caught_panic(weak, payload, |msg| msg),
    }
}

/// The result for a panic caught on a worker, either firewall: the handle is
/// poisoned (I2) and the panic reported where it happened, its message worded
/// by `describe`.
///
/// Location first: disposing of the payload can panic again and record the
/// destructor's location over the original's.
fn caught_panic(
    weak: &Weak<Handle>,
    payload: Box<dyn std::any::Any + Send>,
    describe: impl FnOnce(String) -> String,
) -> JobResult {
    if let Some(h) = weak.upgrade() {
        h.poison();
    }
    let loc = take_panic_location();
    let msg = extract_panic_payload(payload);
    JobResult::Panic {
        msg: describe(msg),
        file: loc.map(|l| l.file),
        line: loc.map(|l| l.line).unwrap_or(0),
    }
}

impl Handle {
    pub(super) fn spawn_workers(&self, count: usize) -> Result<(), String> {
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
            if super::tests::spawn_should_fail() {
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
                        #[cfg(test)]
                        let kill_me = unit.ctx.header().trace_id == super::tests::KILL_TRACE;
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
                            Err(payload) => caught_panic(&weak_clone, payload, |msg| {
                                format!("worker fault outside the engine call: {}", msg)
                            }),
                        };

                        // Store result and wake netpoller if handle still alive
                        if let Some(h) = weak_clone.upgrade() {
                            let result = materialize_result(&h, result);
                            h.complete(ticket, result, wants_inline);
                            // Only now is the unit out of flight: shutdown's drain
                            // counts cancel flags, and removing this one before the
                            // write let `gusset_shutdown` report a clean drain while
                            // a worker still sat in the up-to-10 s write backoff.
                            lock_recover(&h.cancel_flags).remove(&ticket);
                        }
                        // Test-only worker death (see tests::KILL_TRACE):
                        // the worker that ran a marked unit exits once it
                        // has delivered the completion.
                        #[cfg(test)]
                        if kill_me {
                            break;
                        }
                    }
                })
                .map_err(|e| format!("failed to spawn worker thread: {}", e))?;

            workers.push(join_handle);
        }

        Ok(())
    }

    /// Verifies worker health and respawns replacement workers if any died (I5).
    pub(super) fn ensure_workers(&self) -> Result<(), String> {
        // Fast path: no worker has exited and no earlier respawn fell short,
        // so there is nothing to reap or replace.
        if self.exited.load(Ordering::Acquire) == 0 && !self.respawn_pending.load(Ordering::Acquire)
        {
            return Ok(());
        }
        let mut workers = lock_recover(&self.workers);
        // Re-check under the lock: close drains this vec and then joins. A spawn
        // that raced past a pre-lock closed check would leave JoinHandles nobody
        // joins, detaching OS threads on a handle that is already shut down.
        if self.closed.load(Ordering::Acquire) {
            return Ok(());
        }
        // Raised before `exited` is consumed and cleared only once the pool
        // is whole again. A spawn that returns Err or unwinds partway leaves
        // it set, so the next submit retries instead of trusting a fast path
        // that reads `exited == 0`. Raised ahead of the fetch_sub (AcqRel), a
        // concurrent fast path that sees the decremented count sees this too.
        self.respawn_pending.store(true, Ordering::Release);
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
        self.respawn_pending.store(false, Ordering::Release);

        Ok(())
    }
}
