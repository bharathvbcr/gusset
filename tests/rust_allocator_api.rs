//! The stable `Allocator` trait (Rust 1.100+): `BufferAlloc`, `Counting` as a
//! per-collection allocator, and zero-copy adoption of engine output.
//!
//! Separate binary with no `#[global_allocator]`, so the manual recorders are
//! live and every byte `BufferAlloc` hands out must be counted by hand exactly
//! once. One `#[test]` per concern would run in parallel and read each other's
//! deltas off the process-global counters, so the checks run in sequence inside
//! a single test.
//!
//! On a toolchain without the stable trait the whole surface is compiled out;
//! the fallback test then only checks that a CI lane which requires the new path
//! (`GUSSET_EXPECT_ALLOCATOR_API=1`) cannot silently fall back.

#![allow(unsafe_code)]

#[cfg(not(gusset_allocator_api))]
#[test]
fn allocator_api_is_absent_only_where_it_is_not_expected() {
    const { assert!(!gusset::ALLOCATOR_API) };
    assert_ne!(
        std::env::var("GUSSET_EXPECT_ALLOCATOR_API").as_deref(),
        Ok("1"),
        "this lane requires the stable Allocator trait, but the build probe fell back"
    );
}

#[cfg(gusset_allocator_api)]
mod on {
    use gusset::header::CallHeader;
    use gusset::pool::{register_engine, Handle, JobOutput, JobResult};
    use gusset::{get_alloc_stats, BufferAlloc, Counting, BUFFER_ALIGN};
    use std::alloc::{Allocator, Global, Layout, System};
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::Arc;

    /// `expect` without tripping the crate's R3 clippy ban on it.
    trait Must<T> {
        fn must(self, what: &str) -> T;
    }
    impl<T, E: std::fmt::Debug> Must<T> for Result<T, E> {
        fn must(self, what: &str) -> T {
            match self {
                Ok(v) => v,
                Err(e) => panic!("{what}: {e:?}"),
            }
        }
    }

    fn live() -> usize {
        get_alloc_stats().live_bytes
    }

    fn make_pipe() -> (i32, i32) {
        let mut fds = [0i32; 2];
        let rc = unsafe { libc::pipe(fds.as_mut_ptr()) };
        assert_eq!(rc, 0, "pipe() failed");
        (fds[0], fds[1])
    }

    fn read_ticket(fd: i32) -> u64 {
        let mut buf = [0u8; 8];
        let mut got = 0usize;
        while got < buf.len() {
            let n = unsafe {
                libc::read(
                    fd,
                    buf.as_mut_ptr().add(got) as *mut libc::c_void,
                    buf.len() - got,
                )
            };
            assert!(n > 0, "completion pipe read failed");
            got += n as usize;
        }
        u64::from_ne_bytes(buf)
    }

    fn pattern(len: usize, seed: u8) -> impl Iterator<Item = u8> {
        (0..len).map(move |i| (i as u8) ^ seed)
    }

    const OP_ALLOCATED: u32 = 0xA110;
    const OP_EMPTY_WITH_CAPACITY: u32 = 0xA111;

    /// Address of the last engine-built output, to prove Go receives that pointer.
    static LAST_PTR: AtomicUsize = AtomicUsize::new(0);

    fn register_engines() {
        // input: [len u32 LE][seed]
        register_engine(OP_ALLOCATED, |_ctx, input: &[u8]| {
            let len = u32::from_le_bytes([input[0], input[1], input[2], input[3]]) as usize;
            let seed = input[4];
            let mut out: Vec<u8, BufferAlloc> = Vec::new_in(BufferAlloc);
            // Grow in odd steps so realloc paths run, then leave spare capacity.
            let mut i = 0;
            while i < len {
                let n = (len - i).min(3 * 1024 + 7);
                out.try_reserve(n).map_err(|e| e.to_string())?;
                out.extend(pattern(len, seed).skip(i).take(n));
                i += n;
            }
            LAST_PTR.store(out.as_ptr() as usize, Ordering::SeqCst);
            Ok(JobOutput::from(out))
        });
        register_engine(OP_EMPTY_WITH_CAPACITY, |_ctx, _input: &[u8]| {
            let out: Vec<u8, BufferAlloc> = Vec::with_capacity_in(1 << 20, BufferAlloc);
            Ok(JobOutput::from(out))
        });
    }

