# The quiet handoff against GOMAXPROCS: before, reverted, after

`a4d4cd8` made a lone call's handoff quiet: the waiter polls its result channel
for up to 50 µs instead of parking, and the drain reader polls the ring without
yielding. It turned that on for every GOMAXPROCS above one. These files compare
three builds of the same benchmarks:

| Variant | What it is |
| --- | --- |
| `head` | `159d352`, quiet on every GOMAXPROCS above one |
| `reverted` | `159d352` with `a4d4cd8`'s Go changes reverted: `git diff a4d4cd8 a4d4cd8^ -- drain.go handle.go ticket_reader.go wait.go handoff_sched_internal_test.go \| git apply` |
| `fixed` | `4114abe`, quiet only at GOMAXPROCS >= 3 (`quietMinProcs` in `drain.go`) |

All three link the same `libgusset.a`; `a4d4cd8` changed no Rust.

Both files were recorded by `bench/record.sh` with `RECORD_VARIANTS` and
`RECORD_CPU=2,3,4,8`: 10 rounds, and each round ran the arm once in each
variant, in the order head, reverted, fixed, so drift in the machine's clock or
load lands on all three alike. Each file's header names the variants' commits,
CPU idle before and after, and that no other benchmark was running.

Host: Apple M5 Pro, 18 cores, macOS (Darwin 27.0.0), Go 1.27.2, Rust 1.99.0,
release profile. GOMAXPROCS here caps the Ps, not the CPUs: the Rust worker
always had a free core. The section on Linux below shows what changes when
it does not.

## Serial `Call` (`serial-call-*.txt`)

`BenchmarkGussetCallNoop`: one goroutine, one no-op `Call` at a time, pool of
4, the diagnostic engine. Recorded 2026-10-09T18:26Z, 69.3% → 75.8% idle.

```
benchstat -ignore ld -col 'variant@(head reverted fixed)' -filter '.unit:sec/op' serial-call-darwin-arm64-go1.27.2-rust1.99.0.txt

                 │     head     │               reverted               │                fixed                 │
                 │    sec/op    │    sec/op     vs base                │    sec/op     vs base                │
GussetCallNoop-2   1.563µ ± 39%   2.283µ ± 17%  +46.03% (p=0.004 n=10)   2.328µ ± 10%  +48.91% (p=0.004 n=10)
GussetCallNoop-3   1.573µ ± 49%   2.527µ ± 20%  +60.67% (p=0.001 n=10)   1.596µ ±  9%        ~ (p=0.684 n=10)
GussetCallNoop-4   1.631µ ± 49%   2.621µ ± 18%  +60.62% (p=0.003 n=10)   1.520µ ±  8%        ~ (p=0.516 n=10)
GussetCallNoop-8   1.770µ ± 29%   2.555µ ± 15%  +44.39% (p=0.003 n=10)   1.536µ ± 11%        ~ (p=0.353 n=10)
geomean            1.632µ         2.493µ        +52.73%                  1.716µ         +5.14%

benchstat -ignore ld -col 'variant@(reverted fixed)' -filter '.unit:sec/op' serial-call-darwin-arm64-go1.27.2-rust1.99.0.txt

                 │   reverted   │                fixed                 │
                 │    sec/op    │    sec/op     vs base                │
GussetCallNoop-2   2.283µ ± 17%   2.328µ ± 10%        ~ (p=0.971 n=10)
GussetCallNoop-3   2.527µ ± 20%   1.596µ ±  9%  -36.83% (p=0.000 n=10)
GussetCallNoop-4   2.621µ ± 18%   1.520µ ±  8%  -42.00% (p=0.000 n=10)
GussetCallNoop-8   2.555µ ± 15%   1.536µ ± 11%  -39.90% (p=0.001 n=10)
geomean            2.493µ         1.716µ        -31.16%
```

Min of 10, ns/op, from the same file:

| GOMAXPROCS | head | reverted | fixed |
| ---: | ---: | ---: | ---: |
| 2 | 1335 | 1991 | 1958 |
| 3 | 1318 | 2270 | 1329 |
| 4 | 1343 | 2188 | 1387 |
| 8 | 1306 | 2139 | 1361 |

