# BUILD_REPORT

Date: 2026-10-13. Branch: `wave-w1`. HEAD: `e7400a4`.
Base of this wave: `73fbf32`. Not pushed. Not merged. `AUDIT_REPORT.md` stays untracked.

Done was not requested for any card. `gitpulse_complete_task` with `status: review` returned `verification: null` and `held_for_review: false`. `.devcouncil/config.yaml` has `gates.mode: off` and `execution.hook_gate.mode: off`. GitHub Actions has not run this branch.

## Tasks moved to review

| Card | Revision now | Status | Commits on `wave-w1` |
| --- | --- | --- | --- |
| `ft-8a7cd8ef6e74a81b8db1a0f709d7a9f2` Go test hygiene | 3 | review | `c8115b8` |
| `ft-1f4a1228bb088b27773011ac1a211980` CI gates | 8 | review | `c49844d`, `959d7e7` |
| `gs-isolate-corrupt-frame-hang` isolate.Proc | 7 | review | `0776d42` |
| `gs-boundary-defence-in-depth` boundary residuals | 6 | review | `76e2bb1`, `790967a` |
| `ft-0f6a723dfd8c2b3cbf7cec23c07df8bb` adopter residuals | 5 | review | `1cfa0d4`, `6d16320` |
| `ft-568a30e26178bf6030c6e1d25c68c3dd` bench recorder | 3 | review | `94fa191` |
| `ft-9977d2b210e0e190aaf277a581510647` platform named-error gate | 4 | review | `324921f`, changelog `e7400a4` |

W1 changelog is `fd75830`, with the fuzz-lane sentence in `959d7e7`. W2 changelog is `65fad53` for boundary, adopter, and bench, and `e7400a4` for the platform gate. `gitpulse_complete_task` status review returned `verification: null` and `held_for_review: false` (sequences 4163, 4164, 4165, and 4183 for platform). Done was not requested.

Pending before any of these can leave review: Manvi `work.verify.task` on this head, and a green GitHub Matrix (plus adopter, where the card asks for it). Criterion 14 of the CI card ("the race job log lists internal/isolate") is unmet until that job runs. The workflow file includes `./internal/...` on the race step and the `GOGC=1` step.

### What each review contains

Hygiene (`c8115b8`, I1). `LiveBytes` is compared with `<=` after two consecutive stable samples. A held buffer still fails. The 500 ms and 1 s bounds that could only false-fail are removed. `gates_test.go` was not changed. `-p 1` was not added. This turn did not re-run that package; the implementer run and the adversarial approval are the evidence already on the card.

CI (`c49844d`, `959d7e7`, I4). Host-load skips go through `internal/hostprobe.Skip`. `GUSSET_REQUIRE_PROBES=1` fails the test with the same text; any other value still skips. Race and `GOGC=1` include `./internal/...` and do not add `tests/shutdown`. The matrix and nightly fuzz steps set the same variable. `tests/ci` models the environment the `go test` process sees: YAML `env` is the initial export, the last assignment on one `export` wins, a bare `NAME=1` does not export, a prefix applies only to that process, and every `go test` in the step must see exactly `1`. `gofmt` is `gofmt -l --` on NUL-separated tracked paths and fails when gofmt writes to stderr. benchgate keeps reading the sec/op table after a short row; `-allow-one-sided` defaults to empty. `Makefile`, `AGENTS.md`, `CONTRIBUTING.md`, `docs/platforms.md`, `install.sh`, and `bench/bench_test.go` were not edited.

Isolate (`0776d42`, I3). An incomplete frame read is a `frameError`. Bytes `io.ReadFull` already copied are prepended after `detachBufio`. A deadline that consumed nothing still probes once for 50 ms. A deadline between the length write and the body write poisons the stream and closes stdin. `Wait`'s cancel runs on its own goroutine with a one-second budget. After a successful `Start`, `waitProc` is the only `cmd.Wait`. The typed-nil trace recover is duplicated inside isolate. `gusset.go` was not edited, and no new public helper was added. The SIGABRT accept-list in `isolate_internal_test.go` was not changed (`gs-flaky-isolate-sigabrt-cgocheck2` still owns that list).

## Tasks left in review, untouched

These were already in review. This wave did not edit their cards or request done. Their remaining gap is CI-on-GitHub, or a Manvi verification record.

