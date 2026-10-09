# Gusset in the other apps: DevCouncil, Manvi, GitPulse

[Documentation Hub](README.md) · [Adoption Guide](adoption.md) · [Benchmarks, Plotted](benchmarks.md)

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

## What a `devmap` query costs, and who pays it

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
retired ("MCP-first"). So a pool in `dc/devmap` would have no caller that
makes repeated queries, and nothing measured here would get faster. It was not
built.

**What would change that answer:** a long-running Go process that sends
`devmap` many queries. The only candidate seen is Manvi, which runs the same
binary (`MANVI_MAP_BINARY`) and could not be inspected. If Manvi does that, the
pool belongs in Manvi. `dc/store/serve.go` is the template: a pool of
long-lived children with length-prefixed argv frames in and a status, a length
and a body out. It needs a matching stdio framing on the Rust side, one that
returns stdout, stderr and the exit status for each request, because the client
parses all three.

**Running `devmap` in-process through Gusset stays the wrong tool** even for
such a caller. On top of a warm process it would save only the pipe round trip,
about 0.1–0.2 ms against a 4 µs Gusset call. That is under 2% of the fastest
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
- **Whether Manvi queries `devmap` from a long-lived Go process.** If it does, a
  warm pool belongs there (see above). DevCouncil has no such caller.
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
