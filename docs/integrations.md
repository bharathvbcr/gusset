# Gusset in the other apps: DevCouncil, Manvi, GitPulse

Status as of 2026-09-27. The numbers were measured on a Linux VM and are listed at
the end.

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

**Manvi** has its own `gusset-check`, according to DevCouncil's `gussetfn` comment.
It also runs the `devmap` binary (`MANVI_MAP_BINARY`). Its repository could not be
attached to the session that wrote this page, so nothing in it has been checked.

**GitPulse** could not be attached either. Nothing here is known about it.

## Where a deeper integration would pay: `devmap` queries

`dc/devmap` starts the `devmap` binary once per query. It guards each run with a
timeout, an output cap, a process group and `WaitDelay`. Measured per query on
Gusset's own repository, indexed in 0.67 s:

| Query | New process each time | One warm process (`devmap mcp`) | Saved |
| --- | ---: | ---: | ---: |
| `neighbors Handle::submit` | 12.5 ms | 0.78 ms | 94% |
| `impact pool/ring.rs` | 11.3 ms | 1.46 ms | 87% |
| `explore submit` | 17.4 ms | 9.7 ms | 44% |
| `search Handle` | 26.2 ms | 17.9 ms | 32% |
| `--version` (start-up floor) | 4.2 ms | — | — |

Starting the process and opening the store cost 8–11 ms per query. The answer
itself costs anywhere from under 1 ms to 18 ms.

There are three ways to remove that start-up cost.

1. **Keep a pool of warm `devmap` processes, as `dc/store` already does for
   `dcstore`.** This removes the 8–11 ms for every query and keeps
   `CGO_ENABLED=0`. `dc/store/store.go` records that as a requirement: the Go side
   was chosen for a single static binary and simple cross-compilation. The work is
   a `devmap` framing like `dcstore serve` has (length-prefixed argv in, a status,
   a length and a body out), or a client for the existing `devmap serve` socket.
   **This is the recommendation.**
2. **Run `devmap` in-process through Gusset.** On top of option 1 this saves only
   the pipe round trip. A Gusset call costs about 4 µs, against roughly 0.1–0.2 ms
   of JSON over a pipe. That is under 2% of even the fastest query above. The price
   would be:
   - Giving up `CGO_ENABLED=0`, which reverses a recorded DevCouncil decision.
   - Linking tree-sitter and SQLite into every Go binary. The standalone `devmap`
     is 70 MB.
   - One umbrella archive per binary that carries every engine (R14).
   - Moving the process-isolation guarantees (a crash or runaway query cannot take
     the harness down) into the harness itself.

   It is not worth that for queries that take milliseconds.
3. **Leave it as it is.** This is fine for hooks and one-off CLI use, which run
   once per session. It is the wrong choice for agent tool loops, which run many
   queries per task.

## Where Gusset is the right tool

Gusset fits when a Go process makes many calls into Rust, each of a few
microseconds to a few milliseconds, and cgo is already acceptable in that binary:

- Hot per-item work: matching, hashing, parsing or scoring each item of a large
  batch.
- Kernels that vectorize. The multiversioned kernel ran 1 MiB in 46 µs against
  69 µs for Go SIMD on the same host (see `docs/choosing.md`).

In those cases the 4 µs round trip and a fixed thread count beat both a process
per call and a blocking cgo call per request. If DevCouncil or Manvi ever needs
Rust-side matching in a batch, the existing `gussetfn` pattern is the starting
point. Batch the whole list into one frame per call (for example `MatchAny` over
N patterns), rather than making N calls.

## What is pending, and what it needs

- **Access to GitPulse and Manvi.** Both need to be attached to a session, which
  this one was not permitted to do. Then check Manvi's `gusset-check` against the
  current Gusset the same way DevCouncil's was checked, and survey GitPulse for
  Go-to-Rust calls.
- **A decision on the `devmap` pool.** It is a DevCouncil change (and a Manvi one,
  since Manvi runs the same binary). It belongs in DevCouncil's own workflow, with
  its policy tooling available. It is not a Gusset change.
- **macOS.** The completion ring's park path (kqueue under `RawConn.Read`) has only
  run on Linux. `make cross` compiles the Rust side for `aarch64-apple-darwin`.
  CI's `macos-latest` lane is the first real run of it.

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