```
awk '/^variant:/{v=$2} /^BenchmarkGussetCallNoop/{k=$1" "v; x=$3+0; if(!(k in m)||x<m[k])m[k]=x} END{for(k in m) print k, m[k]}' serial-call-*.txt | sort
```

`-ignore ld` is needed because the linker's `ld: warning: ...` lines, which
record.sh keeps with each run's output, parse as a configuration key whose value
names each variant's directory, and would split the table by variant.

## The jobs `a4d4cd8` measured (`crossover-serial-*.txt`)

`BenchmarkCrossoverSerial/Gusset` at ~0.7 µs (`1000it`) and ~7 µs (`10000it`)
jobs, the two sizes `a4d4cd8`'s message quoted. Recorded
2026-10-09T18:22Z, 84.8% → 78.5% idle. Each process calibrates its own job time
into the name (`10000it-6us`, `10000it-7us`), so the names are stripped to pair
the rows, the same normalisation `../linux-amd64-vm/README.md` uses:

```
sed -E 's/(Gusset\/[0-9]+it)-[0-9]+[nu]s/\1/' crossover-serial-darwin-arm64-go1.27.2-rust1.99.0.txt |
  benchstat -ignore ld -col 'variant@(reverted fixed)' -filter '.unit:sec/op' -

                                 │   reverted    │                fixed                 │
                                 │    sec/op     │    sec/op     vs base                │
CrossoverSerial/Gusset/1000it-2     3.947µ ± 19%   4.583µ ± 18%        ~ (p=0.149 n=10)
CrossoverSerial/Gusset/1000it-3     4.045µ ± 46%   2.613µ ± 26%  -35.41% (p=0.000 n=10)
CrossoverSerial/Gusset/1000it-4     3.894µ ± 71%   2.480µ ± 29%  -36.32% (p=0.000 n=10)
CrossoverSerial/Gusset/1000it-8     4.038µ ± 74%   2.374µ ± 36%  -41.21% (p=0.000 n=10)
CrossoverSerial/Gusset/10000it-2    12.46µ ± 49%   13.30µ ± 15%        ~ (p=0.912 n=10)
CrossoverSerial/Gusset/10000it-3   13.590µ ± 41%   9.608µ ± 32%  -29.30% (p=0.000 n=10)
CrossoverSerial/Gusset/10000it-4   13.902µ ± 13%   9.595µ ± 11%  -30.98% (p=0.000 n=10)
CrossoverSerial/Gusset/10000it-8   13.837µ ± 36%   9.575µ ±  6%  -30.80% (p=0.000 n=10)
geomean                             7.313µ         5.492µ        -24.90%
```

Min of 10, ns/op:

| GOMAXPROCS | job | head | reverted | fixed |
| ---: | ---: | ---: | ---: | ---: |
| 2 | ~0.7 µs | 1998 | 3613 | 3624 |
| 2 | ~7 µs | 9078 | 11573 | 11252 |
| 3 | ~0.7 µs | 2013 | 3630 | 2044 |
| 3 | ~7 µs | 9144 | 11235 | 8603 |
| 8 | ~0.7 µs | 2086 | 3730 | 2161 |
| 8 | ~7 µs | 9205 | 11199 | 8710 |

`tools/internal/benchfile` refuses this file: the stripped part of the name
differs between rounds, so an arm reads as uneven (`10000it-6us` 8 runs against
`10000it-7us` 112). That is the calibrated label wobbling by one microsecond
across 30 processes, not a cut-short recording, but the check is not loosened
for it; `docs/benchmarks.md` lists the file as refused. The serial-call file
passes the same checks.

## What the record shows

- At GOMAXPROCS=2, `fixed` is `reverted`: no significant difference on any of
  the three serial rows (p = 0.97, 0.15, 0.91), with the minima within 3%.
  On this host `head` is faster there, because the Rust worker has a core of
  its own. That speed is given up on purpose; the next two sections are why.
- At GOMAXPROCS 3, 4 and 8, `fixed` is `head`: no significant difference on
  any serial row (p = 0.22-0.97), and 29-42% faster than `reverted`. `a4d4cd8`'s gain is kept
  everywhere it does not hold every P.

## Other goroutines at GOMAXPROCS=2

