# Gusset in the other apps: DevCouncil, Manvi, GitPulse

[Documentation Hub](README.md) · [Adoption Guide](adoption.md) · [Benchmarks, Plotted](benchmarks.md)

Status as of 2026-09-27 for DevCouncil and the timings. The Manvi and GitPulse
sections were surveyed on 2026-10-26, by reading source only. The numbers were
measured on a Linux VM and are listed at the end.

## What exists today

**DevCouncil** links Gusset for one engine: `dc-glob`'s fnmatch, through an
umbrella staticlib (`rust/gusset-engine`, archive name `libgusset.a`) and the Go
package `backend/go_orchestrator/gussetfn`. Its only production caller is the
`devcouncil gusset-check` command. The write gate uses Go's `fnmatch` on
purpose. The package comment records why: a gate that returns a bool has no way
to report a Gusset error, so the gate would either fail open or fail closed.

This was checked against the current Gusset, with the completion ring and 17
exports:

- The umbrella rebuilds, and `nm` shows all 17 `gusset_*` symbols plus
  `devcouncil_gusset_init`. Gusset's exports reach the archive through the
  dependency, with no re-export list to keep in sync.
- `go test ./gussetfn` passes.
- `devcouncil gusset-check` prints `ok`. That covers the parity vectors, invalid
  UTF-8 refused without poisoning the handle, and a 0x01 frame treated as data
  rather than an opcode.

**Manvi** (surveyed 2026-10-26, read-only, at the checkout whose `main` ref is
`36e2edf`; uncommitted changes in it were not checked) has its own
`manvi gusset-check`, and it is DevCouncil's check behind a thin wrapper:

- `manvi/cmd/manvi/gusset_check.go` calls `gussetcheck.SelfTest`
  (`manvi/gussetcheck/check_cgo.go`), which refuses a build whose
  `gusset.MaxPoolSize` is below 4 and then runs DevCouncil's
  `gussetfn.SelfTest`, the same function the `devcouncil gusset-check` above
  runs. A `CGO_ENABLED=0` build has no engine and returns `ErrNotLinked`
  (`check_nocgo.go`), so a check that could not run does not read as one that
  passed.
- It is the only in-process Go-to-Rust call in Manvi, apart from `Shutdown` and
  `DrainLogs` on the way out of `manvi serve` (`manvi/cmd/manvi/serve.go`). No
  policy decision uses the engine: the gates leave `Matcher` nil, so Go's
  `fnmatch` decides (`manvi/cmd/manvi/gate.go`, `manvi/serve/policy.go`,
  asserted by `manvi/cmd/manvi/policy_matcher_test.go`).
- It is a gate: `verify.sh` runs `go test ./gussetcheck ./serve ./cmd/manvi` and
  `go run ./cmd/manvi gusset-check` with the cgo environment, and
  `scripts/release-build.sh` runs each built binary's `gusset-check` before it
  is accepted. `scripts/module-pins.txt` pins gusset at `320c56c` (2026-10-13),
  142 commits behind this tree.
- **Against the current Gusset: inferred, not run.** Running it means building
  the DevCouncil umbrella and linking cgo, which this survey did not do. From
  source, in this tree: every `gusset.` identifier `gussetfn` uses (`Open`,
  `WithPoolSize`, `WithOpcode`, `ContextWithOpcode`, `Shutdown`, `DrainLogs`,
  `Handle`, `Buffer`, `ErrClosed`, `ErrPanic`, `ErrPoisoned`) and Manvi's own
  `MaxPoolSize` (1024) exist; `internal/ffi/exports.txt` (17 exports) is
  identical at `320c56c`, at GitPulse's pin below and here. Between the pin and
  here the changes to the Go API and the FFI that I read are argument refusals
  that now match `ErrBadArg` (and return `FFI_BAD_ARG` from Rust), a nil `Option`
  refused instead of panicking, and documentation of an engine-registry rule
  that was already enforced at the pin. `internal/ffi/archive_hash.go` became a
  stub that nothing reads, and the cache-busting key the check depends on is
  DevCouncil's own `-DDEVCOUNCIL_GUSSET_ENGINE_SHA256` flag from `cgo-env.sh`,
  not a Gusset file. This is not a full change list: the rest of the 142 commits
  (the `[0.0.3]` sections of `CHANGELOG.md` from 2026-10-23 on, `internal/isolate`,
  `internal/hostprobe`) were not read for effect on a check that touches none of
  them by name, and the Rust diff was skimmed on the pool and FFI-guard paths,
  not audited. None of what I read is on the path of a valid check, and the check
  compares no error text. That predicts `ok`; it does not prove it. The live
  module graph does not use the pin: `manvi/go.mod` replaces gusset with the
  sibling checkout, so the check runs against whatever is checked out beside
  Manvi.

