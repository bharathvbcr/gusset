# Changelog

## [Unreleased] - 2026-09-20 · Audit and hardening

Findings from a full audit of the v0.0.1 tree, validated by adopting Gusset in a
second codebase (DevCouncil's `go_orchestrator` driving its `dc-glob` crate). Every
fix below ships with a test that fails against the pre-fix code.

### Security and correctness (protecting I2, I3, I4, I6)

- **Untrusted input could select a Rust panic.** The built-in diagnostic engine
  chooses behaviour from `input[0]` — bytes 1, 2 and 3 are panics — and ran by
  default whenever no adopter engine was registered. Since no C export registers
  one, that was every process before its Rust engine had installed itself. A
  submission with no registered engine is now refused; the diagnostic engine is
  reachable only through `gusset.WithDiagnosticEngine()`, and a registered engine
  always outranks it.
- **ABI version 2.** `AllocStats` crossed the boundary unverified: version 1
  checked three `#[repr(C)]` types while `gusset_alloc_stats` wrote a fourth
  straight into Go memory. `AbiLayout` now covers four types (28 → 36 bytes), and
  Go additionally cross-checks Rust's reported layout against what cgo compiled,
  because `internal/ffi/gusset.h` is hand-maintained rather than generated.
- **`Wait` ignored its context.** A ticket that was never submitted, or a second
  concurrent `Wait` on one ticket, parked the caller forever. Now `ErrUnknownTicket`
  and `ErrTicketBusy`.
- **Completion-pipe descriptor could be closed twice.** `Handle` took the write fd
  at construction, so an `open` that failed after the `Arc` existed closed a
  descriptor the caller still owned. Ownership now transfers only on success.
- **`gusset_shutdown` was a stub** that returned `FFI_OK` without draining,
  cancelling or refusing anything. It now latches a shutdown flag, cancels every job
  on every live handle, waits up to `drain_ms`, and returns `FFI_ERR` with the
  residual count when cooperative work outlives the budget.
- **Unbounded resources bounded:** pool size capped at 1024 and refused rather than
  clamped; the completion-write EAGAIN path backs off and gives up instead of
  spinning a core forever; the panic-location map is capped at 256 entries; the log
  ring evicts oldest lines instead of clearing itself, and truncates a line larger
  than the whole budget (it previously stored 500,001 bytes in a 64 KiB ring).
- **Fail-silent locks removed.** Poisoned mutexes no longer skip the guarded work:
  a dropped result hung the Go caller, a skipped `sender.take()` hung `close` in
  `join`, and a lost cancel flag disabled cancellation without telling anyone.
- **Allocator double counting fixed.** `RawBuffer` counted its allocations by hand
  *and* through `Counting` when an adopter installed it, reporting every buffer at
  twice its size to `AdviseMemoryLimit`.
- Unknown `CallHeader.flags` bits are rejected; a failed `sigaltstack` install is
  logged rather than silently degrading I5; trace context moved off the bare
  `"spanContext"` string key to `gusset.SpanContextKey`.

### Gates that were not gating

- **The lint job could never pass.** Its forbidden-pattern audit grepped the whole
  tree and matched the rules as written in `AGENTS.md`, `docs/PLAN.md` and the
  workflow file itself. Now scoped to tracked `*.go`/`*.rs`, verified to pass clean
  and to still detect a planted match.
- **Clippy skipped all test code** (no `--all-targets`), so R3's disallowed-method
  rules never saw it — and `tests/exports_match.rs` was violating them.
- **The export check never inspected the shipped archive.** It preferred
  `target/debug` while cgo links `target/release`, and it ignored `nm`'s stderr —
  with `lto = "fat"` the release archive is LLVM bitcode that a system `nm` cannot
  read, so it printed an error, exited 0, and produced an empty symbol list
  indistinguishable from a clean one. It now uses the toolchain's `llvm-nm`, checks
  every archive present, and fails when a reader cannot parse one.
- **Miri ran zero tests.** The library had no unit tests at all; `header` and
  `ffi::alloc` now have six, and the job fails if the count is zero.
- **The ASan job only built.** It now runs the Rust suite under the sanitizer.
- Added the `cgocheck2` job (R6 and R16 named it as their enforcer; it did not
  exist) and a `memlimit` job. `AGENTS.md`'s testing table now carries a status
  column.

### Every remaining MISSING gate, implemented

The status column introduced above listed eight gates that did not exist. All of
them now do. Where the table's original wording named a tool that would have meant
a new dependency, the implementation differs and the table says why.

- **`musl` (R8's enforcer).** Runs the suite in `golang:1.27-alpine`, where the
  128 KiB default thread stack is actually in play. R8 also gained a gate that works
  everywhere: `pool::sys::current_thread_stack_size` reads the size back off the
  running worker, so `worker_stack_is_explicitly_sized_not_inherited` fails anywhere
  the explicit sizing is dropped. A deep-recursion probe cannot do that — on glibc
  and darwin it passes whether or not Gusset set the size, because the platform
  default already covers it. Workers also log a shortfall instead of degrading
  quietly, as they already did for `sigaltstack`.
- **`cross`.** `zig`/QEMU dropped. Gusset is unix-only by construction — POSIX pipe,
  `sigaltstack`, pthread stack accounting — so `lib.rs` now fails a non-unix build
  with a named `compile_error!` instead of a `libc` symbol cascade, `make cross`
  compile-checks the supported set (including musl), and CI asserts the Windows
  build *refuses* with that error. `docs/platforms.md` records what a port would
  have to add.
- **`bench` (R15).** `tools/benchdoc` regenerates the README table from benchstat;
  `make docs-check` fails when it drifts. The old `make docs` printed "Documentation
  up to date" and regenerated nothing, while the README's figures disagreed with
  benchstat over the very file they cited (parallel read 5.07 µs against a 5.237 µs
  median) and rested on five samples where benchstat needs six before it will quote
  a confidence interval. Numbers are now generated from a ten-sample run and carry
  their intervals.
- **`fuzz`.** Three Go native fuzz targets over the FFI boundary — no dependency,
  where `cargo fuzz` would have meant `libfuzzer-sys` and a nightly-only build.
  4.56M executions on the refusal path, 178K against the deliberately-panicking
  diagnostic engine, 45s on the buffer lifecycle: no crashers. Diagnostic mode 6's
  recursion depth is now capped, since it came from the payload and `[6, 0xFF ×4]`
  would recurse 4.29 billion frames and overflow even an 8 MiB stack.
- **Header diff.** `tests/header_match.rs` compares the hand-written `gusset.h`
  against the Rust exports *by type*, not just by name — no cbindgen dependency.
  Verified against three planted mutations: a widened parameter, a dropped `const`,
  and a removed parameter.
- **R4's cross-free.** `TestR4_CrossFreeIsDetectedUnderASan` cross-frees a Gusset
  buffer and then lets the owner release it. ASan accepts the libc free — a
  64-byte-aligned block comes from `posix_memalign`, and freeing that with `free` is
  legal C — and reports the owner's release as `attempting double-free`. R4 also
  gained an always-on static gate: `gussetvet` now rejects `C.free` outside the one
  package allowed to hold the deliberate violation.
- **The `soak` goroutine-leak assertion.** Uses Go 1.27's real `goroutineleak`
  profile rather than counting goroutines, and fails if the profile is unavailable
  instead of skipping.
- **The `memlimit` 200 MiB / 1% assertion.** Measured drift across a 200 MiB Rust
  allocation is 0.0000%.
- **The `cgocheck2` retained-pointer test.** Stores a Go pointer into C memory in a
  subprocess and asserts it is caught *with* the experiment and allowed *without*
  it, so the job cannot go green while the experiment is off. The obvious probe —
  passing a Go pointer to an unpinned Go pointer — is caught by baseline cgo
  checking in every build and would have proved nothing.
- **`tip`'s issue-on-failure.** Now exists, and reuses an open `toolchain` issue
  rather than filing one per week.

### Found by the new gates

- **A use-after-free of Rust memory in the close-race stress test.** It called
  `buf.Bytes()` and indexed the result while a concurrent `Close` was in flight.
  Before `Bytes()` was hardened to return nil after close, it got a live slice onto
  memory Rust had already released and wrote three bytes into it — invisible to the
  Go race detector, because the memory is not Go's. The nil return turned that
  silent corruption into a panic, which is how it was found.
- **R5's enforcer could be evaded by a name prefix.** `gussetvet` matched each
  required directive with `strings.Contains` over the whole cgo preamble, so
  `#cgo noescape gusset_cancel_all` satisfied the check for `gusset_cancel`.
  Deleting *both* directives for `gusset_cancel` left it reporting "R5 checks
  passed" — verified, then fixed by matching whole directive lines. That is a live
  pair in `exports.txt`, and R5 is the rule that stops cgo treating Go pointers as
  escaping into C, so losing it on one export is silent.
- **`staticcheck ./...` exited 1 on every commit**, so the `lint` job could never
  pass — the same shape as the forbidden-pattern audit fixed earlier in this
  release, found the same way: by running the gate instead of reading it.
  `tools/gussetvet` used `go/parser.ParseDir`, deprecated since Go 1.25 (SA1019).
  Replaced with a per-file `parser.ParseFile` walk, which is what the check
  actually needs — it reads cgo preambles and never asks which package a file
  belongs to — rather than adding `golang.org/x/tools/go/packages`. It now also
  fails if it parsed zero files, instead of reporting success having inspected
  nothing.
- **A checklist item claimed the header was generated by `build.rs` and diffed in
  CI, and had been ticked.** `build.rs` only enforces `panic = "unwind"` and has
  never run cbindgen. A `cbindgen.toml` sat in the repository root for a tool
  nothing invoked, which is what made the claim look plausible; it was deleted.

### Adoption (validated against DevCouncil)

- **Gusset could not be linked by any external Go module.** `#cgo LDFLAGS` resolve
  `target/release` inside a source checkout; `.gitignore` keeps it out of the module
  zip and the module cache is read-only. Adopters now build with
  `-tags gusset_pkgconfig` against the installed `gusset.pc`, or pass
  `CGO_LDFLAGS=-L…`. Both verified against a module-cache layout.
- `docs/adoption.md` documents the full recipe, including the two requirements
  nothing stated before: an umbrella crate must set `[lib] name = "gusset"` so its
  output is `libgusset.a`, and the adopter must export and call its own engine
  registration entry point.
- `lto` is no longer prescribed. No invariant depends on it, a real adopter had
  measured `thin` as the better trade, and `fat` is what made the archive opaque to
  `nm`.
- `crates/gusset-example` is now actually exercised. `init_example_engine` had zero
  callers anywhere in the repository, and the Go test named `TestPhase2_CpuBoundEngine`
  drove Gusset's built-in dispatcher — which carried its own near-copy of the
  example's opcode 10 — so the Phase 2 "second-app validation" had never run the
  adopter path it claimed to validate.

### Tests

- New: `tests/rust_runtime.rs` (7), `tests/rust_shutdown.rs` (1),
  `crates/gusset-example/tests/adopter.rs` (1), six lib unit tests,
  `tests/pitfalls/hardening_test.go` (5), `tests/pitfalls/adversarial_test.go` (13).
  Rust tests went from 1 to 15.
- Green under `-race`, `GOEXPERIMENT=cgocheck2`, `GOGC=1 -count=3`, and
  `GOMEMLIMIT=256MiB`.

## [0.0.1] - 2026-09-20

### Phase 0 · Seed Reproduction & Firewall Baseline
- Confirmed the audited document's firewall failure: `rs_guarded_doc` uses `CString::new(msg).unwrap()`, leading to double panic and `SIGABRT` under embedded NUL byte.
- Verified that Gusset's hardened firewall catches NUL-byte panics safely, returns `ErrPanic`, records file and line, and keeps the Go process alive.
- Toolchains used: Go 1.27.1 / Rust 1.98.0 (darwin/arm64, Apple M5 Pro).
- Measured numbers: parallel call 5.07 µs/op, no-op call 20.38 µs/op, submit/wait 23.68 µs/op, large buffer 32.19 µs/op, channel hop 18.32 ns/op (0 B/op, 0 allocs/op). Committed in `bench/results/darwin-arm64-go1.27.1-rust1.98.0.txt`.

### Phase 1 · Core Gusset Runtime (v0.1)
- Implemented Rust runtime crate `crates/gusset` with strict `#![deny(unsafe_code, unsafe_op_in_unsafe_fn, improper_ctypes_definitions, missing_docs)]` crate-wide (with `#[allow(unsafe_code)]` only on `ffi` and `pool::sys`).
- Three `#[repr(C)]` types with static size/alignment assertions: `CallHeader` (40 bytes), `FfiStatus` (48 bytes), `AbiLayout` (28 bytes).
- 14 exported C functions verified against `internal/ffi/exports.txt` and `nm` output in `tests/exports_match.rs` (Rule R1).
- Worker pool with 8 MiB explicit stack size and 64 KiB `sigaltstack` (Rule R8).
- Completion pipe over `os.Pipe` waking Go netpoller without pinning OS threads (Invariant I4).
- Custom `go vet` analyzer `tools/gussetvet` enforcing `#cgo noescape` and `#cgo nocallback` on all exports (Rule R5).
- 5-case panic zoo in `tests/panic_zoo/` passed (&str, String, non-string, NUL byte, Display panic).
- 8-case pitfall suite in `tests/pitfalls/` passed (ABI drift, poisoning, deadline, migration, large buffer, alloc stats, recursion, thread-cap soak).
- All 16 rules (R1–R16) enforced by tests or lints with zero unaddressed rules.

### Phase 2 · Second-App Validation
- Adopter validation in `crates/gusset-example` under CPU-bound batch arithmetic (vector sum-and-square) exercising cooperative cancellation checks.
- Public surface diff between v0.1 and v0.2: **0 new functions** (14 Rust exports and 10 Go entry points preserved strictly).

### Phase 3 · Public Release Preparation
- Complete `Makefile` with `all`, `build`, `test`, `bench`, `lint`, `ci-local`, and `clean` targets.
- Comprehensive `README.md` with measured numbers, architectural diagrams, quickstart, and invariants.
- CI workflows in `.github/workflows/`: `matrix.yml` (multi-OS, soak, race, lint), `nightly.yml` (Miri, ASan), `tip.yml` (Go tip weekly).

### Phase 4 · Out-of-Process IPC Specification
- Documented Phase 4 out-of-process Metal GPU crash isolation architecture in `docs/ipc.md` using `iceoryx2` upstream shared memory transport.
