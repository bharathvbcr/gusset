# Seed benchmark and firewall repro

Measured 2026-09-20 on Go 1.24.7 / Rust 1.95, x86-64 Linux, 2 vCPU. Re-run on every toolchain bump (R15).

```
cd rs && cargo build --release
cd ../go && go test -run xxx -bench . -benchtime=1s .
cd ../esc && go test -run xxx -bench . .
```

| Benchmark | Result |
| --- | --- |
| cgo call into Rust no-op | 64 ns/op (36 ns/op parallel) |
| cgo call into inline C no-op | 76 ns/op |
| Batch 16 / 256 / 4096 items | 4.25 / 0.67 / 0.43 ns/item |
| Buffered channel send | 56 ns/op |
| Unbuffered request/response | 365 ns/op |
| Pure Go loop, same body | 0.64 ns/item |
| Pointer arg without `#cgo noescape` | 89 ns/op, 64 B/op, 1 alloc/op |
| Pointer arg with `#cgo noescape` | 67 ns/op, 0 allocs/op |

`go test -run TestGuardedNUL -v ./go` reproduces the audited document's firewall bug:
`rs_guarded_doc` uses `CString::new(msg).unwrap()` in the `Err` arm, a panic message
containing a NUL byte panics again, the second panic unwinds out of `extern "C"`, and
the whole Go test process dies with `SIGABRT`. Phase 0 exit criterion: this test fails
against the original guard and passes against the hardened one.
