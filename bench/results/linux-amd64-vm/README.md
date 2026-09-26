# linux/amd64 VM: before and after, on one host

Every file here was recorded on the same machine by `bench/record.sh`: a lock,
one process per arm, CPU idle checked before and after (≥ 93% idle throughout),
and a provenance header in each file. "Before" is the original main (`9d25f87`),
built in a separate worktree. "After" is this branch at `dc9f8ad`. The same
`bench/bench_test.go` was used for both, with the `BufferLarge` input fix
(`a97e216`) applied to "before" as well, so those two rows measure the same work.

Host: 4 vCPU Intel Xeon @ 2.80 GHz (AVX-512), Linux 6.18 guest, Go 1.27.1, Rust
1.98.1 (release profile, fat LTO).

This directory sits below `bench/results/`, so `tools/benchdoc` and
`tools/benchplot` do not read it. The README's generated table and charts still
describe the darwin-arm64 host, and those numbers predate the
spin-then-park completion path below.

## Why the "before" numbers are so slow here

A no-op call cost 90 µs on this VM, against 13 µs on the darwin host. A pure-Go
pipe round trip on the same VM costs 4.5 µs. The difference is thread wake-ups.
Each call slept twice, once in a Rust worker's `recv` and once in the Go
reader's netpoll, and on a virtualized host waking a thread whose vCPU went
idle costs tens of microseconds. The fix polls briefly before parking, on both
sides (`pool::queue`, and the drain reader in `handle.go`). An idle handle polls
once and then sleeps, so it costs no CPU at rest.

## Gusset's own suite (`main-*.txt`, benchstat, n=8)

| Benchmark | before | after | Δ |
| --- | ---: | ---: | ---: |
| Call no-op | 90.3 µs | 10.6 µs | −88% |
| Call parallel | 24.5 µs | 5.9 µs | −76% |
| Submit & Wait | 88.7 µs | 10.7 µs | −88% |
| 64 KiB buffer, copy | 116.5 µs | 44.9 µs | −61% |
| 64 KiB buffer, zero-copy | 89.3 µs | 16.6 µs | −81% |
| Channel hop (pure Go floor) | 56.4 ns | 56.8 ns | ~ |

Allocations are unchanged: 2/op on `Call` and `Submit`. `B/op` is within 2 bytes,
except zero-copy at +25 B/op, which is the `*Buffer`'s `owner` and budget fields
(the owner closes a GC use-after-free).

## Later rounds: instruction counts and inline completions

Both were measured after the tables above, in a later container. Its CPU
reports 2.10 GHz rather than 2.80 GHz, so it may not be the same host. Compare
rows within one table, not across tables.

`a4e8f0b` cut instructions on the Rust submit and complete path, found with
callgrind on `crates/gusset/examples/rt_latency.rs`: 78.3M → 56.3M
instructions for 20k round trips. `Call` no-op went 10.6 → 7.7 µs and parallel
5.9 → 3.8 µs.

Inline completion records then carry results of up to 48 bytes inside the
pipe record, which removes two cgo calls and a Rust buffer per small call.
`inline-before-a4e8f0b.txt` and `inline-after.txt` hold the two arms,
interleaved four times (n=8 each). They were run by hand with `go test -bench`,
not through `bench/record.sh`, so they carry no provenance header.

| Benchmark | a4e8f0b | inline records | Δ |
| --- | ---: | ---: | ---: |
| Call no-op | 7.76 µs | 7.67 µs | ~ (p=0.80) |
| Call parallel | 3.97 µs | 3.24 µs | −18.5% |
| Submit & Wait | 8.11 µs | 7.53 µs | ~ (p=0.07) |

The serial rows are bound by thread wake-ups, which this does not change.
Allocations stay at 2/op.

## Shared-memory completion ring (`ring-*.txt`)

Measured in the same later container as the inline-record round. "Before"
is `907ecb1`, built in its own worktree, and "after" is the ring with the
adaptive reader spin. The arms alternated four times. The crossover names
carry each run's measured job time, which is stripped so the rows pair
(`sed -E 's/(it)-[0-9]+us/\1/'`).

Timing each phase of a serial call showed where the time went. The Rust
work was about 1.6 µs in all: 1 µs submit, 0.2 µs pickup, 0.4 µs run. Most
of the rest was the completion's trip back. That trip was a `write` by the
worker, a `read` by the drain reader, and about two `read`s per call that
returned `EAGAIN` while the reader polled. It is now a slot in a ring the
reader polls with atomic loads. The pipe only wakes a reader that has parked.

Gusset's suite (n=8):

