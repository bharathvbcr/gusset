//! Process-wide shutdown, the live-handle registry, and closing a handle (I4).

use super::{lock_recover, sys, Handle};
use crate::ffi::guard::drop_panic_payload;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, Weak};
use std::thread;

/// Set by [`begin_shutdown`]; cleared by [`rearm`].
///
/// While set, `Handle::submit` refuses new work. Draining is meaningless if fresh
/// submissions keep arriving behind the drain loop.
static SHUTTING_DOWN: AtomicBool = AtomicBool::new(false);

/// Every handle opened in this process, weakly held.
///
/// Weak so the registry never keeps a handle alive past its own close, and so a
/// leaked registry entry costs one pointer rather than a worker pool.
pub(super) static LIVE_HANDLES: Mutex<Vec<Weak<Handle>>> = Mutex::new(Vec::new());

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

impl Handle {
    /// Joins worker threads for at most this long, then returns.
    ///
    /// Longer than any diagnostic work unit (mode 9 sleeps at most 2.55 s) so an
    /// ordinary `Close` still waits the job out. A worker that never returns —
    /// an engine that ignores `JobContext::check` and does not finish — stops
    /// blocking the caller here instead of joining forever.
    pub const CLOSE_JOIN_BUDGET: std::time::Duration = std::time::Duration::from_secs(30);

    /// Closes the handle, disconnects workers, joins them, and closes the write fd.
    ///
    /// `Ok(())` means every worker this call had to join has exited and the
    /// completion pipe is closed. `Err` means one was still inside an engine
    /// call when [`CLOSE_JOIN_BUDGET`](Self::CLOSE_JOIN_BUDGET) expired. In that case a background
    /// thread keeps the `Arc` and the `JoinHandle`s until the workers exit, so
    /// dropping the caller's `Arc` does not free the pool under them. The
    /// error is the signal that the join did not finish; a second `close` on
    /// an already-closed handle returns `Ok` and does not repeat it.
    ///
    /// `Drop` of a handle that was never closed joins on the dropping thread.
    /// That path has no strong `Arc` left to hand to a background joiner, so
    /// it cannot return early without freeing memory a worker is still using.
    /// Go's `Close` always goes through an explicit close while the `Arc` is
    /// alive, which is the bounded path.
    pub fn close(&self) -> Result<(), String> {
        self.close_within(Self::CLOSE_JOIN_BUDGET)
    }

    /// [`close`](Self::close) with an explicit join budget. Tests use a short one.
    pub(super) fn close_within(&self, budget: std::time::Duration) -> Result<(), String> {
        if self.closed.swap(true, Ordering::SeqCst) {
            return Ok(());
        }

        // 1. Disconnect sender so workers unblock from pop(). Skipping this
        // would leave every worker parked forever and hang the join.
        lock_recover(&self.sender).take();

        // 2. Signal cooperative cancellation to all active jobs.
        self.cancel_all();

        // 3. Take the worker threads. `closed` is already set, and
        // `ensure_workers` re-checks it under this same mutex, so the lock
        // does not have to be held for the whole join: a spawn that arrives
        // after the drain sees `closed` and returns.
        let handles: Vec<_> = {
            let mut workers = lock_recover(&self.workers);
            workers.drain(..).collect()
        };
        self.join_workers(handles, budget)
    }

    /// Joins `handles`, skipping this thread, within `budget` when a strong
    /// `Arc` can outlive the call.
    fn join_workers(
        &self,
        handles: Vec<thread::JoinHandle<()>>,
        budget: std::time::Duration,
    ) -> Result<(), String> {
        let me = thread::current().id();
        let mut foreign = Vec::with_capacity(handles.len());
        for handle in handles {
            // A worker can run this close itself: it upgrades its Weak to
            // publish a completion, and if every other Arc was dropped
            // meanwhile, Drop runs here, on that worker. Joining our own
            // thread fails with EDEADLK and std panics outside any firewall.
            // Dropping the JoinHandle detaches it. The sender is already
            // gone, so it exits on its next pop.
            if handle.thread().id() == me {
                continue;
            }
            foreign.push(handle);
        }
        if foreign.is_empty() {
            self.release_pipe();
            return Ok(());
        }

        // Inside Drop the strong count is already zero, so nothing can keep
        // the allocation alive on another thread. Join here. Go's Close
        // never arrives through Drop: it holds the Arc across the call.
        let Some(keeper) = lock_recover(&self.self_weak).upgrade() else {
            for handle in foreign {
                if let Err(payload) = handle.join() {
                    drop_panic_payload(payload);
                }
            }
            self.release_pipe();
            return Ok(());
        };

        // The JoinHandles live in an Arc so a failed `spawn` — which drops the
        // closure — cannot detach them. The joiner takes them out; this thread
        // only takes them if the joiner never started.
        let slots = Arc::new(Mutex::new(foreign));
        let slots_for_joiner = Arc::clone(&slots);
        let (tx, rx) = std::sync::mpsc::sync_channel(1);
        let spawned = thread::Builder::new()
            .name("gusset-close".to_string())
            .spawn(move || {
                let handles = std::mem::take(&mut *lock_recover(&slots_for_joiner));
                for handle in handles {
                    // A worker that died hands back its panic payload here, and
                    // close runs under ffi_guard_code inside an extern "C" export: a
                    // payload whose destructor panics must not unwind out of the
                    // joiner into the process.
                    if let Err(payload) = handle.join() {
                        drop_panic_payload(payload);
                    }
                }
                keeper.release_pipe();
                let _ = tx.send(());
            });
        let joiner = match spawned {
            Ok(j) => j,
            Err(_) => {
                // `keeper` died with the closure. The caller's Arc is still
                // alive, and the JoinHandles are still in `slots`.
                let handles = std::mem::take(&mut *lock_recover(&slots));
                for handle in handles {
                    if let Err(payload) = handle.join() {
                        drop_panic_payload(payload);
                    }
                }
                self.release_pipe();
                return Ok(());
            }
        };
        match rx.recv_timeout(budget) {
            Ok(()) => {
                // The joiner already released the pipe and dropped `keeper`.
                let _ = joiner.join();
                Ok(())
            }
            Err(std::sync::mpsc::RecvTimeoutError::Timeout) => {
                // Detach the joiner. It still owns `keeper` and the
                // JoinHandles, so the pool is not freed under a live worker
                // and the threads are not detached from their results.
                drop(joiner);
                Err(format!(
                    "close: workers still running after {budget:?}; the pool stays \
                     alive until they exit and its memory is not freed under them"
                ))
            }
            Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => {
                let _ = joiner.join();
                Err("close: joiner exited before the workers were joined".to_string())
            }
        }
    }

    /// Closes the completion-pipe write end once. A second call finds -1.
    ///
    /// Under `pipe_write_lock`, so no writer holds a loaded copy of the number
    /// while it is closed and possibly reused.
    fn release_pipe(&self) {
        let _w = lock_recover(&self.pipe_write_lock);
        let fd = self.pipe_write_fd.swap(-1, Ordering::AcqRel);
        sys::close_fd(fd);
    }
}

impl Drop for Handle {
    fn drop(&mut self) {
        // Already closed when a timed-out close parked an Arc on the joiner.
        // A handle dropped without an explicit close joins on this thread:
        // the strong count is zero, so close_within cannot park a clone.
        let _ = self.close();
    }
}
