# GP-FEAT-001: call-path coordination overhead

The card's bar is Gusset under 1.5x of raw cgo on 1-10 µs jobs. This directory
holds the profiles that name the contended locks, the CallNoop allocation
question, and one interleaved A/B per candidate. The R15 crossover itself is
`../crossover/transport-darwin-arm64-go1.27.2-rust1.99.0.txt`, re-recorded on
the R9 tree (`56553d9`) and charted by `make docs`.

Host: Apple M5 Pro (6 Super + 12 Performance cores), macOS (Darwin 27.0.0),
Go 1.27.2, Rust 1.99.0, release profile.

## Where the bar stands

Medians from the crossover record (`benchstat`, n=10), names stripped of their
calibrated suffix as in `../handoff-gomaxprocs/README.md`:

| job | cgo serial | Gusset serial | ratio | cgo, 18 goroutines | Gusset, 18 goroutines | ratio |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| ~0.7 µs (`1000it`) | 702.1n ± 1% | 2.170µ ± 28% | 3.09x | 44.94n ± 6% | 2.733µ ± 7% | 60.8x |
| ~7 µs (`10000it`) | 7.288µ ± 6% | 9.043µ ± 3% | 1.24x | 495.1n ± 9% | 3.001µ ± 3% | 6.06x |

```
sed -E 's/-[0-9]+[nu]s-18/-18/' ../crossover/transport-darwin-arm64-go1.27.2-rust1.99.0.txt |
  benchstat -ignore ld -filter '.unit:sec/op' -
```

1 µs and 10 µs themselves were not recorded: `workSizes` brackets the range
with ~0.7 µs (below it) and ~7 µs (inside it). A filtered `CrossoverSerial`
arm at ~1.4k and ~14k iterations, in a file of its own, would pin the ends.

**The bar is not met.** Serial, a ~7 µs job is under it (1.24x). A ~0.7 µs job
is 3.1x serial, and under 18 goroutines both sizes are far over: the parallel
arm measures throughput, and Gusset's is capped by its coordination (one drain
reader, `handleState.mu` taken per submit, delivery and collect) at roughly
0.3-0.4 M calls/s, where raw cgo runs 18 jobs at once. A serial no-op costs
1.34 µs; three cross-thread handoffs (submit to worker, worker to drain reader,
reader to waiter) sit on that path, so a 1 µs job under 1.5x (0.5 µs of
overhead) needs a structural change, not a lock trim. See "Next".

## CallNoop allocs/op

The README's `3.000 ± 33%` came from the 2026-09-20 record, taken before result
channels were recycled (`handleState.waitChans`). Today CallNoop, SubmitWait
and CallParallel are 1 alloc/op, 1 B/op, and `make bench` + `make docs`
regenerated the table from `../darwin-arm64-go1.27.2-rust1.99.0.txt`.

`profiles/callnoop-23e925b.mem.pb.gz` (`-memprofilerate=1`, 200k calls) puts
the one allocation at `drain.go:88`, `out = make([]byte, len(inlineData))`:
the result bytes handed to the caller, which `Call` must return. It reports
12,503 objects for 200,000 calls because 1-byte allocations go through the
tiny allocator, which profiles once per 16-byte block (200,000 / 16 = 12,500).
No code change: the docs were wrong, not the code.

```
go test -run '^$' -bench 'GussetCallNoop$' -benchmem -benchtime=200000x \
  -memprofile callnoop.mem -memprofilerate=1 ./bench/
go tool pprof -sample_index=alloc_objects -list 'gusset.drainPipe' callnoop.mem
```

## Profiles: the contended locks

`BenchmarkGussetCallParallel` (pool of 8, 18 goroutines). The `23e925b`
profiles were taken at ~56% idle while other sessions compiled, so they are
good for attribution, not for latency; the `r9` ones at ~77% idle.

Go mutex profile (`-mutexprofilefraction=1`), delay at Unlock by line:

| lock site | `23e925b` | `r9` |
| --- | ---: | ---: |
| `handleState.mu`, `wait.go:253` (collect after the result arrives) | 3.55 s | 4.69 s |
| `handleState.mu`, `submit.go:246` (`semTickets` insert) | 1.22 s | 1.08 s |
| `handleState.mu`, `drain.go:218` (deliver to a waiter) | 0.69 s | 0.67 s |
| semaphore channel, `submit.go:187` | 0.47 s | 0.69 s |
| total delay | 7.06 s | 8.41 s |

`handleState.mu` is the top contended lock in Go both times. Delay totals
from separate runs are not an A/B; what moved between them is on the Rust side.

Rust, from `/usr/bin/sample` of the same benchmark, 4 s at 1 ms
(`profiles/*.native-sample.txt`). Go's pprof cannot symbolize Rust (it shows
as `<unknown>`, 29% of CPU at `23e925b`), so the native sampler is the Rust-side
profile. Each `__psynch_mutexwait` leaf is attributed to the nearest gusset or
`std::sync::Mutex::lock` frame above it in the call tree:

| Rust lock | `23e925b` | `r9` |
| --- | ---: | ---: |
| `Handle::sender` (`Mutex<Option<QueueSender>>`), R9 | 986 | gone |
| `Handle::cancel_flags` (`Mutex<IdMap<u64, Arc<AtomicBool>>>`) | not separated | 122 |
| `JobQueue::push` state mutex | 32 | 114 |
| worker loop (`spawn_workers_locked` closure) | 51 | 88 |
| `take_panic_location` global, R6 | 39 | 33 |
| all `__psynch_mutexwait` | 1116 | 374 |

At `23e925b` the sender mutex was held across the cancel-flag lock and the
queue's push lock, so the other two waited inside it; with it gone, their own
waits show.

```
go test -run '^$' -bench 'GussetCallParallel$' -benchtime=3s \
  -mutexprofile par.mutex -mutexprofilefraction=1 -cpuprofile par.cpu -o bench.test ./bench/
go tool pprof -list 'handleState\).(waitInternal|submitInput|deliver)$' bench.test par.mutex
./bench.test -test.run '^$' -test.bench 'GussetCallParallel$' -test.benchtime=8s &
sample $! 4 -mayDie -file par.sample.txt
```

## Candidates

Each A/B was recorded by `bench/record.sh` with
`RECORD_VARIANTS="base=<main @ 23e925b> cand=<this tree>"`: 10 rounds, each
round running the arm once per variant, base first. Both trees built their own
`libgusset.a` (`cargo build --release`, `go generate ./internal/ffi`).

### R9: submit sends without a sender mutex (landed, `56553d9`)

`r9-callparallel-*.txt`, recorded 87.0% -> 80.9% idle:

```
benchstat -ignore ld -col 'variant@(base cand)' -filter '.unit:sec/op' r9-callparallel-darwin-arm64-go1.27.2-rust1.99.0.txt

                      │    base     │                cand                 │
                      │   sec/op    │    sec/op     vs base               │
GussetCallParallel-18   3.479µ ± 8%   3.248µ ± 15%  -6.64% (p=0.012 n=10)
```

Min of 10: base 3325 ns, cand 2707 ns (-18.6%).

`r9-callnoop-*.txt`, the serial path it should not touch, 71.7% -> 88.9% idle:

```
                  │     base     │              cand              │
                  │    sec/op    │    sec/op     vs base          │
GussetCallNoop-18   1.454µ ± 23%   1.449µ ± 24%  ~ (p=0.897 n=10)
```

Min of 10: base 1360 ns, cand 1368 ns.

```
awk '/^variant:/{v=$2} /^BenchmarkGusset/{k=$1" "v; x=$3+0; if(!(k in m)||x<m[k])m[k]=x} END{for(k in m) print k, m[k]}' r9-*.txt
```

### Rejected: G6 and the inline-by-value result

None of these landed. Each is kept on a local branch so the code behind each
record stays readable. Variants per file, in recording order (all recorded
with the same 10-round interleaving):