| Benchmark | 907ecb1 | ring | Δ |
| --- | ---: | ---: | ---: |
| Call no-op | 7.46 µs | 3.73 µs | −50% |
| Call parallel | 3.18 µs | 2.44 µs | −23% |
| Submit & Wait | 7.45 µs | 3.73 µs | −50% |
| 64 KiB buffer, copy | 33.7 µs | 34.1 µs | ~ (p=0.96) |
| 64 KiB buffer, zero-copy | 12.4 µs | 11.3 µs | −9% |

Same work as raw cgo, Gusset arm only (n=4):

| Sweep | Work | 907ecb1 | ring | Δ |
| --- | --- | ---: | ---: | ---: |
| serial | no-op | 7.97 µs | 3.50 µs | −56% |
| serial | 1 µs | 8.92 µs | 5.42 µs | −39% |
| serial | 10 µs | 19.1 µs | 16.7 µs | −12% |
| serial | 100 µs | 124 µs | 121 µs | ~ |
| serial | 1 ms | 1.23 ms | 1.24 ms | ~ |
| parallel | no-op | 3.00 µs | 2.40 µs | −20% |
| parallel | 1 µs | 3.38 µs | 2.43 µs | −28% |
| parallel | 10 µs | 5.74 µs | 4.71 µs | −18% |
| parallel | 100 µs | 49.8 µs | 44.8 µs | −10% |
| parallel | 1 ms | 323 µs | 316 µs | −2% |

Allocations are unchanged. The zero-copy row prints 4 or 5 allocs/op
because testing rounds down. Counted exactly over 50,000 calls, it is
4.998 before and 5.000 after.

A same-binary A/B (`BenchmarkCompletionTransport` in the root package,
ring against the unexported pipe-only mode) first showed the ring 11%
slower on serial 64 KiB results. The profile pointed at the Go runtime.
The scavenger and `madvise` ran about four times as much, and `memmove`
doubled on pages it had to fault back in. `GODEBUG=madvdontneed=0` closed
most of the gap. The cause was the reader's flat 200 µs spin for a lone
job. Once polling no longer made system calls, the reader spun through
every GC pause and kept its P from the GC's idle mark workers. Adding
pacing between yields made this worse, not better. The lone-job window is
now twice a moving average of recent completion gaps, clamped to
50–200 µs. Serial, ring against pipe:

| Work | pipe | ring, flat 200 µs | ring, adaptive |
| --- | ---: | ---: | ---: |
| 64 KiB copy | 33.9 µs | 37.3 µs | 33.4 µs |
| no-op | 8.6 µs | 3.8 µs | 3.9 µs |
| ~10 µs | 19.2 µs | — | 17.0 µs |
| ~115 µs | 124 µs | 122 µs | 122 µs |

The work-queue change in the same commit was measured on its own, on top
of the ring. Pollers check an atomic length before taking the lock, and
the first 5 µs of polling does not yield. Serial no-op −2.4%, parallel
no-op −2.9%, parallel 10 µs −6.4%, and neutral elsewhere.

An idle handle still costs no measurable CPU: about 130 µs of CPU time
over one second.

## Against one blocking cgo call per request (`transport-*.txt`, median of 6)

The same integer loop on both transports (`rs_spin` and diagnostic mode 11), so
the difference is transport alone. The `×` columns are Gusset time over raw-cgo
time.

| Sweep | Work | raw cgo | Gusset before | Gusset after | after × | before × |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| serial | no-op | 0.04 µs | 83.4 µs | 10.4 µs | — | — |
| serial | 1 µs | 1.1 µs | 86.6 µs | 11.9 µs | 10.5× | 76× |
| serial | 10 µs | 11.1 µs | 95.2 µs | 22.6 µs | 2.0× | 8.6× |
| serial | 100 µs | 109 µs | 215 µs | 130 µs | 1.19× | 1.97× |
| serial | 1 ms | 1.09 ms | 1.23 ms | 1.24 ms | 1.14× | 1.13× |
| parallel | no-op | 0.01 µs | 24.0 µs | 5.5 µs | — | — |
| parallel | 1 µs | 0.4 µs | 23.4 µs | 6.6 µs | 19× | 66× |
| parallel | 10 µs | 3.5 µs | 28.0 µs | 7.9 µs | 2.2× | 7.9× |
| parallel | 100 µs | 31.3 µs | 67.1 µs | 56.5 µs | 1.80× | 2.14× |
| parallel | 1 ms | 283 µs | 313 µs | 321 µs | 1.13× | 1.11× |