- `gs-allocator-api-hygiene-leak` revision 5
- `gs-ci-lint-staticcheck-go127` revision 5 (P0)
- `gs-flaky-isolate-sigabrt-cgocheck2` revision 4
- `gs-gussetseal-darwin-ar-padding` revision 7 (P0)
- `gs-issue-3-asan-quadratic-engine` revision 7
- `ft-94a2f74b1f9d5a4d6c51bc1c8813d2c8` revision 6
- `ft-c689767b57a8ed6768589485f751f3f0` revision 4

## Deferred or blocked

`ft-1a7e8a995978987a62ee66b6151f9675` (build identity, revision 2, ready, P3). Deferred for this wave. It plans `Makefile` and `AGENTS.md`, which `ft-1f4a1228` also planned. The higher-priority CI card ran; the build-identity card did not. The deferral is already on the card. The intended mechanism, when it starts: untrack `archive_hash.go`, generate an ignored file with a committed stub, and edit `AGENTS.md` line 127 only. `ft-86b4ae63` owns the rest of that file. Do not make `gen.go` alone follow `CARGO_TARGET_DIR`. Do not add a runtime build identity in `init`. The linker warning `target/debug` not found, seen while testing isolate, belongs to this card. The link used the existing `target/release/libgusset.a`.

W0 and W0b were not started. Each of these needs an explicit go before anyone pushes, opens a PR, merges, changes branch protection, tags, or files a GitHub security advisory:

- `ft-97faf4f4d956b3d48396e3e730e85347` revision 2, ready. Land `main` through a PR.
- `ft-415ddf1f6ef83644e1789f98ee3b99d3` revision 4, ready. Branch protection after that merge.
- `ft-0dde896cd5cd2c9929103414e9b43884` revision 8, ready. Tag `v0.0.3`, then the advisory for the `v0.0.2` WaitBuffer write-after-free (fix `38f0296`).

W2 is in review: boundary, adopter, bench, and the platform named-error gate. Nothing in W2 is still ready or in progress.

Adopter criterion 6 stays blocked: no `adopter.yml` GitHub run id, because this branch is not pushed. Bench criterion for a quiet-host baseline stays pending. One idle sample was not treated as a recording, and no `bench/results` file was committed. No number from the functional bench run is a baseline.

The `Makefile` comment above the `cross` target still says the Rust gate is non-unix. The code now refuses every target whose `target_os` is not linux or macos. This card was not allowed to edit `Makefile`. `docs/platforms.md` is the corrected statement. No follow-up task was filed.

Not touched, still backlog: `ft-6867f957f58a3a343e6253a28a283eef` (owner `@bharathvbcr`), `ft-8a827cf5d4de697dbee38db09823ee24`, `ft-d3d3888d9bb226b0313b3e652b05ba83`, `ft-83151a087d5812fd3688eb20b0032fbc`.

No task is `in_progress`. No new task was filed. W0, W0b, and W3–W5 were not started.

## Benchmark deltas

No quiet-host baseline was recorded, and no percentage is claimed. Hygiene, the CI checker, `hostprobe`, and the isolate spike are not on the in-process call path. The platform change is a compile gate, not a call path. The new Go benches and the Rust `dispatch` bench binary were run once to prove they complete. Those runs are not a measurement. The 5% bar was not measured.

## Tests run on this tree

Verified this turn:

- `go test ./internal/isolate/ -count=1 -timeout 240s` → `ok`, 64.859s (darwin). `ld` warned that `internal/ffi/../../target/debug` is missing.
- `go test ./tests/ci/ -count=1 -timeout 120s` → `ok`, 0.197s, after the fuzz-lane env edit.
- `gofmt -l` on the isolate Go files and the CI Go files was clean before those commits.

Verified earlier in this session, on the same sources before they were copied here, and again on this tree for the four CI packages:

- `go test ./tools/ci/benchgate/ ./tools/ci/ ./tests/ci/ ./internal/hostprobe/ -count=1` → all `ok` (benchgate 0.080s, tools/ci 1.978s, tests/ci 0.186s, hostprobe 0.583s). The hostprobe time includes `TestRequireProbesTurnsALateProbeIntoAFailure`, which runs a child `go test`.
- Isolate adversarial pass approved: header splits, a body split, three frames already queued, an oversize declared length, invalid JSON of a complete frame, `{}`, and a zero-length body. A past-deadline read that consumed nothing still probes.

