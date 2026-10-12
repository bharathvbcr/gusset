# The bystander's wake tail, before and after `4114abe` (exploratory, not recorded)

`TestSerialCall_LeavesAPForOtherGoroutines` fails when the median wake of a
goroutine sleeping 200 µs beside serial Calls at GOMAXPROCS=2 is more than
100 µs late. The 2026-10-10 verification of `gs-perf-quiet-handoff-gomaxprocs2`
found it passing against the pre-fix gate (`quietAt` returning `procs > 1`) in
5 of 16 runs on a host at load 14-18. The proposal was to keep the median check
and add a tail statistic beside it, for example the share of wakes later than
one nap, with its threshold calibrated on a 2-4 vCPU Linux runner first.

These runs are that calibration. They find no tail threshold that the fixed
build stays under and the pre-fix build exceeds, so no threshold was chosen
and the test is unchanged.

## Method

`probe.patch` adds one `t.Logf` to the test: p25/p50/p75/p90/p95/p99, max, and
the counts of wakes later than 200 µs and later than 1 ms. It also raises the
wake target from 300 to 1000. Two test binaries were built from `984209b` with
the patch: `fixed`, and `prefix` with `quietAt` returning `procs > 1`.
`interleave.sh` (darwin) and `run.sh` (Linux) run each binary once per round, in
a separate process with `-test.count=1`, and log the load average every round.
`summ.awk` reduces a log to the ranges below. The two darwin logs and
`linux-cpus2-shared-vm.txt` were taken before the wake target was raised, so
those runs have 300 wakes. The others have 1000, or fewer for a pre-fix run
that hit the 2 s deadline. Shares are of the wakes the run actually took.

- darwin: Apple M5 Pro, 18 cores, Go 1.27.2, Rust 1.99.0. Load came from
  `load.sh`, which runs N `yes` loops.
- Linux: `localhost/gusset-stress` (Debian bookworm arm64, Go 1.27.1,
  Rust 1.99.0) in the 9-vCPU podman machine on that host. `build.sh` builds
  both binaries inside the container, plain and `-race`. Variants ran plain,
  under `-race`, and with `GOGC=1`, the two steps of `matrix.yml`'s `race` job.
  Rootless podman cannot delegate the cpuset controller (`--cpuset-cpus`
  fails), so the pinned runs used `taskset` inside the container.

## Results

Failures are of the existing median check. The two right-hand columns give
the range over runs of the share of wakes later than 200 µs and later than
1 ms.