    fn header(opcode: u32) -> CallHeader {
        CallHeader {
            reserved: opcode,
            ..Default::default()
        }
    }

    fn input(len: usize, seed: u8) -> Vec<u8> {
        let mut v = (len as u32).to_le_bytes().to_vec();
        v.push(seed);
        v
    }

    fn run(h: &Handle, fd: i32, opcode: u32, inp: &[u8]) -> JobResult {
        let t = h.submit(header(opcode), inp, 0).must("submit");
        assert_eq!(read_ticket(fd), t);
        h.take(t).must("take")
    }

    fn buffer_alloc_contract() {
        let base = live();

        // Alignment is at least BUFFER_ALIGN whatever was asked for, and a
        // stricter request is honoured.
        for &(size, align) in &[
            (1usize, 1usize),
            (7, 8),
            (4096, 16),
            (100, 128),
            (1 << 20, 4096),
        ] {
            let layout = Layout::from_size_align(size, align).must("unwrap");
            let block = BufferAlloc.allocate(layout).must("allocate");
            let p = block.cast::<u8>().as_ptr() as usize;
            assert!(block.len() >= size);
            assert_eq!(p % BUFFER_ALIGN.max(align), 0, "{size}/{align} misaligned");
            assert_eq!(live(), base + size, "{size} bytes counted once");
            unsafe { BufferAlloc.deallocate(block.cast(), layout) };
            assert_eq!(live(), base, "{size} bytes released");
        }

        // Zero-size requests never touch the counters.
        let z = Layout::from_size_align(0, 1).must("unwrap");
        let block = BufferAlloc.allocate(z).must("zero-size allocate");
        assert_eq!(live(), base);
        unsafe { BufferAlloc.deallocate(block.cast(), z) };
        assert_eq!(live(), base);

        // Zeroed memory is zeroed.
        let l = Layout::from_size_align(8192, 1).must("unwrap");
        let block = BufferAlloc.allocate_zeroed(l).must("zeroed");
        let bytes = unsafe { std::slice::from_raw_parts(block.cast::<u8>().as_ptr(), 8192) };
        assert!(bytes.iter().all(|&b| b == 0));
        unsafe { BufferAlloc.deallocate(block.cast(), l) };

        // Grow / shrink keep the live total equal to the current capacity and
        // keep contents and alignment.
        let mut v: Vec<u8, BufferAlloc> = Vec::new_in(BufferAlloc);
        for round in 0..64usize {
            v.extend(pattern(997, round as u8));
            assert_eq!(live(), base + v.capacity());
            assert_eq!(v.as_ptr() as usize % BUFFER_ALIGN, 0);
        }
        v.truncate(10);
        v.shrink_to_fit();
        assert_eq!(v.capacity(), 10);
        assert_eq!(live(), base + 10);
        assert_eq!(v.as_ptr() as usize % BUFFER_ALIGN, 0);
        assert!(v.iter().copied().eq(pattern(10, 0)));
        drop(v);
        assert_eq!(live(), base, "vector fully released");

        // A request past isize::MAX is an error, never a wrapped layout.
        let mut v: Vec<u8, BufferAlloc> = Vec::new_in(BufferAlloc);
        assert!(v.try_reserve(usize::MAX - 8).is_err());
        assert_eq!(live(), base);
    }

