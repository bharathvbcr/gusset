//! Bounded work queue with spin-then-park workers.
//!
//! Replaces `sync_channel` + `Mutex<Receiver>`. With a shared receiver, one
//! idle worker holds the receiver lock while blocked in `recv`, so the next
//! unit always goes to a thread asleep in the kernel. Waking it is a futex
//! wake, and on virtualized hosts a wake into an idle vCPU costs tens of
//! microseconds — measured at 41 µs of a 56 µs Rust-only round trip.
//!
//! Here nobody holds a lock while waiting. A worker that has just finished a
//! unit polls the queue for [`WORKER_SPIN`] before it sleeps, so a
//! request/response caller that submits again right away hands the unit to a
//! thread that is already running. `push` skips the wake-up syscall entirely
//! while any worker is polling. An idle pool polls once for that window and
//! then sleeps exactly as before, so it costs no CPU at rest.

use std::collections::VecDeque;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Condvar, Mutex, MutexGuard};
use std::time::{Duration, Instant};

/// How long an idle worker polls before parking.
pub const WORKER_SPIN: Duration = Duration::from_micros(50);

/// The first part of [`WORKER_SPIN`], polled without yielding.
///
/// A request/response caller resubmits a few microseconds after its result
/// lands. `sched_yield` in that window let a spinning Go thread take the
/// worker's core, and the resubmitted unit then waited for the worker to be
/// scheduled back: the 9-20 us tail at p90 of a serial call on 4 vCPUs.
/// After this, polls yield as before, so a longer idle spin still gives up
/// its core to threads that have work.
pub const WORKER_SPIN_HOT: Duration = Duration::from_micros(5);

struct State<T> {
    items: VecDeque<T>,
    closed: bool,
    sleepers: usize,
}

/// The shared queue. Workers hold an `Arc` and call [`JobQueue::pop`].
pub struct JobQueue<T> {
    state: Mutex<State<T>>,
    ready: Condvar,
    cap: usize,
    /// Workers currently in their polling phase.
    spinning: AtomicUsize,
    /// `items.len()`, readable without the lock.
    ///
    /// Pollers read this and take the lock only when it is nonzero
    /// (test-and-test-and-set). Polling with `try_lock` was a CAS each time,
    /// which pulls the mutex's cache line exclusive on every poll: the
    /// submitter's `lock` then fought a spinning worker for that line, and a
    /// Rust-only submit cost ~750 ns for ~600 instructions. A load keeps the
    /// line shared until a push actually writes it.
    queued: AtomicUsize,
    /// `closed`, readable without the lock, so an empty closed queue ends
    /// the poll at once instead of spinning out the window.
    closed: AtomicBool,
}

/// Why a push was refused.
#[derive(Debug)]
pub enum PushError<T> {
    /// The queue holds `cap` units already.
    Full(T),
    /// The sending side is gone (handle closing).
    Closed(T),
}

fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

impl<T> JobQueue<T> {
    /// A queue holding at most `cap` units, and its sending half.
    pub fn new(cap: usize) -> (QueueSender<T>, Arc<JobQueue<T>>) {
        let q = Arc::new(JobQueue {
            state: Mutex::new(State {
                items: VecDeque::with_capacity(cap),
                closed: false,
                sleepers: 0,
            }),
            ready: Condvar::new(),
            cap: cap.max(1),
            spinning: AtomicUsize::new(0),
            queued: AtomicUsize::new(0),
            closed: AtomicBool::new(false),
        });
        (QueueSender(Arc::clone(&q)), q)
    }

    fn push(&self, item: T) -> Result<(), PushError<T>> {
        let wake = {
            let mut st = lock(&self.state);
            if st.closed {
                return Err(PushError::Closed(item));
            }
            if st.items.len() >= self.cap {
                return Err(PushError::Full(item));
            }
            st.items.push_back(item);
            self.queued.store(st.items.len(), Ordering::Release);
            // Wake a sleeper only for units the polling workers cannot take
            // right away: waking one for a unit a poller is about to grab
            // costs a syscall and a wake that finds nothing, while skipping
            // it for a second unit would serialize it behind the first. SeqCst
            // pairs with the decrement in `pop`: a worker that stopped polling
            // before this read re-checks the queue under the lock before it
            // sleeps, so no unit is stranded.
            st.sleepers > 0 && st.items.len() > self.spinning.load(Ordering::SeqCst)
        };
        if wake {
            self.ready.notify_one();
        }
        Ok(())
    }

    fn close(&self) {
        lock(&self.state).closed = true;
        self.closed.store(true, Ordering::Release);
        self.ready.notify_all();
    }

