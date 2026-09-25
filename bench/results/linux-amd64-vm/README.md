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

## Reproduce

```sh
make build && cargo build --release --manifest-path bench/seed/rs/Cargo.toml
RECORD_DIR=. RECORD_PKG=./bench bench/record.sh OUT.txt 1s 8 'GussetCallNoop$' …
bench/record.sh OUT.txt 1s 6 CrossoverSerial/RawCgo CrossoverSerial/Gusset …
GOEXPERIMENT=simd go test ./bench -run '^$' -bench SumSquares -count=6
```