| Log | Setting | Build | Median fails | Median late | > 200 µs | > 1 ms |
| --- | --- | --- | ---: | --- | --- | --- |
| `darwin-load17-burners14.txt` | host load 17.4-18.0, 14 `yes` | fixed | 0/16 | 1.6-36 µs | 1.0-18.7% | 0-3.0% |
| | | prefix | 3/16 | 31-145 µs | 5.0-47.3% | 1.7-31.7% |
| `darwin-load26-burners10.txt` | host load 26-28, 10 `yes` plus other sessions' builds | fixed | 0/12 | 2.0-29 µs | 0-7.7% | 0-1.3% |
| | | prefix | 6/12 | 42 µs-2.0 ms | 15.0-72.3% | 7.0-58.3% |
| `linux-pinned2.txt` | `taskset -c 0,1`, VM load < 1.6, host load ~10 | fixed | 0/15 | 0.4-0.7 µs | 10.0-17.2% | 8.1-12.0% |
| | | fixed `-race` | 0/15 | 2.2-3.1 µs | 10.6-21.4% | 8.4-18.7% |
| | | fixed `GOGC=1` | 0/15 | 0.5-0.8 µs | 9.4-15.7% | 5.6-11.2% |
| | | prefix | **0/15** | 11-42 µs | 3.5-7.3% | 1.9-3.9% |
| | | prefix `-race` | **0/15** | 28-39 µs | 3.2-9.3% | 1.2-4.9% |
| | | prefix `GOGC=1` | **0/15** | 9.8-37 µs | 3.7-6.4% | 2.0-4.5% |
| `linux-pinned4.txt` | `taskset -c 0-3`, host load 12-14 | fixed | 0/10 | 1.2-10 µs | 0.2-12.9% | 0.2-3.2% |
| | | fixed `-race` | 0/10 | 3.8-8.8 µs | 0-7.3% | 0-2.1% |
| | | fixed `GOGC=1` | 0/10 | 1.2-17 µs | 0.3-17.6% | 0.1-3.7% |
| | | prefix | 10/10 | 0.33-6.8 ms | 59.5-88.6% | 27.8-80.4% |
| | | prefix `-race` | 10/10 | 0.11-0.64 ms | 38.9-73.8% | 8.5-40.4% |
| | | prefix `GOGC=1` | 10/10 | 0.24-5.7 ms | 53.3-86.7% | 20.0-75.0% |
| `linux-cpus2.txt` | `--cpus=2` quota, host load 13-18 | fixed | 0/10 | 2.2-61 µs | 0.5-29.0% | 0.2-8.8% |
| | | fixed `-race` | 0/10 | 4.8-31 µs | 0.2-23.1% | 0.2-6.7% |
| | | fixed `GOGC=1` | 0/10 | 3.1-6.0 µs | 0.3-7.0% | 0.2-1.3% |
| | | prefix | 10/10 | 0.17-1.2 ms | 46.8-96.8% | 14.6-73.3% |
| | | prefix `-race` | 9/10 | 0.10-0.97 ms | 29.6-79.9% | 1.7-48.0% |
| | | prefix `GOGC=1` | 10/10 | 0.16-1.2 ms | 44.6-93.3% | 10.6-71.4% |
| `linux-cpus2-shared-vm.txt` | `--cpus=2`, host load 55-59, another session's container in the same VM (VM load ~7) | fixed | **2/10** | 23-250 µs | 26.0-55.0% | 10.3-20.7% |
| | | fixed `-race` | **4/10** | 4.3-161 µs | 15.0-45.3% | 4.0-17.7% |
| | | fixed `GOGC=1` | **2/10** | 12-166 µs | 9.0-46.0% | 3.0-23.7% |
| | | prefix | 10/10 | 106-310 µs | 39.7-59.3% | 11.0-22.0% |
| | | prefix `-race` | 10/10 | 140-265 µs | 42.0-54.0% | 14.0-25.3% |
| | | prefix `GOGC=1` | 6/10 | 75-220 µs | 26.7-53.7% | 6.7-20.3% |
| `linux-pinned2-baseline.txt` | `taskset -c 0,1`, VM load < 0.6, host load 8-9 | no Calls | - | 0.94-1.08 ms | 99.4-100% | 30.3-72.7% |
| | | fixed | 0/10 | 0.4-0.8 µs | 9.0-13.4% | 7.4-10.3% |
| | | prefix | 0/10 | 9.0-26 µs | 3.2-6.6% | 2.0-5.4% |

The `no Calls` rows come from `noload_probe_internal_test.go.txt`. It is the
same sleeper with a handle open and no goroutine calling it, copied into the
package by `build-baseline.sh` and run by `run-baseline.sh`. On those two CPUs
the baseline's p99 was 1.34-1.40 ms and its max 1.4-3.7 ms. The fixed build's
p99 was 3.4-3.7 ms and its max 3.8-9.2 ms. The pre-fix build's p99 was
2.8-3.2 ms.

### On GitHub runners

Every `matrix.yml` run since the fix landed logs the test's line under `-v`.
Runs 38012157242 (`23e925b`) and 38079151687 (`984209b`) hold 20 runs of the
fixed build. All 20 pass:

| Job | Runs | Median late | Max |
| --- | ---: | --- | --- |
| Race & Aggressive GC (`-race`, then `GOGC=1 -count=5`) | 12 | 1.2-6.7 µs | 67 µs-2.9 ms |
| Unit & Pitfalls, `ubuntu-latest` | 2 | 1.5-2.2 µs | 0.18-2.6 ms |
| Unit & Pitfalls, `ubuntu-24.04-arm` | 2 | 1.7-2.0 µs | 2.3-2.7 ms |
| Unit & Pitfalls, `macos-latest` | 2 | 11.4-13.2 µs | 1.4-1.7 ms |

Read with `gh run view <id> --log | rg 'wakes beside serial Calls'`.
`nightly.yml` does not run the test.

## What it shows