    fn counting_as_local_allocator() {
        let base = get_alloc_stats();

        // System bypasses the global allocator: Counting must count it.
        let mut v: Vec<u64, Counting<System>> = Vec::new_in(Counting::new(System));
        v.extend(0..10_000u64);
        assert_eq!(live(), base.live_bytes + v.capacity() * 8);
        assert!(get_alloc_stats().alloc_count > base.alloc_count);
        v.shrink_to_fit();
        assert_eq!(live(), base.live_bytes + 80_000);
        drop(v);
        assert_eq!(live(), base.live_bytes);

        // No Counting global allocator here, so Global is not counted elsewhere
        // and Counting<Global> must count it.
        let b = Box::new_in([7u8; 4096], Counting::new(Global));
        assert_eq!(live(), base.live_bytes + 4096);
        drop(b);
        assert_eq!(live(), base.live_bytes);

        // BufferAlloc counts itself; wrapping it must not count again.
        let mut v: Vec<u8, Counting<BufferAlloc>> = Vec::new_in(Counting::new(BufferAlloc));
        v.resize(10_000, 1);
        v.shrink_to_fit();
        assert_eq!(
            live(),
            base.live_bytes + 10_000,
            "Counting<BufferAlloc> counted the same bytes twice"
        );
        drop(v);
        assert_eq!(live(), base.live_bytes);
    }

    fn zero_copy_adoption(h: &Handle, fd: i32) {
        let base = live();

        for &(len, seed) in &[
            (1usize, 3u8),
            (4096, 5),
            (4097, 9),
            (1 << 20, 0x5a),
            (3 << 20, 1),
        ] {
            let res = run(h, fd, OP_ALLOCATED, &input(len, seed));
            let id = match res {
                JobResult::Buffer(id) => id,
                other => panic!("len {len}: expected an adopted buffer, got {other:?}"),
            };
            let (ptr, got_len) = h.buf_get(id).must("buf_get");
            assert_eq!(got_len, len);
            assert_eq!(
                ptr as usize,
                LAST_PTR.load(Ordering::SeqCst),
                "len {len}: the buffer must be the engine's allocation, not a copy"
            );
            assert_eq!(ptr as usize % BUFFER_ALIGN, 0);
            let bytes = unsafe { std::slice::from_raw_parts(ptr, got_len) };
            assert!(
                bytes.iter().copied().eq(pattern(len, seed)),
                "len {len}: content"
            );
            h.buf_free(id).must("free");
            assert_eq!(live(), base, "len {len}: adopted buffer fully released");
        }

        // Empty output with capacity: an empty result, and the capacity freed.
        match run(h, fd, OP_EMPTY_WITH_CAPACITY, &[]) {
            JobResult::Ok(v) => assert!(v.is_empty()),
            other => panic!("expected empty Ok, got {other:?}"),
        }
        assert_eq!(live(), base, "empty output's capacity released");

        // Diagnostic mode 16 goes through the same adoption.
        let mut diag = vec![gusset::pool::DIAG_MODE_ALLOCATED];
        diag.extend(input(200_000, 0x33));
        let hdr = CallHeader {
            flags: gusset::header::GUSSET_FLAG_DIAGNOSTIC_ENGINE,
            ..Default::default()
        };
        let t = h.submit(hdr, &diag, 0).must("submit");
        assert_eq!(read_ticket(fd), t);
        match h.take(t).must("take") {
            JobResult::Buffer(id) => {
                let (ptr, n) = h.buf_get(id).must("buf_get");
                let bytes = unsafe { std::slice::from_raw_parts(ptr, n) };
                assert!(bytes.iter().copied().eq(pattern(200_000, 0x33)));
                h.buf_free(id).must("free");
            }
            other => panic!("mode 16: expected adopted buffer, got {other:?}"),
        }
        assert_eq!(live(), base);
    }

