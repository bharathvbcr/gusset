//! What the process holds, read from `/proc/self`: descriptors, threads and
//! mappings, for the tests that prove handles give them all back (I4, I5).
//!
//! `crates/gusset/tests/resource_hygiene.rs` and the pool unit test
//! `respawn_and_failed_open_release_threads_and_mappings` each carried a copy
//! of these readers and their own baseline. Linux only.

use std::time::{Duration, Instant};

/// How many entries a `/proc` directory lists.
pub fn count_dir(path: &str) -> usize {
    match std::fs::read_dir(path) {
        Ok(d) => d.count(),
        Err(e) => panic!("read {path}: {e}"),
    }
}

/// The lines of `/proc/self/maps`.
pub fn maps() -> Vec<String> {
    match std::fs::read_to_string("/proc/self/maps") {
        Ok(s) => s.lines().map(str::to_owned).collect(),
        Err(e) => panic!("read /proc/self/maps: {e}"),
    }
}

/// Bytes as well as lines. The kernel merges an anonymous mapping into a
/// neighbour with the same protection, so a leak shaped like its neighbour
/// adds no line at all: one leaked read-only page per lifetime passed the line
/// count alone. The brk heap is malloc's, grows and trims with ordinary
/// allocation, and is not a handle's mapping.
pub fn mapped_bytes(maps: &[String]) -> usize {
    maps.iter()
        .filter(|line| !line.ends_with("[heap]"))
        .map(|line| {
            let range = line.split(' ').next().unwrap_or("");
            let (lo, hi) = range.split_once('-').unwrap_or(("0", "0"));
            match (usize::from_str_radix(lo, 16), usize::from_str_radix(hi, 16)) {
                (Ok(lo), Ok(hi)) if hi >= lo => hi - lo,
                _ => panic!("unparsable /proc/self/maps line: {line}"),
            }
        })
        .sum()
}

/// Open descriptors, threads, `/proc/self/maps` lines and the bytes they map.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Usage {
    pub fds: usize,
    pub threads: usize,
    pub maps: usize,
    pub mapped_bytes: usize,
}

impl Usage {
    /// Descriptors and threads come back exactly. Mappings may end below the
    /// baseline, never above: glibc evicting a cached thread stack shrinks the
    /// address space without anything leaking.
    pub fn leaked_since(self, base: Usage) -> bool {
        self.fds != base.fds
            || self.threads != base.threads
            || self.maps > base.maps
            || self.mapped_bytes > base.mapped_bytes
    }
}

/// One reading of the process's descriptors, threads and mappings.
pub fn usage() -> Usage {
    let maps = maps();
    Usage {
        fds: count_dir("/proc/self/fd"),
        threads: count_dir("/proc/self/task"),
        maps: maps.len(),
        mapped_bytes: mapped_bytes(&maps),
    }
}

/// How often [`baseline`] and [`settled`] re-read, and for how long.
const POLL: Duration = Duration::from_millis(5);
const BASELINE_WAIT: Duration = Duration::from_secs(5);
const SETTLE_POLLS: usize = 200;

/// The usage to compare against, read once `/proc/self/task` lists exactly
/// `idle_threads`: the count the test recorded before it created any handle.
///
/// A thread is joined before the kernel stops listing it, so a reading taken
/// just after a warm-up can count a worker or `gusset-close` joiner that has
/// already been joined. A baseline that waits only for its own first reading
/// to repeat locks that thread in; every later reading is one lower and the
/// test reports a leak that is not there (CI run 37944107330: threads 3 then
/// 2, descriptors and mappings exact). The same too-high baseline would hide
/// a real one-thread leak. Waiting for the idle count removes both.
///
/// Panics if the count is not reached within five seconds, rather than
/// measuring against a baseline that is not the idle process.
pub fn baseline(idle_threads: usize) -> Usage {
    let started = Instant::now();
    loop {
        let now = usage();
        if now.threads == idle_threads {
            return now;
        }
        if started.elapsed() >= BASELINE_WAIT {
            panic!(
                "no baseline: /proc/self/task still lists {} threads, not the \
                 {idle_threads} recorded before any handle existed, after {:?}",
                now.threads,
                started.elapsed()
            );
        }
        std::thread::sleep(POLL);
    }
}

/// A reading taken after giving thread exit a moment to reach
/// `/proc/self/task`, for comparing against `base`. Returns the last reading
/// even if it still differs, so the caller's assertion reports the numbers.
pub fn settled(base: Usage) -> Usage {
    let mut now = usage();
    for _ in 0..SETTLE_POLLS {
        if !now.leaked_since(base) {
            break;
        }
        std::thread::sleep(POLL);
        now = usage();
    }
    now
}