- **No tail threshold separates the builds on Linux.** A safe bound sits at
  least 3x above the fixed build's worst share and at most half the pre-fix
  build's best. Wakes later than 200 µs under a 2-CPU quota give at least 87%
  against at most 14.8%. At four pinned CPUs they give at least 53% against at
  most 19.5%. Wakes later than 1 ms fail the same way (at least 11.1% against at
  most 4.3% at four CPUs). Under a 2-CPU quota the fixed build's own tail
  reached 29% of wakes later than 200 µs.
- **On two pinned CPUs the ordering inverts.** The pre-fix build passes the
  median check in 55 of 55 runs, and its tail is lighter than the fixed
  build's. With the Rust worker sharing two CPUs with the two pollers, the
  kernel deschedules a poller often enough that the waiter overruns its 50 µs
  spin and parks, and the parked P runs the timer [inferred]. The fixed build's
  tail there sits at 3.5-3.8 ms p99 in every run. That is consistent with
  CFS timeslices between the Rust worker, the yielding reader and the
  sleeper's M [inferred, not instrumented]. So a 2-vCPU runner cannot check
  this regression through the bystander's lateness at all.
- **Against a process making no Calls, the fixed build's tail there is
  heavier only at the far end.** On the same two CPUs the no-Calls sleeper
  woke about 1 ms late at the median, because idle Ps sleep in epoll waits
  with millisecond timeouts. That is 99-100% of its wakes later than 200 µs and
  30-73% later than 1 ms. The fixed build is far earlier for most wakes: 7-10%
  later than 1 ms. Its p99 of 3.4-3.7 ms is 2.5x the baseline's 1.4 ms,
  though, so roughly the last 1% of wakes is later than an idle process's.
- **The current median bound holds on GitHub's runners.** The 20 CI runs above
  sit at 1.2-13 µs against the 100 µs bound, the race job included.
- **On darwin the separation depends on what the load is.** At 14 constant
  `yes` loops (load ~17.5) the fixed build had up to 18.7% of wakes later than
  200 µs, and the pre-fix build had as little as 5.0%. The brief reports that its load of
  14-18, from other sessions' work, left the fixed build at 0-4 wakes in 300
  (not re-run here).
  A load average of 14 does not pin down the regime.
- **The median check itself fails the fixed build on an oversubscribed VM.**
  It failed 2/10 plain, 4/10 `-race` and 2/10 `GOGC=1` in the shared-VM run,
  and none once the VM was quiet. Quiet, it passed 60/60 where the check tells
  the builds apart (four pinned CPUs and the quota) and 55/55 on two pinned
  CPUs, where the pre-fix gate passes too, so those 55 say nothing about the
  check's power. That run is not a calibration of a `ubuntu-latest` runner. It
  does show what a noisy neighbour does to the current bound.
- **The checks that do separate the builds are not wake latency.**
  `TestQuietProcs_FollowsGOMAXPROCS` (`quietAt(2) = true, want false`) and
  `TestPollResult_Gates` fail on the pre-fix gate, darwin and Linux binaries
  alike (`two Ps: pollResult polled for 50.1µs`). Neither check depends on
  wake latency, so host load does not mask them.

## Reproduce

From the repository root, with `target/release/libgusset.a` built:

```sh
git apply bench/results/handoff-gomaxprocs/wake-tail/probe.patch
go test -c -o fixed.test .
sed -i '' 's/return procs >= quietMinProcs/return procs > 1/' drain.go
go test -c -o prefix.test . && git checkout drain.go
bench/results/handoff-gomaxprocs/wake-tail/load.sh 14 1200 &
bench/results/handoff-gomaxprocs/wake-tail/interleave.sh darwin.txt 16 ./prefix.test ./fixed.test
awk -f bench/results/handoff-gomaxprocs/wake-tail/summ.awk darwin.txt

# Linux, with the patch applied to the worktree; OUT holds build.sh and run.sh
podman run --rm -v "$PWD":/src:ro -v "$OUT":/out localhost/gusset-stress bash /out/build.sh
podman run --rm -v "$OUT":/out localhost/gusset-stress bash -c 'taskset -c 0,1 bash /out/run.sh pinned2.txt 15'
podman run --rm --cpus=2 -v "$OUT":/out localhost/gusset-stress bash /out/run.sh cpus2.txt 10
```