Not run: the full `go test ./...` matrix, `-race`, `GOGC=1` as a CI job, GitHub Actions, Windows, and a clean EOF with the isolate write end already closed. Those last two were named gaps in the isolate adversarial pass, not breaks.

## DevMap and GitPulse after the wave

`gitpulse_insights` and `gitpulse_collision_risk` on `/Users/bharath/Code/devtools/gusset`: one registered worktree, `collisions.ok` true, `overlapping_files` 0, `unscanned_worktrees` 0, `truncated` false, `sessions_ok` true, Claude 0, Codex 0. The only dirty file at scan time was untracked `AUDIT_REPORT.md`. This report was written after that scan.

The scan cannot see the implementer clones under `/Users/bharath/.grok/worktrees/devtools-gusset/`. They are full clones, not entries in `git worktree list`. GitPulse on them returns `REPOSITORY_TRUST_REQUIRED`. They were finished before the copy, and this wave did not keep editing them. A clean collision result is not a scan of those directories.

DevMap `generation_id` 949, `source_freshness` true, `analyzer_freshness` false, `is_fresh` false: "stored extraction payload is obsolete". Impact and affected-tests answers from this index are unknown, not safe. The index was not rebuilt. An earlier impact of `internal/isolate/proc.go` at depth 2, on the previous generation, showed only edges inside `internal/isolate` and was `walk_incomplete`.

## Fable's decisions

1. Do not run `ft-1a7e8a99` (build identity) in the same wave as `ft-1f4a1228`. The CI card is P1. The build-identity card stays deferred, with the mechanism above.
2. This wave's three non-overlapping cards were isolate, test hygiene, and the CI residuals. The CI implementer did not edit `Makefile`, `AGENTS.md`, `CONTRIBUTING.md`, `docs/platforms.md`, `install.sh`, or `bench/bench_test.go`.
3. W0 and W0b outward GitHub actions stay stopped until an explicit go. Landing through a PR was already decided by the owner and was not executed.
4. Typed-nil trace carrier: duplicate the recover inside isolate. Do not export a helper. Do not edit `gusset.go`.
5. A deadline-interrupted isolate write may be partial: poison the `Proc`, close stdin, restore permits for unsent tickets. `Wait`'s cancel must not run on the caller's goroutine, and it must not use an unbounded `context.Background()`. Budget is `cancelSendBudget` (1s). If the context ends while still waiting for the write lock, return without poisoning.
6. Exactly one goroutine owns `cmd.Wait` after a successful `Start`. The handshake-failure path still waits itself, before a `Proc` exists. After exit, if `readerDone` is not closed within `protocolDrain` (200 ms), unblock the protocol read without discarding frames already readable.
7. Probe on any `os.ErrDeadlineExceeded`, including a read that consumed nothing. Prepend `frameError.consumed` only when the error carries those bytes, and only after `detachBufio`.
8. CI shell model: bare `NAME=1` does not export and must not count. YAML `env` is the initial exported value. The last assignment on one `export` wins. A step is armed only when every `go test` in it would see exactly `1`. After the confirmation pass, stop the shell-parser chase.
9. Hygiene: success is `LiveBytes <=` baseline after quiesce, before the snapshot. Remove the two redundant wall-clock bounds. Do not add `-p 1`. Do not change `gates_test.go`.
10. CI residuals only for criteria 10–14. Criteria 1–9 were already met in source.
11. Review cards whose only gap is CI-on-GitHub stay in review. Do not touch `gs-issue-3`.
12. Shell residuals, recorded and not implemented: `export -n`, `+=`, `unset X || export X=1`, `false && NAME=0`, an unquoted YAML trailing comment, a space after a closing quote, `unset -f`, `command unset`, `builtin unset`, `/usr/bin/env`, `env -i`, `eval`, `source`, subshells, command substitution, aliases, `declare`, `readonly`, process substitution, `set -a`, backslash continuations, pipelines, and flow-style YAML. A YAML value of `1 ` stays not equal to `1` under the existing contract.

## Owner decisions still pending

Already decided on 2026-10-13. Execution is still stopped.

