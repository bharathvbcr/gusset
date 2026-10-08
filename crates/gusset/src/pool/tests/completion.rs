use super::{make_pipe, Must, FORCE_TICKET_ONLY};
use crate::header::{CallHeader, GUSSET_FLAG_DIAGNOSTIC_ENGINE};
use crate::pool::completion::{size_completion_pipe, write_completion};
use crate::pool::{
    lock_recover, sys, Handle, IdMap, JobResult, INLINE_RECORD_FLAG, INLINE_RECORD_MAX,
    INLINE_RESULT_MAX,
};
use std::sync::atomic::{AtomicBool, AtomicI32, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant};

/// A non-blocking pipe whose buffer is full, returning `(read_fd, write_fd)`.
fn full_pipe() -> (i32, i32) {
    let (r, w) = make_pipe();
    sys::set_nonblocking(w).must("set_nonblocking");
    let filler = [0u8; 4096];
    // SAFETY: writing from a valid stack buffer until the kernel refuses.
    while unsafe { libc::write(w, filler.as_ptr() as *const libc::c_void, filler.len()) } > 0 {}
    (r, w)
}

/// A full completion pipe used to spin `yield_now()` forever, hanging
/// `Handle::close` in `join`. `write_completion` keeps retrying a live reader
/// by design, but `close` sets `closed` first, so a writer on a pipe nobody
/// will drain gives up within a backoff step of it.
///
/// Ported from `write_ticket_gives_up_on_a_permanently_full_pipe`, which
/// tested a bounded writer no production path used.
#[test]
#[cfg_attr(miri, ignore)]
fn write_completion_gives_up_on_a_full_pipe_once_the_handle_closes() {
    let (r, w) = full_pipe();
    let lock = Mutex::new(());
    let fd = AtomicI32::new(w);
    let closed = Arc::new(AtomicBool::new(false));

    let closer = {
        let closed = Arc::clone(&closed);
        thread::spawn(move || {
            thread::sleep(Duration::from_millis(200));
            closed.store(true, Ordering::Release);
        })
    };
    let started = Instant::now();
    let result = write_completion(&lock, &fd, &closed, 42, &42u64.to_ne_bytes());
    let elapsed = started.elapsed();
    closer.join().must("closer");

    match result {
        Err(e) => assert_eq!(e.kind(), std::io::ErrorKind::NotConnected, "{e}"),
        Ok(()) => panic!("a full pipe nobody drains reported a delivered completion"),
    }
    assert!(
        elapsed >= Duration::from_millis(200),
        "gave up before the handle closed: a live reader would lose the ticket"
    );
    assert!(
        elapsed < Duration::from_secs(5),
        "still writing {elapsed:?} after close"
    );
    // SAFETY: both ends are owned by this test.
    unsafe {
        libc::close(r);
        libc::close(w);
    }
}

/// A slow-but-live reader is served: the retry path makes progress rather
/// than giving up, and the record arrives whole after the filler.
///
/// Ported from `write_ticket_succeeds_once_a_stalled_reader_drains`.
#[test]
#[cfg_attr(miri, ignore)]
fn write_completion_succeeds_once_a_stalled_reader_drains() {
    let (r, w) = full_pipe();
    let lock = Mutex::new(());
    let fd = AtomicI32::new(w);
    let closed = AtomicBool::new(false);

    let reader = thread::spawn(move || {
        thread::sleep(Duration::from_millis(200));
        let mut sink = vec![0u8; 1 << 20];
        let mut got = Vec::new();
        // Drain until the record lands behind the filler: every filler byte
        // is zero, so the ticket is the only nonzero word.
        while !got.ends_with(&7u64.to_ne_bytes()) {
            // SAFETY: reading into a valid heap buffer from our read end.
            let n = unsafe { libc::read(r, sink.as_mut_ptr() as *mut libc::c_void, sink.len()) };
            assert!(n > 0, "pipe read failed");
            got.extend_from_slice(&sink[..n as usize]);
        }
        r
    });

    let result = write_completion(&lock, &fd, &closed, 7, &7u64.to_ne_bytes());
    if let Err(e) = result {
        panic!("write must succeed once the reader drains, got {e}");
    }
    let r = reader.join().must("reader");
    // SAFETY: both ends are owned by this test.
    unsafe {
        libc::close(r);
        libc::close(w);
    }
}