Not in these files, because it is a latency distribution rather than a
benchmark: `TestSerialCall_LeavesAPForOtherGoroutines` (in
`handoff_sched_internal_test.go`) sleeps 200 µs in a loop beside a stream of
serial Calls at GOMAXPROCS=2 and fails if the median wake is more than 100 µs
late. Run by hand on this host, three times each:

| Build | median late | max |
| --- | ---: | ---: |
| `head` | 2.2-3.5 ms | 21-25 ms |
| `reverted` | 2.0-4.2 µs | 65-503 µs |
| `fixed` | 2.1-2.7 µs | 48 µs-2.2 ms |

A process making no Calls wakes the same sleeper 32 µs late at the median, so
`head` at GOMAXPROCS=2 is not a slow wake but a starved one. The quiet waiter and
the quiet reader each hold a P and neither passes through the scheduler, so with
two Ps a readied goroutine waits for a preemption. With three, one P is left:
an exploratory probe (not recorded) had `head` at 38-43 µs, close to the idle
figure, at GOMAXPROCS 3, 4 and 8.

## Linux with a CPU quota (exploratory, not recorded)

GOMAXPROCS on an 18-core host is not a 2-CPU container. The same probes ran in
`podman run --cpus=N` on a 9-vCPU arm64 Linux VM on this machine (Go sets
GOMAXPROCS from the quota): each container ran three 1 s serial loops (pool of
2) and then the 200 µs sleeper beside serial Calls, the builds interleaved, two
rounds at `--cpus` 2, 3, 4 and 8, then three more at 8. Run by hand, so these
are ranges over runs, not certified:

| `--cpus` | serial ns/call head / reverted / fixed | sleeper median late head / reverted / fixed |
| ---: | --- | --- |
| 2 | 3.8k-5.6k / 2.1k-2.7k / 2.1k-2.4k | 0.90-0.98 ms / 5.1-5.4 µs / 3.5-5.3 µs |
| 3-8 | 1.8k-4.5k / 1.8k-2.8k / 1.8k-3.0k | 0.76-0.91 ms / 8.2-9.4 µs / 0.41-0.91 ms |

The 3-8 row leaves out the first round at `--cpus` 4 and 8, in which all three
builds read 2.9k-9.4k and the sleeper figures scattered (head 0.21-0.25 ms,
reverted 21-42 µs, fixed 0.24-0.43 ms) while the host was loaded, and one fixed
container at `--cpus=8` that read 7.9k-72k; the three interleaved rounds that
re-ran `--cpus=8` read 1.8k-3.0k for the fixed build.

At two CPUs `head`'s serial Call is itself slower than `reverted`'s: the two
pollers and the Rust worker contend for two CPUs. That is the regression the
2026-10-09 audit saw on a loaded host. At three or more the quiet builds'
sleeper wakes up to 0.91 ms late, about what an idle process shows on that VM
(0.97 ms; its idle Ps sleep in epoll waits with millisecond timeouts). The
reverted build's 8-9 µs there comes from the reader's yields waking threads,
the cost `a4d4cd8` removed.

## Reproduce

Two extra worktrees, one at `159d352` and one at `159d352` with the revert
applied as above, each with `target/release/libgusset.a` and
`bench/seed/rs/target/release/libffibench.a` copied in (the Rust is identical):

```sh
make build && cargo build --release --manifest-path bench/seed/rs/Cargo.toml
SUF=darwin-arm64-go$(go env GOVERSION | sed 's/^go//')-rust$(rustc --version | cut -d' ' -f2).txt
RECORD_PKG=./bench/... RECORD_CPU=2,3,4,8 \
  RECORD_VARIANTS="head=$HEAD reverted=$REVERTED fixed=$PWD" \
  bench/record.sh bench/results/handoff-gomaxprocs/serial-call-$SUF 1s 10 'GussetCallNoop$'
RECORD_PKG=. RECORD_CPU=2,3,4,8 \
  RECORD_VARIANTS="head=$HEAD/bench/seed/go reverted=$REVERTED/bench/seed/go fixed=$PWD/bench/seed/go" \
  bench/record.sh bench/results/handoff-gomaxprocs/crossover-serial-$SUF 1s 10 'CrossoverSerial/Gusset/^(1000|10000)it'
go test -count=1 -run TestSerialCall_LeavesAPForOtherGoroutines -v .
```