1. Land `main` through a PR, not a direct push (`ft-97faf4f4`).
2. After that merge, enable branch protection, enforced for admins, 0 approvals, Matrix and adopter required, force-push and deletion blocked, with a `DECISIONS.md` row (`ft-415ddf1f`).
3. Support only the latest release. File the GitHub security advisory for the `v0.0.2` WaitBuffer write-after-free (affected `<=0.0.2`, patched `0.0.3`, fix `38f0296`) once `v0.0.3` is tagged (`ft-0dde896c`).
4. Cut `v0.0.3` as serial W0b immediately after W0. Priority was raised P2 to P1.
5. Change `FFI_ERR` to `FFI_BAD_ARG` in the release after `v0.0.3`, with a `DECISIONS.md` row, the `gusset.h` refusal list, and a changelog entry (`ft-3d60ee16`). Pre-1.0.
6. No lone-call fast path. The 1.5x criterion on `ft-8a8279f5` is superseded. The replacement criterion is that `docs/choosing.md` states the generated crossover. No `DECISIONS.md` edits in W4.

## W2, added when the three cards reached review

Boundary (`76e2bb1`, `790967a`, I1). A failed Apple `F_SETNOSIGPIPE` writes back the `F_GETFL` value captured before the call. A failed restore names both errors. `gusset.h` documents `reserved` as the opcode and that unknown flag bits are refused. `ffi_exports` sets `reserved` from its input on every submit. DECISIONS 2026-10-13 keeps `gusset_init` and `gusset_shutdown` out of that target. Canonical `cargo test -p gusset --lib -- set_nonblocking callheader_comment` passed, 11 tests.

Adopter (`1cfa0d4`, `6d16320`). The step-5 skip is a `::warning::`. `GUSSET_REQUIRE_STEP5=1` fails it. Nightly job `step5-snippet` runs the walkthrough with the Allocator probe forced on. A snippet whose type does not exist fails `cargo check`. `gusset-example` is an rlib and `libc` is dev-only. Adopter prose uses `gusset_core::`. The quoted `gusset::set_engine_handler()` error was left as the library prints it. Adversarial found that `checkDirectives` accepted directives from `//go:build ignore` and other-GOOS files. `go/build.Default.MatchFile` is the fix. Canonical `go test ./tools/gussetvet/` and `go run ./tools/gussetvet ./internal/ffi` passed.

Bench (`94fa191`, I4). The recorder kills only the child process group it started, publishes the lock by rename after `owner` and `pgid` exist, holds a lock with no pgid for 2 seconds, and alternates variant order. Canonical `sh bench/record_test.sh` passed. The three Go benches passed at `-benchtime=1x`. Opcode 0 uses `set_engine_handler` because `register_engine` refuses opcode 0.

Fable for W2:

13. The four W2 cards do not run all at once. Platform waits until boundary, adopter, and bench have stopped editing. Bench tests live under `bench/`, not `tests/ci/`. Boundary does not edit `nightly.yml` or `matrix.yml`. Adopter does not edit `Makefile`, `install.sh`, or `gusset.pc.in`.
14. Boundary criterion 6 is the DECISIONS row (15 of 17). No lifecycle fuzz harness. No new export.
15. Adopter criterion 6 stays unmet without the explicit-go push. The card goes to review with that criterion pending.
16. A quiet-host baseline is recorded only by `bench/record.sh` when the idle check passes for a real run. A single idle sample is not that run. Do not invent bench numbers. Do not edit `ft-8a8279f5`.
17. `gusset-example` drops `staticlib` and moves `libc` to dev-dependencies. No new `extern "C"` init. The opcode-11 panic stays.
18. DevMap generation 950 stayed stale after `devmap build --manifest` (no source changes, analyzer payload still obsolete). Impact edges that were returned were used. Missing edges were treated as unknown. GitPulse on the canonical checkout saw one worktree. The implementer clones under `/Users/bharath/.grok/worktrees/` are not in `git worktree list` and return `REPOSITORY_TRUST_REQUIRED`. That is not a clean scan.

Verified on the canonical tree after the fast-forward, HEAD `65fad53`:

- `cargo test --locked -p gusset --lib -- set_nonblocking callheader_comment` → 11 passed.
- `gofmt -l` on the new Go files was empty.
- `go test -count=1 ./tools/gussetvet/` → ok, 0.135s.
- `go run ./tools/gussetvet ./internal/ffi` → R5, R4, R5 callbacks, R1 passed.
- `sh bench/record_test.sh` → variant order, lock grace, one winner of 8, INT, TERM.
- `go test -run '^$' -bench 'Benchmark(DeadlineExpiry|Discard|Stats)$' -benchtime=1x -count=1 ./bench` → pass.