#[test]
fn completion_pipe_sizing_falls_back_to_tickets_then_refuses() {
    let limit = |max: usize| {
        move |bytes: usize| {
            if bytes <= max {
                Ok(())
            } else {
                Err(format!("cannot grow to {bytes}"))
            }
        }
    };
    // Room for 4 records: inline.
    assert_eq!(
        size_completion_pipe(4, true, limit(4 * INLINE_RECORD_MAX)),
        Ok(true)
    );
    // Room for tickets but not records: tickets, not an error.
    assert_eq!(size_completion_pipe(4, true, limit(4 * 8)), Ok(false));
    // Not even tickets: refused.
    assert!(size_completion_pipe(4, true, limit(4 * 8 - 1)).is_err());
    // Cannot grow: records only while pool_size of them fit in 16 KiB.
    let any = |_: usize| Ok(());
    assert_eq!(size_completion_pipe(256, false, any), Ok(true));
    assert_eq!(size_completion_pipe(257, false, any), Ok(false));
}

/// With the pipe sized for tickets only, a caller that asks for inline
/// records still gets bare tickets, and every result is taken as before.
#[test]
#[cfg_attr(miri, ignore)]
fn ticket_only_pipe_ignores_the_inline_flag() {
    use crate::header::GUSSET_FLAG_INLINE_COMPLETION;
    let (r, w) = make_pipe();
    *lock_recover(&FORCE_TICKET_ONLY) = Some(std::thread::current().id());
    let opened = Handle::open(1, w);
    *lock_recover(&FORCE_TICKET_ONLY) = None;
    let handle = match opened {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };
    assert!(!handle.inline_ok);
    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE | GUSSET_FLAG_INLINE_COMPLETION,
        ..Default::default()
    };
    for input in [&[0u8][..], &[0, 1, 2, 3, 4, 5, 6, 7, 8]] {
        let ticket = match handle.submit(header, input, 0) {
            Ok(t) => t,
            Err(e) => panic!("submit failed: {}", e),
        };
        assert_eq!(word(&read_exact_fd(r, 8)), ticket, "expected a bare ticket");
        assert!(matches!(handle.take(ticket), Ok(JobResult::Ok(ref v)) if v == input));
    }
    handle.close().must("close");
    // SAFETY: the read end is still owned by this test.
    unsafe {
        libc::close(r);
    }
}

