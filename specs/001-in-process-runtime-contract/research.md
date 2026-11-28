# Technical Research & Alternatives

## 1. Options Evaluated

- **Per-call blocking cgo versus submit-and-return.** `AGENTS.md` ("Runtime differences") records that a goroutine blocked in cgo pins an OS thread and that the thread cap is 10,000. The chosen design is submit, then park on a Rust-owned completion ring with a one-token pipe as doorbell, with a Go semaphore bounding in-flight work (R11). The measured crossover against plain cgo is in `docs/choosing.md` and `bench/results/crossover/`.
- **`catch_unwind` placement.** Around each work unit in the worker loop, not only around the submit call, so the loop survives and a dead worker is respawned (`crates/gusset/src/pool/worker.rs`).
- **Recovery after a panic.** `Close` then `Open`; no in-library supervisor (`DECISIONS.md` 2026-10-24).
- **Second Rust staticlib.** Either one umbrella crate or `tools/gussetseal` (R14, `DECISIONS.md` 2026-10-08).
- **Go runtime tricks** (`//go:linkname`, `asmcgocall`, `purego`) were rejected outright (R13).
- **Docs and benchmarks.** Numbers are generated, never typed (`tools/benchdoc`, `make docs`, `make docs-check`; R15). `docs/choosing.md` states that Gusset caps overhead and does not remove it, so calls doing under about 10 microseconds of work belong in Go or raw cgo.

## 2. Benchmarks & Memory Impact

- Committed benchmark evidence lives in `bench/results/` (for example `darwin-arm64-go1.27.2-rust1.99.0.txt`, `crossover/`, `dispatch/`, `linux-amd64-vm/`). This spec quotes no figure from them.
- `make alloc-gate` bounds allocations per call through `tools/ci/benchgate`; the bounds are in the `Makefile` (`ALLOC_BOUNDS`).
- Allocator symmetry: Rust buffers are allocated and freed only through `gusset_buf_alloc` and `gusset_buf_free`, and counted by Gusset directly whether or not `Counting` is the global allocator.

## 3. Navigation Evidence

- Waylode Insights (`waylode_insights`, `waylode_collision_risk` on `Makefile`) answered `ok: true` for one worktree with no collisions. Its `changes` facet counted 17 dirty files, which includes this spec's own new files plus the three pre-existing dirty files (`CONTRIBUTING.md`, `DCO.txt`, `NOTICE`).
- DevMap: `devmap status` reports a built index (generation 1056, 3120 symbols, 298 files) but `source_freshness: false` ("source tree differs from the indexed generation"), and the exported artifacts lag one generation. Paths and symbols cited here were therefore confirmed with `rg` and direct reads, not with the graph. That is a recorded gap, not a graph confirmation.

## 4. Unverified

- **Not run during authoring**: `cargo test --workspace`, `go test ./...` beyond `./tests/ci`, `make ci-local`, and every Linux and sanitizer job. Only `go test ./tests/ci/ -count=1` was run (passed). There is no `target/` directory in this checkout, so the Rust archive was not built.
- **Phase 1 exit criterion** "the Go service in front of tessl/sparsl runs on it" has no record in this repository (`AGENTS.md` Phase status).
- **Phase 2 exit criteria** "both apps green on Linux and macOS" and "tessl/sparsl shows no thread growth under the soak test" have no record here.
- **Public entry-point count**: `AGENTS.md` says 13 Go entry points; `testdata/public_api.txt` pins the full exported surface (71 lines) and does not state a count, so the number 13 was not reconciled and is not used as a metric.
- **Latest CI state on `main`** was not checked (no network access to GitHub was used). The `AGENTS.md` cite of matrix run 40080429800 is a claim, not re-verified.
- `BUILD_REPORT.md`, `AUDIT_REPORT.md` and `BRANCH_REPORT*.md` are git-ignored working notes (`.gitignore` `/*_REPORT*.md`); nothing here relies on them.