    /// Many threads submitting allocator-backed jobs concurrently while other
    /// threads churn BufferAlloc and Counting<System> vectors directly. Every
    /// result is checked byte for byte and every byte is accounted for at the end.
    fn stress(h: &Arc<Handle>, fd: i32) {
        let base = live();
        const SUBMITTERS: usize = 8;
        const PER: usize = 64;

        // One reader drains completions and routes them by ticket.
        let (tx, rx) = std::sync::mpsc::channel::<u64>();
        let reader = std::thread::spawn(move || {
            for _ in 0..SUBMITTERS * PER {
                if tx.send(read_ticket(fd)).is_err() {
                    break;
                }
            }
        });

        let done: Arc<std::sync::Mutex<std::collections::HashSet<u64>>> = Default::default();
        let mut threads = Vec::new();
        for s in 0..SUBMITTERS {
            let h = Arc::clone(h);
            threads.push(std::thread::spawn(move || {
                let mut expect = Vec::new();
                for i in 0..PER {
                    let len = 1 + ((s * 7919 + i * 104_729) % (256 * 1024));
                    let seed = (s * PER + i) as u8;
                    // The queue is bounded by the pool size (I4); retry a full
                    // queue the way the Go semaphore would have parked.
                    let t = loop {
                        match h.submit(header(OP_ALLOCATED), &input(len, seed), 0) {
                            Ok(t) => break t,
                            Err(e) if e.contains("full") => std::thread::yield_now(),
                            Err(e) => panic!("submit: {e}"),
                        }
                    };
                    expect.push((t, len, seed));
                }
                expect
            }));
        }
        let churn: Vec<_> = (0..4)
            .map(|k| {
                std::thread::spawn(move || {
                    for r in 0..2000usize {
                        let mut a: Vec<u8, BufferAlloc> = Vec::new_in(BufferAlloc);
                        a.extend(pattern(1 + (r * 31 + k) % 9000, k as u8));
                        a.truncate(a.len() / 2);
                        a.shrink_to_fit();
                        let mut b: Vec<u32, Counting<System>> = Vec::new_in(Counting::new(System));
                        b.extend(0..(r as u32 % 700));
                        assert!(a.iter().copied().eq(pattern(a.len(), k as u8)));
                    }
                })
            })
            .collect();

        let mut all = Vec::new();
        for t in threads {
            all.extend(t.join().must("submitter"));
        }
        for c in churn {
            c.join().must("churn");
        }
        let mut arrived = std::collections::HashSet::new();
        while arrived.len() < all.len() {
            arrived.insert(rx.recv().must("completion"));
        }
        reader.join().must("reader");

        for (t, len, seed) in all {
            let id = match h.take(t).must("take") {
                JobResult::Buffer(id) => id,
                other => panic!("ticket {t}: {other:?}"),
            };
            assert!(
                done.lock().must("unwrap").insert(id),
                "buffer id {id} issued twice"
            );
            let (ptr, n) = h.buf_get(id).must("buf_get");
            assert_eq!(n, len);
            let bytes = unsafe { std::slice::from_raw_parts(ptr, n) };
            assert!(
                bytes.iter().copied().eq(pattern(len, seed)),
                "ticket {t} corrupted"
            );
            h.buf_free(id).must("free");
        }
        assert_eq!(live(), base, "stress leaked or double-counted buffer bytes");
    }

    /// Outputs finishing after close are refused and freed, not leaked.
    fn adoption_after_close() {
        let base = live();
        let (r, w) = make_pipe();
        let h = Handle::open(2, w).must("open");
        for _ in 0..4 {
            h.submit(header(OP_ALLOCATED), &input(64 * 1024, 1), 0)
                .must("submit");
        }
        h.close();
        drop(h);
        unsafe { libc::close(r) };
        assert_eq!(live(), base, "outputs of a closed handle must be freed");
    }

    #[test]
    fn allocator_api_end_to_end() {
        const { assert!(gusset::ALLOCATOR_API) };
        register_engines();
        buffer_alloc_contract();
        counting_as_local_allocator();

        let (r, w) = make_pipe();
        let h = Handle::open(4, w).must("open");
        zero_copy_adoption(&h, r);
        stress(&h, r);
        h.close();
        drop(h);
        unsafe { libc::close(r) };

        adoption_after_close();
    }
}