For work of 10 µs and up, Gusset now costs 1.1–2.2× a raw blocking call on this
VM, against 1.1–8.6× before. The 1 ms parallel row is unchanged within noise.
Two tuning steps held it there:

- The pollers yield (`sched_yield`, `Gosched`) instead of spinning. Pure
  spinning measured +6% on that row.
- The reader's longer 200 µs poll is kept for a lone in-flight job, the serial
  pattern. Allowing it under parallel load cost +18% on that row, because the
  reader took a core the workers needed.

## Threads and memory under concurrency (`scaling-*.txt`, `threads-*.txt`)

What blocking cgo costs is threads: each in-flight call pins an OS thread.

| In-flight requests | raw cgo threads | Gusset threads | raw cgo VSZ | Gusset VSZ | Gusset time vs cgo |
| --- | ---: | ---: | ---: | ---: | ---: |
| 32 | 30 | 11 | 3326 MiB | 2246 MiB | +2% |
| 128 | 47 | 11 | 3590 MiB | 2246 MiB | +5% |
| 512 | 47 | 11 | 3590 MiB | 2246 MiB | +6% |
| 2048 | 47 | 11 | 3590 MiB | 2246 MiB | +7% |

The thread count is flat at the pool size whatever the concurrency; that is I4.
Raw cgo's count is capped here by `GOMAXPROCS=4` plus Go's thread reuse. On the
18-core darwin host in `bench/results/crossover/` it grows much further. The
cost of the bound is 2–7% wall time at high concurrency on 4 vCPUs, the same
as before this branch (`scaling-gusset-before.txt`: 99.8 / 382 / 1520 / 6071
ms against 98.7 / 381 / 1516 / 6050 ms after).

## cgo floor on this host (`seed-micro.txt`, median of 6)

| Benchmark | ns/op |
| --- | ---: |
| cgo into a Rust no-op | 39.4 |
| cgo into an inline C no-op | 36.7 |
| cgo into a Rust no-op, `RunParallel` | 11.3 (wall time per op across 4 Ps) |
| Batch of 16 / 256 / 4096 items per call | 44.6 / 147.7 / 1860 (per call) |
| Buffered channel send | 56.0 |
| Unbuffered request/response | 422 |

## Go SIMD against a Gusset call (`simd.txt`, `GOEXPERIMENT=simd`, median of 6)

The sum of squares over bytes. The Rust side is diagnostic mode 10, which checks
cancellation once per 4 KiB chunk so that LLVM vectorizes the loop.

| Input | Go scalar | Go SIMD | Gusset → Rust |
| --- | ---: | ---: | ---: |
| 256 B | 0.2 µs | 0.1 µs | 10.5 µs |
| 4 KB | 2.7 µs | 0.9 µs | 11.6 µs |
| 64 KiB | 42.3 µs | 13.9 µs | 20.1 µs |
| 1 MiB | 725 µs | 242 µs | 151 µs |

Go SIMD wins below roughly 100 KB on this VM. Above that, the vectorized Rust
kernel (about 7 GB/s) pays for the round trip.

## Go SIMD against the multiversioned kernel (`simd-ring-multiversion.txt`)

Recorded after the completion ring, in the later container, with diagnostic
mode 10 dispatching at run time to an AVX-512BW build of its loop
(`pool::sys::sum_squares_chunk`). Median of 6:

| Input | Go scalar | Go SIMD | Gusset → Rust |
| --- | ---: | ---: | ---: |
| 256 B | 0.13 µs | 0.03 µs | 4.5 µs |
| 4 KB | 2.8 µs | 0.29 µs | 6.6 µs |
| 64 KiB | 43 µs | 4.4 µs | 7.9 µs |
| 1 MiB | 756 µs | 69 µs | 46 µs |

Go SIMD is about 3.4× faster here than in the table above, so the host
differs in more than clock speed. For the Rust kernel at 1 MiB, the same
build measured 148 µs compiled for baseline x86-64 (SSE2), 57 µs with
`-C target-cpu=native`, and 46 µs multiversioned. The native build appears to
prefer 256-bit vectors on this CPU.

## Reproduce

```sh
make build && cargo build --release --manifest-path bench/seed/rs/Cargo.toml
RECORD_DIR=. RECORD_PKG=./bench bench/record.sh OUT.txt 1s 8 'GussetCallNoop$' …
bench/record.sh OUT.txt 1s 6 CrossoverSerial/RawCgo CrossoverSerial/Gusset …
GOEXPERIMENT=simd go test ./bench -run '^$' -bench SumSquares -count=6
go test -run '^$' -bench CompletionTransport -count=6 .   # ring vs pipe, one binary
```
