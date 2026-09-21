# Seed benchmark and firewall repro

Re-run on every toolchain bump (R15). Both measured columns are kept: a single
column would have to be relabelled on every bump, and the point of these numbers
is that the *shape* is portable while the magnitudes are not.

```
cd rs && cargo build --release
cd ../go && go test -run xxx -bench 'Cgo|Batch|Channel|Unbuffered|PureGo' -benchtime=1s .
cd ../esc && go test -run xxx -bench . .
```

The regex matters: this module also holds `crossover_test.go`, so a bare
`-bench .` additionally runs the transport, thread-pressure and thread-scaling
sweeps — about a minute of extra work, and two of those are only valid one
transport per process (`make bench-crossover`, `make bench-scaling`).

| Benchmark | Go 1.24.7 / Rust 1.95<br>x86-64 Linux, 2 vCPU | Go 1.27.1 / Rust 1.98.0<br>darwin/arm64, Apple M5 Pro |
| --- | --- | --- |
| cgo call into Rust no-op | 64 ns/op | 18.43 ns/op |
| cgo call into Rust no-op, `RunParallel` | 36 ns/op | 1.79 ns/op |
| cgo call into inline C no-op | 76 ns/op | 14.93 ns/op |
| Batch 16 / 256 / 4096 items | 4.25 / 0.67 / 0.43 ns/item | 1.29 / 0.227 / 0.159 ns/item |
| Buffered channel send | 56 ns/op | 17.62 ns/op |
| Unbuffered request/response | 365 ns/op | 186.5 ns/op |
| Pure Go loop, same body | 0.64 ns/item | 0.270 ns/item |
| Pointer arg without `#cgo noescape` | 89 ns/op, 64 B/op, 1 alloc/op | 23.86 ns/op, 64 B/op, 1 alloc/op |
| Pointer arg with `#cgo noescape` | 67 ns/op, 0 allocs/op | 15.90 ns/op, 0 allocs/op |

**The `RunParallel` row is not a per-call latency and does not compare across
these two columns.** `b.RunParallel` reports wall time per op summed over every
P, so its figure falls as core count rises: 36 ns on 2 vCPU and 1.79 ns on 18
are roughly 72 ns and 32 ns of per-call latency. Reading 36 against 1.79 as a
20x improvement is reading the core count, not the boundary.

Two results that do survive the platform change, and one that does not:

- **`#cgo noescape` still earns its place.** It removes the same single 64-byte
  allocation and about a third of the call on both machines (89 -> 67 ns there,
  23.86 -> 15.90 ns here). This is why R13 requires it on every export.
- **Batching still dominates.** Per-item cost falls roughly 27x from 16 to 4096
  items on both. A boundary crossing is a fixed cost to amortise, not a rate.
- **"C is cheaper than Rust across cgo" does not survive.** The inline-C no-op
  was *slower* than the Rust one on x86-64 Linux (76 vs 64 ns) and is *faster*
  here (14.93 vs 18.43 ns). The ordering is a property of the toolchain and the
  machine, not of the two languages.

`go test -run TestGuardedNUL -v ./go` reproduces the audited document's firewall bug:
`rs_guarded_doc` uses `CString::new(msg).unwrap()` in the `Err` arm, a panic message
containing a NUL byte panics again, the second panic unwinds out of `extern "C"`, and
the whole Go test process dies with `SIGABRT`. Phase 0 exit criterion: this test fails
against the original guard and passes against the hardened one.