Not run on that earlier tree: full `go test ./...`, `-race`, `GOGC=1`, `make ci-local`, GitHub Actions, the nightly step-5 job, a full `record.sh` measurement, Windows, and FreeBSD.

## W2 platform, added when the card reached review

Platform (`324921f`, changelog `e7400a4`, I4, revision 4, sequence 4183). `lib.rs` allow-lists `target_os` linux and macos and cfg's out the modules with the same condition. The `compile_error!` string is the one Windows CI already matches. `internal/ffi/unsupported.go` is built for `!cgo` or `windows` and fails compilation by using a string as an `int`, which is what puts the phrase on the compiler's output. `docs/platforms.md` says cgo is required, that Windows fails on both sides, and that other unix fails the Rust build with the same named error. The cross job, beside the existing Windows step, checks FreeBSD, illumos, `CGO_ENABLED=0`, and `GOOS=windows`. `tests/ci` fails if those steps disappear or if a comment containing the phrase is treated as the check. `pool/sys.rs`, `nightly.yml`, `Makefile`, `AGENTS.md`, `DECISIONS.md`, `gusset-example`, `bench/`, and `fuzz/` were not edited.

Adversarial pass approved (subagent `01a13832`, no edits). Implementer was `01a13827`.

Fable for this card:

19. Platform runs alone, after the other three W2 cards are committed. Keep the existing `compile_error!` string. Cfg out the rest of the crate on a refused target so the failure is that string and not E0425 in `pool::sys`. The Go phrase must be printed by the compiler. Illumos stands in for OpenBSD on this host because `rustup target add x86_64-unknown-openbsd` has no prebuilt std for `stable-aarch64-apple-darwin`. Do not edit `Makefile`. Do not request done.

Verified on the canonical tree after the fast-forward, at `324921f`, before the changelog commit:

- `CGO_ENABLED=0 go build ./...` → exit 1, only `unsupported.go:9` printing `gusset requires cgo on a unix target` and `docs/platforms.md`.
- `GOOS=windows go build ./...` → exit 1, the same line.
- `cargo check --locked -p gusset --target x86_64-unknown-freebsd` → exit 101, one `compile_error!`, no `E0425`, no `pthread_getattr_np`.
- `cargo check --locked -p gusset --target x86_64-unknown-illumos` → exit 101, the same single error.
- `cargo check --locked -p gusset --all-targets` on this darwin host → exit 0.
- `go test -count=1 ./tests/ci/` → ok, 0.165s. `gofmt -l` on the two new Go files was empty.
- `go run ./tools/gussetvet ./internal/ffi` → R5, R4, R1 passed. `CGO_ENABLED=1 go build -o /dev/null .` → exit 0.

The adversarial pass, in the implementer clone of the same commit, also checked installed std for `aarch64-apple-darwin`, `x86_64-apple-darwin`, `x86_64-unknown-linux-gnu`, `aarch64-unknown-linux-gnu`, and `x86_64-unknown-linux-musl` (`cargo check --locked -p gusset --target`, each exit 0), Windows `x86_64-pc-windows-msvc` (exit 101, named error only), and `cargo check --all-targets --target x86_64-unknown-freebsd` (exit 101, named error for lib and lib test, no `E0425`). Those cross-std checks were not repeated on the canonical tree after the fast-forward. OpenBSD remains unverified because the std is not published for this host.

DevMap this turn: the DevMap MCP reported generation 953, `is_fresh` true, `analyzer_freshness` true, `repository.root` `/Users/bharath/Code/devtools/gusset`. GitPulse Insights in the same turn reported generation 953 with `analyzer_freshness` false and `is_fresh` false ("stored extraction payload is obsolete"). The two answers disagree, so impact was not treated as a complete blast radius. The commit's file list was taken from `git diff`. Insights on the canonical checkout: one worktree, `collisions.ok` true, `overlapping_files` 0, `unscanned_worktrees` 0, `sessions_ok` true. The implementer clone is not in `git worktree list` and returns `REPOSITORY_TRUST_REQUIRED`.

Not run: full `go test ./...`, `-race`, `GOGC=1`, `make ci-local`, `make cross` end to end, GitHub Actions. No new GitPulse task was filed.