**GitPulse** (surveyed 2026-10-26, read-only, `main` ref `fa89a7e`) has one Go
module and no product code in Go:

- `go/` is the whole Go tree: `go/check.go` (build tag `gusset`),
  `go/check_stub.go`, `go/errors.go` and `go/cmd/gusset-check/main.go`. It has the
  same shape as Manvi's: the `MaxPoolSize` guard, then DevCouncil's
  `gussetfn.SelfTest`. Without the tag it exits 2, "could not run", distinct
  from exit 1, "engine failed". Nothing else imports it.
- Searching `*.go` in both repositories for `import "C"` and `#cgo` finds no hit
  in either, with ignore rules off and scratch and nested-worktree copies
  excluded. Neither repository declares cgo itself: the link to Rust is the
  `gusset` package and DevCouncil's `gussetfn`. GitPulse's application
  (`src-tauri`) is Rust.
- GitPulse reaches Go and Rust components through processes and through Rust
  crates, not through Gusset: `manvi serve` as one long-lived NDJSON-over-stdio
  child (`src-tauri/src/harness/sidecar.rs`), the `devmap` binary resolved and
  spawned by `src-tauri/src/devmap/cli.rs` (`resolve_binary`, `Command::new`),
  and vendored `devmap-query`, `devmap-store` and `devmap-resolve` linked into the
  app (`src-tauri/Cargo.toml`). Its Tauri binary is already a Rust staticlib, so
  linking Gusset into it would be a second one in a process (R14), which
  `go/check.go` records as the reason the check is a separate program.
- CI runs it: the `gusset` job in `.github/workflows/ci.yml` (`go vet` and
  `go test -race` with `-tags gusset`, then `bun run gusset:check`), against
  `GUSSET_PIN` `92c2056` (2026-09-28), 309 commits behind this tree. The check was
  not run here. The same inference as Manvi's applies, because it calls the same
  `gussetfn.SelfTest`.

## What a `devmap` query costs, and who pays it

`dc/devmap` starts the `devmap` binary once per query. It guards each run with a
timeout, an output cap, a process group and `WaitDelay`. Measured per query on
Gusset's own repository, indexed in 0.67 s. These are historical, one-off timings
from 2026-09-27, taken as described under "How the numbers were taken". The
timing script and its raw samples were not committed, so `make docs-check`
cannot regenerate or check this table, and it is not a Gusset benchmark:

| Query | New process each time | One warm process (`devmap mcp`) | Saved |
| --- | ---: | ---: | ---: |
| `neighbors Handle::submit` | 12.5 ms | 0.78 ms | 94% |
| `impact pool/ring.rs` | 11.3 ms | 1.46 ms | 87% |
| `explore submit` | 17.4 ms | 9.7 ms | 44% |
| `search Handle` | 26.2 ms | 17.9 ms | 32% |
| `--version` (start-up floor) | 4.2 ms | — | — |

Starting the process and opening the store cost 8–11 ms per query. The answer
itself costs anywhere from under 1 ms to 18 ms.

### Who pays that cost today: nobody who repeats it

An earlier version of this page recommended pooling warm `devmap` processes in
`dc/devmap`. It assumed agent tool loops went through that client. They do
not. Tracing every Go path in DevCouncil that runs `devmap` (at `71e2cf9`)
found only one-shot uses:

| Go caller | What it runs | How often |
| --- | --- | --- |
| `internal/mapcli` (`dcmap`) through `dc/devmap` | `status` | once or twice per short-lived `dcmap` invocation |
| `dc/devmap` `Search`, `Dead`, `Deps` | — | no production caller, only their own tests |
| `devcouncil map`, `devcouncil ast` | one passthrough command | once per invocation |
| `devcouncil/integrate` | asset and skill installs | once per `integrate` |
| `devcouncil mcp` (the Go MCP server) | — | never runs `devmap` |

Agents reach `devmap` through `devmap mcp`, which the host config written by
`integrate` points at directly. That is one long-lived process per session,
which is exactly the warm column above. DevCouncil's lifecycle hooks are
retired ("MCP-first"). So a pool in `dc/devmap` would have no caller inside
DevCouncil that makes repeated queries, and nothing measured here would get
faster there. It was not built. (Manvi, which imports `dc/devmap`, is such a
caller; see below.)

**What would change that answer:** a long-running Go process that sends
`devmap` many queries. Manvi is one, as surveyed on 2026-10-26 (table below). The
pool would not belong in Manvi's own code: Manvi imports DevCouncil's
`dc/devmap` (`manvi/cmd/manvi/main.go`), and that package's `invoke` is where
every spawn happens, so `dc/devmap` has a repeated-query caller after all. A pool
would be built there. `dc/store/serve.go` is the template: a pool of
long-lived children with length-prefixed argv frames in and a status, a length
and a body out. It needs a matching stdio framing on the Rust side, one that
returns stdout, stderr and the exit status for each request, because the client
parses all three. It is not built, for the reasons under the table.

Who in Manvi starts `devmap`, from a process that stays up:

| Manvi caller | What it runs | How often |
| --- | --- | --- |
| Agent harness (`manvi/cmd/manvi/harness.go`, `manvi/devcouncil/navigate.go`): `graph_query` | one `status`, one `search` | per tool call |
| same: `code_dead` | one `status`, one `dead` | per tool call |
| same: `graph_context` | one `status`, then one `deps` per file, up to 25 files, 5 at a time (`graphContextWorkers`) | per tool call, up to 26 processes |
| `manvi serve` (`manvi/cmd/manvi/serve.go`): scan, verify and the `devmap` module, each building a client per request root | queries including `Advanced`; the verify check sends one per batch of targets (`manvi/serve/verify_checks.go`) | per request |
| `manvi doctor`, `manvi map` | `status`, `build`, `manifest` | once per invocation |

Each of these is a new process: the client builds an `exec.CommandContext` per
call (`dc/devmap/devmap.go`, `invoke`), and no Manvi Go source starts `devmap
mcp` or `devmap serve`. (Manvi's MCP plugin loader, `manvi/mcp/plugin.go`, runs
servers a config declares; whether any config points one at `devmap mcp` was not
checked.) The spawn counts are pinned by tests
(`manvi/devcouncil/navigate_spawn_test.go`), so someone has already cut
`graph_query` from three processes to two and given `graph_context` a worker
pool. The `status` read is deliberately not shared between calls
(`TestGraphStatusIsNotServedAcrossCalls`): a verdict served across calls would
call a stale index fresh. A warm process has to keep that, which means one
`status` per call even though the rest of the query is warm.

What is not known: how often a real session makes these calls, and what share of
a turn they take. Nothing in Manvi or here measures it, and no committed
recording gives a per-start cost to multiply by, so no figure is offered. The
pool is a candidate with a caller, not a recommendation.

**Running `devmap` in-process through Gusset stays the wrong tool** even for
such a caller. On top of a warm process it would save only the pipe round trip,
about 0.1–0.2 ms (an estimate; no measurement of it is recorded here) against a 4 µs Gusset call (the serial no-op in `bench/results/linux-amd64-vm/ring-main-after.txt`, median 3.7 µs). That is under 2% of the fastest
query above. The price would be:

- Giving up `CGO_ENABLED=0`, which `dc/store/store.go` records as a
  requirement.
- Linking tree-sitter and SQLite into every Go binary. The standalone `devmap`
  is 70 MB.
