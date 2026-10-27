# Dispatch, deadline, Discard and Stats baseline

The quiet-host baseline that card ft-568a30e2 (criterion 4) asks for, and that
ft-8a8279f5's R7 is to be A/B'd against. Both files were recorded by
`bench/record.sh` on 2026-10-26 from a clean checkout of `d1aa1b8`; each
file's provenance header carries the commit, the idle samples before and after,
and the round order. The only code change between `d1aa1b8` and the commit this
directory landed on is `bench/result_size_test.go` (new benchmarks, not run
here), so the library code measured is main's.

| File | Recorded by | What it holds |
| --- | --- | --- |
| `darwin-arm64-go1.27.3-rust1.99.0.txt` | `make bench-dispatch` | `crates/gusset/benches/dispatch.rs`: diagnostic vs registered engine on opcode 0, a nonzero registered opcode, and opcode 0 with the registry loaded, interleaved ABBA rounds |
| `go-suite-darwin-arm64-go1.27.3-rust1.99.0.txt` | `make bench` | the Go suite, including `BenchmarkDeadlineExpiry`, `BenchmarkDeadlineInFlight`, `BenchmarkDiscard` and `BenchmarkStats` |

Summarise either with benchstat:

```
benchstat -filter '.unit:sec/op' bench/results/dispatch/darwin-arm64-go1.27.3-rust1.99.0.txt
benchstat bench/results/dispatch/go-suite-darwin-arm64-go1.27.3-rust1.99.0.txt
```

**Why the Go file is here and not in `bench/results/`.** `make bench` writes
there, and `make docs` builds the README table from the single `.txt` in that
directory. This recording is noisier on two of the README's rows than the one
the README already cites: `GussetCallNoop` starts high and falls over its ten
samples, and `GussetBufferLarge` scatters, so benchstat gives both a confidence
interval several times wider than in the cited file. Replacing the README's
source with it would have widened those rows for no gain, so it was moved here
and the README is unchanged. Of the four rows this baseline exists for,
`DeadlineExpiry`, `DeadlineInFlight` and `Stats` have narrow intervals;
`Discard`'s is wider, comparable to `GussetSubmitWait`'s in the same file.