/// Reads exactly `n` bytes from the pipe's read end.
fn read_exact_fd(r: i32, n: usize) -> Vec<u8> {
    let mut buf = vec![0u8; n];
    let mut got = 0usize;
    let start = std::time::Instant::now();
    while got < n {
        // SAFETY: reading into the unfilled tail of a valid buffer.
        let m = unsafe { libc::read(r, buf.as_mut_ptr().add(got) as *mut libc::c_void, n - got) };
        if m < 0 && std::io::Error::last_os_error().kind() == std::io::ErrorKind::WouldBlock {
            // A non-blocking read end: the write is on its way.
            assert!(
                start.elapsed() < std::time::Duration::from_secs(10),
                "timed out"
            );
            std::thread::yield_now();
            continue;
        }
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

    handle.close().must("close");
    // SAFETY: the read end is still owned by this test.
    unsafe {
        libc::close(r);
    }
}

/// With a ring attached, completions land in its slots and the pipe stays
/// silent; a full ring spills into the pipe and counts the overflow; a
/// reader that announced it is parking gets exactly one wake token.
#[test]
#[cfg_attr(miri, ignore)]
fn ring_carries_completions_spills_when_full_and_wakes_a_parked_reader() {
    use crate::header::GUSSET_FLAG_INLINE_COMPLETION;
    use std::sync::atomic::Ordering::SeqCst;
    let (r, w) = make_pipe();
    // SAFETY: fcntl on a descriptor this test owns.
    unsafe { libc::fcntl(r, libc::F_SETFL, libc::O_NONBLOCK) };
    let handle = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };
    let ring = match handle.attach_ring() {
        Ok(r) => r,
        Err(e) => panic!("attach failed: {}", e),
    };
    assert!(handle.attach_ring().is_err(), "one reader, one ring");
    assert_eq!(ring.shared().capacity, 2, "pool 1 rounds up to 2 slots");
    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE | GUSSET_FLAG_INLINE_COMPLETION,
        ..Default::default()
    };
    let pipe_empty = || {
        let mut b = [0u8; 8];
        // SAFETY: non-blocking read into a valid stack buffer.
        let n = unsafe { libc::read(r, b.as_mut_ptr() as *mut libc::c_void, 8) };
        n < 0
    };
    let settle = |pred: &dyn Fn() -> bool| {
        let start = std::time::Instant::now();
        while !pred() {
            assert!(
                start.elapsed() < std::time::Duration::from_secs(10),
                "timed out"
            );
            std::thread::yield_now();
        }
    };

    // Three completions, none consumed: two fill the ring, the third
    // spills into the pipe.
    // One at a time: the worker's queue holds only two units. A unit's
    // cancel flag is removed once its completion is published.
    let t: Vec<u64> = (0..3u8)
        .map(|i| {
            let t = match handle.submit(header, &[0, i], 0) {
                Ok(t) => t,
                Err(e) => panic!("submit failed: {}", e),
            };
            settle(&|| lock_recover(&handle.cancel_flags).is_empty());
            t
        })
        .collect();
    assert_eq!(ring.shared().overflow.load(SeqCst), 1);
    let spilled = read_exact_fd(r, 24);
    assert_eq!(word(&spilled) & !INLINE_RECORD_FLAG, t[2]);
    let mut head = 0u64;
    for (i, &ticket) in t[..2].iter().enumerate() {
        let words = match ring.pop_for_test(&mut head) {
            Some(w) => w,
            None => panic!("ring slot {i} empty"),
        };
        assert_eq!(words[0], ticket | INLINE_RECORD_FLAG);
        assert_eq!(words[1], 2, "two-byte echo");
        assert_eq!(words[2].to_ne_bytes()[..2], [0, i as u8]);
    }
    assert!(pipe_empty(), "ring completions must not touch the pipe");

    // A reader that is about to park gets one token for the next publish.
    ring.shared().waiting.store(1, SeqCst);
    let t4 = match handle.submit(header, &[0], 0) {
        Ok(t) => t,
        Err(e) => panic!("submit failed: {}", e),
    };
    assert_eq!(word(&read_exact_fd(r, 8)), 0, "wake token is ticket 0");
    assert_eq!(ring.shared().waiting.load(SeqCst), 0);
    let words = match ring.pop_for_test(&mut head) {
        Some(w) => w,
        None => panic!("woken reader found the ring empty"),
    };
    assert_eq!(words[0], t4 | INLINE_RECORD_FLAG);
    assert!(pipe_empty(), "one park, one token");

    // Bare tickets (a panic) use the ring too; the result is stored.
    let t5 = match handle.submit(header, &[1], 0) {
        Ok(t) => t,
        Err(e) => panic!("submit failed: {}", e),
    };
    settle(&|| lock_recover(&handle.cancel_flags).is_empty());
    let words = match ring.pop_for_test(&mut head) {
        Some(w) => w,
        None => panic!("ring empty"),
    };
    assert_eq!(words[0], t5, "a panic is a bare ticket");
    assert!(matches!(handle.take(t5), Ok(JobResult::Panic { .. })));

    handle.close().must("close");
    // The ring outlives the handle for as long as the reader holds it.
    assert_eq!(ring.shared().capacity, 2);
    // SAFETY: the read end is still owned by this test.
    unsafe {
        libc::close(r);
    }
}