    /// The next unit, or `None` once the queue is closed and drained.
    ///
    /// Units queued before close are still handed out, as a disconnected
    /// channel drains its buffer before reporting the disconnect.
    pub fn pop(&self) -> Option<T> {
        self.spinning.fetch_add(1, Ordering::SeqCst);
        let start = Instant::now();
        let mut hot = true;
        let mut polls = 0u32;
        loop {
            // Test, then test-and-set: the lock is touched only when a load
            // says there is something to take (see `queued`). try_lock, since
            // another worker taking a unit right now is fine: poll again
            // rather than queue behind it.
            if self.queued.load(Ordering::Acquire) > 0 {
                if let Ok(mut st) = self.state.try_lock() {
                    if let Some(item) = st.items.pop_front() {
                        self.queued.store(st.items.len(), Ordering::Release);
                        drop(st);
                        self.spinning.fetch_sub(1, Ordering::SeqCst);
                        return Some(item);
                    }
                }
            } else if self.closed.load(Ordering::Acquire) {
                // Closed and (as far as the load shows) empty: confirm under
                // the lock below, which also hands out a unit raced in.
                break;
            }
            polls = polls.wrapping_add(1);
            if polls.is_multiple_of(32) {
                let spent = start.elapsed();
                if spent >= WORKER_SPIN {
                    break;
                }
                hot = spent < WORKER_SPIN_HOT;
            }
            // Past the hot window, yield rather than burn: under load, the
            // core this poll would occupy belongs to a worker with a unit to
            // run. Pure spinning cost ~6% on 1 ms parallel jobs on 4 vCPUs;
            // sched_yield returns at once when nothing else is runnable, so
            // an idle poll stays as fast as a spin.
            if !hot && polls.is_multiple_of(4) {
                std::thread::yield_now();
            } else {
                std::hint::spin_loop();
            }
        }
        self.spinning.fetch_sub(1, Ordering::SeqCst);

        let mut st = lock(&self.state);
        loop {
            if let Some(item) = st.items.pop_front() {
                self.queued.store(st.items.len(), Ordering::Release);
                return Some(item);
            }
            if st.closed {
                return None;
            }
            st.sleepers += 1;
            st = self.ready.wait(st).unwrap_or_else(|e| e.into_inner());
            st.sleepers -= 1;
        }
    }
}

/// The sending half. Dropping it closes the queue, like dropping the last
/// `SyncSender`: workers drain what is queued, then see `None`.
pub struct QueueSender<T>(Arc<JobQueue<T>>);

impl<T> QueueSender<T> {
    /// Queues `item` without blocking.
    pub fn try_send(&self, item: T) -> Result<(), PushError<T>> {
        self.0.push(item)
    }
}

impl<T> Drop for QueueSender<T> {
    fn drop(&mut self) {
        self.0.close();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fifo_bounded_and_drains_after_close() {
        let (tx, q) = JobQueue::new(2);
        assert!(tx.try_send(1).is_ok());
        assert!(tx.try_send(2).is_ok());
        assert!(matches!(tx.try_send(3), Err(PushError::Full(3))));
        assert_eq!(q.pop(), Some(1));
        drop(tx);
        assert_eq!(q.pop(), Some(2), "queued units survive close");
        assert_eq!(q.pop(), None);
    }

    #[test]
    #[cfg_attr(miri, ignore)]
    fn no_unit_is_lost_or_duplicated_across_sleepers_and_spinners() {
        let (tx, q) = JobQueue::new(1024);
        let total = 20_000u64;
        let workers: Vec<_> = (0..4)
            .map(|_| {
                let q = Arc::clone(&q);
                std::thread::spawn(move || {
                    let mut got = Vec::new();
                    while let Some(v) = q.pop() {
                        got.push(v);
                    }
                    got
                })
            })
            .collect();
        for i in 0..total {
            let mut v = i;
            loop {
                match tx.try_send(v) {
                    Ok(()) => break,
                    Err(PushError::Full(back)) => {
                        v = back;
                        std::thread::yield_now();
                    }
                    Err(PushError::Closed(_)) => panic!("closed early"),
                }
            }
            // Bursts and gaps, so workers cross between polling and sleeping.
            if i % 997 == 0 {
                std::thread::sleep(Duration::from_micros(200));
            }
        }
        drop(tx);
        let mut all: Vec<u64> = workers
            .into_iter()
            .flat_map(|w| match w.join() {
                Ok(v) => v,
                Err(_) => panic!("worker panicked"),
            })
            .collect();
        all.sort_unstable();
        assert_eq!(all.len() as u64, total);
        assert!(all.iter().copied().eq(0..total));
    }
}