- One umbrella archive per binary that carries every engine (R14).
- Moving the process-isolation guarantees into the harness itself.

## Where Gusset is the right tool

Gusset fits when a Go process makes many calls into Rust, each of a few
microseconds to a few milliseconds, and cgo is already acceptable in that binary:

- Hot per-item work: matching, hashing, parsing or scoring each item of a large
  batch.
- Kernels that vectorize. The multiversioned kernel ran 1 MiB in 46 µs against
  69 µs for Go SIMD on the same host (medians of 6 in `bench/results/linux-amd64-vm/simd-ring-multiversion.txt`; see `docs/choosing.md`).

In those cases the 4 µs round trip and a fixed thread count beat both a process
per call and a blocking cgo call per request. If DevCouncil or Manvi ever needs
Rust-side matching in a batch, the existing `gussetfn` pattern is the starting
point. Batch the whole list into one frame per call (for example `MatchAny` over
N patterns), rather than making N calls.

## What is pending, and what it needs

- **Manvi's `gusset-check` against the current Gusset: surveyed, not run.** Read
  on 2026-10-26 (see Manvi above). It is DevCouncil's `gussetfn.SelfTest` plus a
  `MaxPoolSize` guard, wired into Manvi's `verify.sh` and release build. By
  source it should still print `ok`. To close it, run `manvi gusset-check` from a
  cgo build linked with DevCouncil's umbrella at the Gusset in question, which
  this survey did not do.
- **GitPulse's Go-to-Rust calls: done.** The only one is the `gusset-check`
  program in `go/`; the product has none (see GitPulse above). Its check was not
  run here either.
- **Whether Manvi queries `devmap` from a long-lived Go process: done, yes.** The
  agent harness and `manvi serve` start `devmap` once per query, up to 26
  processes for one `graph_context` call (table above). That changes one earlier
  conclusion: `dc/devmap` has a repeated-query caller through Manvi, so a warm
  pool, if built, belongs in `dc/devmap`. The others stand: DevCouncil's own
  paths are still one-shot, and running `devmap` in-process through Gusset is
  still the wrong tool.
- **Still pending: whether that pool is worth building.** It needs a measurement
  of real Manvi sessions (calls per turn, share of turn time) and the Rust-side
  stdio framing described above. Neither exists. The per-call `status` rule
  limits the saving.
- **The pins in both apps are behind this tree and were not touched.** Manvi's
  `scripts/module-pins.txt` is 142 commits behind and GitPulse's `GUSSET_PIN` 309.
  Moving a pin changes what that repository claims to build against, so it is
  reported here and not changed. Both checks run against the pinned commit in CI,
  so a green CI there says nothing about this tree until the pin moves.
- **Not covered by this survey:** `git` against the sibling repositories was
  refused to the session, so the files read are the sibling working trees and the
  pinned commits were not read with `git show`. DevCouncil's caller trace was
  not redone; this page's trace is still at `71e2cf9`. Only `gussetfn/match.go`
  and `dc/devmap/devmap.go` were read, from the DevCouncil working tree (its
  commit was not recorded).
- **macOS: done.** The completion ring's park path (kqueue under `RawConn.Read`)
  runs on darwin: `bench/results/darwin-arm64-*.txt` is a committed run of the
  Gusset benchmarks there, and `matrix.yml` runs `unit` and `r14`, and `adopter.yml` runs the adopter
  walkthrough on `macos-latest` (see [platforms](platforms.md) for exactly which
  jobs). This item was written before those existed.

## How the numbers were taken

- `devmap` was built from DevCouncil at `71e2cf9` (`cargo build --release -p
  devmap-cli`).
- `devmap build --manifest` was run in a scratch clone of this repository.
- Each spawned query was timed 20–30 times from Python (`subprocess.run`, output
  discarded), and the median is reported.
- For the warm numbers, one `devmap mcp` process answered 30 `tools/call`
  requests per query. The first 3 were dropped, and the median of the rest is
  reported.
- The Gusset round trip is the serial no-op `Call` from
  `bench/results/linux-amd64-vm/ring-main-after.txt`.