/// Stress: the whole completion path — queue, workers, ring, waiting flag
/// and wake tokens — against a reader that follows Go's `ticketReader`
/// (`next` / `waitRing` in handle.go): poll the ring, announce `waiting`,
/// re-check, park on the pipe, count owed tokens. Submitters hold one of
/// `pool_size` permits per job, returned when the reader delivers it, as
/// Go's semaphore does (I4). Every completion must arrive, and none may
/// sit unread behind a parked reader or wait in the queue beside idle
/// workers: the reader parks with a timeout, and a timeout with work
/// outstanding fails the test with the state it found.
#[test]
#[cfg_attr(miri, ignore)]
fn ring_reader_protocol_loses_and_strands_nothing_under_load() {
    use crate::header::GUSSET_FLAG_INLINE_COMPLETION;
    use std::sync::atomic::Ordering::SeqCst;
    use std::sync::Condvar;
    use std::time::{Duration, Instant};

    for &(pool, jobs, submitters) in &[(1usize, 3000u64, 2usize), (2, 6000, 4), (4, 8000, 8)] {
        let (r, w) = make_pipe();
        // SAFETY: fcntl on a descriptor this test owns.
        unsafe { libc::fcntl(r, libc::F_SETFL, libc::O_NONBLOCK) };
        let handle = match Handle::open(pool as u32, w) {
            Ok(h) => h,
            Err(e) => panic!("open failed: {}", e),
        };
        let ring = match handle.attach_ring() {
            Ok(r) => r,
            Err(e) => panic!("attach failed: {}", e),
        };
        let permits = Arc::new((Mutex::new(pool), Condvar::new()));
        let outstanding: Arc<Mutex<IdMap<u64, Instant>>> = Arc::new(Mutex::new(IdMap::default()));
        let submitted = Arc::new(AtomicU64::new(0));

        let workers: Vec<_> = (0..submitters)
            .map(|s| {
                let handle = Arc::clone(&handle);
                let permits = Arc::clone(&permits);
                let outstanding = Arc::clone(&outstanding);
                let submitted = Arc::clone(&submitted);
                thread::spawn(move || {
                    let mut i = s as u64;
                    loop {
                        let n = submitted.fetch_add(1, SeqCst);
                        if n >= jobs {
                            return;
                        }
                        {
                            let (m, cv) = &*permits;
                            let mut p = lock_recover(m);
                            while *p == 0 {
                                p = cv.wait(p).unwrap_or_else(|e| e.into_inner());
                            }
                            *p -= 1;
                        }
                        i = i
                            .wrapping_mul(6364136223846793005)
                            .wrapping_add(1442695040888963407);
                        // Mostly instant echoes, some 10 ms sleeps, some
                        // results that go through take (no inline flag).
                        let (input, inline): (&[u8], bool) = match (i >> 33) % 20 {
                            0 => (&[9, 1], true),
                            1 => (&[0, 7, 7], false),
                            _ => (&[0, 1], true),
                        };
                        let flags = GUSSET_FLAG_DIAGNOSTIC_ENGINE
                            | if inline {
                                GUSSET_FLAG_INLINE_COMPLETION
                            } else {
                                0
                            };
                        let header = CallHeader {
                            flags,
                            ..Default::default()
                        };
                        let mut out = lock_recover(&outstanding);
                        match handle.submit(header, input, 0) {
                            Ok(t) => {
                                out.insert(t, Instant::now());
                            }
                            Err(e) => panic!("submit failed: {}", e),
                        }
                    }
                })
            })
            .collect();

        // The reader, as in handle.go.
        let mut head = 0u64;
        let mut buf: Vec<u8> = Vec::new();
        let mut overflow_seen = 0u64;
        let mut tokens_owed = 0i64;
        let mut delivered = 0u64;
        let shared = ring.shared();
        let overflow_pending =
            |seen: u64| (shared.overflow.load(SeqCst).wrapping_sub(seen) as i64) > 0;
        let read_now = |buf: &mut Vec<u8>| {
            let mut tmp = [0u8; 512];
            // SAFETY: non-blocking read into a valid stack buffer.
            let n = unsafe { libc::read(r, tmp.as_mut_ptr() as *mut libc::c_void, tmp.len()) };
            if n > 0 {
                buf.extend_from_slice(&tmp[..n as usize]);
            }
        };
        let deliver = |ticket: u64, delivered: &mut u64| {
            if lock_recover(&outstanding).remove(&ticket).is_none() {
                // Taken results are stored; inline ones are not.
                panic!("completion for unknown or duplicate ticket {ticket}");
            }
            let _ = handle.take(ticket);
            *delivered += 1;
            let (m, cv) = &*permits;
            *lock_recover(m) += 1;
            cv.notify_one();
        };
        while delivered < jobs {
            if let Some(words) = ring.pop_for_test(&mut head) {
                deliver(words[0] & !INLINE_RECORD_FLAG, &mut delivered);
                continue;
            }
            // Whole records already read from the pipe.
            if buf.len() >= 8 {
                let w0 = word(&buf);
                let size = if w0 & INLINE_RECORD_FLAG == 0 {
                    8
                } else if buf.len() >= 16 {
                    16 + (word(&buf[8..]) as usize).div_ceil(8) * 8
                } else {
                    usize::MAX
                };
                if buf.len() >= size {
                    buf.drain(..size);
                    if w0 == 0 {
                        tokens_owed = (tokens_owed - 1).max(0);
                    } else {
                        overflow_seen += 1;
                        deliver(w0 & !INLINE_RECORD_FLAG, &mut delivered);
                    }
                    continue;
                }
            }
            if overflow_pending(overflow_seen) || tokens_owed > 0 {
                let before = buf.len();
                read_now(&mut buf);
                if buf.len() != before {
                    continue;
                }
            }
            // waitRing: announce, re-check, park.
            shared.waiting.store(1, SeqCst);
            let mut popped = None;
            if let Some(words) = ring.pop_for_test(&mut head) {
                popped = Some(words);
            } else if !overflow_pending(overflow_seen) {
                let mut pfd = libc::pollfd {
                    fd: r,
                    events: libc::POLLIN,
                    revents: 0,
                };
                // SAFETY: one valid pollfd.
                let rc = unsafe { libc::poll(&mut pfd, 1, 5000) };
                if rc == 0 {
                    let out = lock_recover(&outstanding);
                    let oldest = out.values().map(|t| t.elapsed()).max();
                    panic!(
                        "pool {pool}: reader parked 5 s with {} job(s) outstanding (oldest {:?}); \
                             ring head ready: {}, waiting={}, overflow={} seen={}, in_flight={}",
                        out.len(),
                        oldest,
                        ring.pop_for_test(&mut head.clone()).is_some(),
                        shared.waiting.load(SeqCst),
                        shared.overflow.load(SeqCst),
                        overflow_seen,
                        handle.in_flight(),
                    );
                }
                read_now(&mut buf);
            }
            if shared.waiting.swap(0, SeqCst) == 0 {
                tokens_owed += 1;
            }
            if let Some(words) = popped {
                deliver(words[0] & !INLINE_RECORD_FLAG, &mut delivered);
            }
            // Nothing outstanding may be older than the slowest job by much.
            if let Some(age) = lock_recover(&outstanding)
                .values()
                .map(|t| t.elapsed())
                .max()
            {
                assert!(
                    age < Duration::from_secs(5),
                    "pool {pool}: a job has been outstanding for {age:?}"
                );
            }
        }
        for s in workers {
            if s.join().is_err() {
                panic!("submitter panicked");
            }
        }
        assert!(lock_recover(&outstanding).is_empty());
        handle.close().must("close");
        // SAFETY: the read end is still owned by this test.
        unsafe {
            libc::close(r);
        }
    }
}