| file | variants | branch |
| --- | --- | --- |
| `g6-*.txt` (76.3% -> 75.2% idle) | `base` = `05484e2`, `s0` = take id on `callResult`, `b` = deliver collects for a Call and returns its permit, `a` = Call registers its waiter under the `semTickets` hold, `c` = result channels recycled through a channel | `g6-step0-takeid`, `g6-rejected-b-a-c` |
| `g6b-*.txt` (92.4% -> 92.3% idle) | `base`, `s0`, `bp` = B' (deliver collects, the waiter returns the permit), `bpc` = B' + `c` | `g6-rejected-bprime-recycle` |
| `inline-*.txt` (79.9% -> 97.3% idle) | `base`, `inl` = an inline result carried by value on `callResult`, sliced by the waiter instead of the drain reader | `g6-rejected-inline` |

```
benchstat -ignore ld -col 'variant@(base s0 b a c)' -filter '.unit:sec/op' g6-darwin-arm64-go1.27.2-rust1.99.0.txt
benchstat -ignore ld -col 'variant@(base s0 bp bpc)' -filter '.unit:sec/op' g6b-darwin-arm64-go1.27.2-rust1.99.0.txt
benchstat -ignore ld -col 'variant@(base inl)' -filter '.unit:sec/op' inline-darwin-arm64-go1.27.2-rust1.99.0.txt
```

| candidate | CallParallel vs base | CallNoop vs base | verdict |
| --- | --- | --- | --- |
| s0, take id on `callResult` | ~ (p=0.97; p=0.80) | ~ | rejected: no gain (it deletes a map and the popped-0 bug class, but did not win) |
| B, deliver collects + returns the permit | +16.4% (p=0.015) | ~ | rejected: slower |
| A on B | +18.2% (p=0.005) | ~ | rejected: slower |
| c on A on B | +10.9% (p=0.035) | ~ | rejected: slower |
| B', the waiter returns the permit | +18.7% (p=0.000) | ~ | rejected: slower |
| c on B' | +15.0% (p=0.002); vs B' ~ (p=0.12) | ~ | rejected: slower |
| inline result by value | ~ (p=0.91) | ~ (p=0.99) | rejected: no gain |

Every round of B' sat at 3.4-4.0 µs against base's 2.6-3.8 µs, so it is a
shift, not an occasional collapse. That rules out what B and B' were built on.
G6 targeted `handleState.mu` because the mutex profile put most of its delay
at the waiter's collect (`wait.go:253`). Removing that acquisition made
throughput worse whether the drain reader or the waiter returned the permit.
Moving the inline result's allocation off the drain reader changed nothing,
which argues against the reader being the throughput limit at all. The
mutex profile's delay attribution therefore does not identify the
bottleneck. One unverified reading: the waiter's extra hold of mu acted as
backpressure, and without it more submitters reach the Rust submit path's
pthread mutexes at once (`cancel_flags`, `JobQueue::push`), where a wait goes
to the kernel. An execution trace (`go test -trace`) of CallParallel on base
and on B' would test that before any further attempt on these locks.

While building `c`, `TestReusedWaitChansUnderCloseAndDrainExit` found that
drainPipe's exit set `drainExited` only after answering waiters, so a waiter
that returns without retaking mu could report ErrClosed on a handle still
reading as open. Today every waiter retakes mu, so nothing on this branch can
reach it; the reorder is in `g6-rejected-bprime-recycle` for anyone who
builds on that path.

## Next

In profile order, each needing its own A/B:

- **Find the real limit first.** G6 (above) shows the mutex profile is the
  wrong guide here. Take an execution trace of CallParallel and see which
  goroutines or threads are runnable but waiting, and where.
- **Rust `cancel_flags`**: a per-submit insert and a per-job remove on one
  mutex. A slab sized to the queue (`pool_size * 2`) is the audit's candidate.
- **R11 / `JobQueue::push`**: now visible at 114 samples.
- The serial floor at 1 µs is not reachable by any of these: it needs fewer
  cross-thread handoffs, for example a lone waiter reading the ring itself
  instead of being handed its result by the drain reader.