/// The ring's overflow counter tells the reader how many records are in
/// the pipe for it. A spill that never reached the pipe must not count.
///
/// `complete` bumped the counter after every spill attempt, including one
/// abandoned because the handle closed while the pipe was full. Go's
/// reader then saw `overflowPending()` true with nothing to read: `next`
/// found no bytes, `waitRing` returned at once on the same check, and the
/// reader spun on non-blocking reads instead of parking, until `close`
/// finally closed the descriptor after joining every worker (unbounded
/// for an engine that never checks its cancel flag).
#[test]
#[cfg_attr(miri, ignore)]
fn overflow_counts_only_spills_that_reached_the_pipe() {
    use crate::header::GUSSET_FLAG_INLINE_COMPLETION;
    use std::sync::atomic::Ordering::SeqCst;
    let (r, w) = make_pipe();
    let handle = match Handle::open(1, w) {
        Ok(h) => h,
        Err(e) => panic!("open failed: {}", e),
    };
    let ring = match handle.attach_ring() {
        Ok(r) => r,
        Err(e) => panic!("attach failed: {}", e),
    };
    let header = CallHeader {
        flags: GUSSET_FLAG_DIAGNOSTIC_ENGINE | GUSSET_FLAG_INLINE_COMPLETION,
        ..Default::default()
    };
    let settle = |h: &Handle| {
        let start = std::time::Instant::now();
        while !lock_recover(&h.cancel_flags).is_empty() {
            assert!(
                start.elapsed() < std::time::Duration::from_secs(10),
                "timed out"
            );
            std::thread::yield_now();
        }
    };
    // Fill the ring (two slots for a pool of one); nobody consumes it.
    for i in 0..2u8 {
        if let Err(e) = handle.submit(header, &[0, i], 0) {
            panic!("submit failed: {}", e);
        }
        settle(&handle);
    }
    assert_eq!(ring.shared().overflow.load(SeqCst), 0);

    // Fill the pipe so a spilled record cannot be written. The write end
    // is non-blocking (open set it), so this stops at EAGAIN.
    let junk = [0u8; 4096];
    for chunk in [4096usize, 1] {
        loop {
            // SAFETY: writing from a valid buffer to a descriptor this
            // test created; the handle has not closed it yet.
            let n = unsafe { libc::write(w, junk.as_ptr() as *const libc::c_void, chunk) };
            if n < 0 {
                break;
            }
        }
    }

    // The third completion finds the ring and the pipe full, and retries.
    if let Err(e) = handle.submit(header, &[0, 2], 0) {
        panic!("submit failed: {}", e);
    }
    std::thread::sleep(std::time::Duration::from_millis(100));
    assert_eq!(
        lock_recover(&handle.cancel_flags).len(),
        1,
        "the worker should still be retrying the spill"
    );

    // Closing abandons the spill: nothing reached the pipe.
    handle.close().must("close");
    assert_eq!(
        ring.shared().overflow.load(SeqCst),
        0,
        "an abandoned spill was counted as a record in the pipe; the reader \
             would poll for it instead of parking"
    );
    // SAFETY: the read end is still owned by this test; close() took the write end.
    unsafe {
        libc::close(r);
    }
}
