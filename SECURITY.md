# Security Policy

Gusset provides the runtime boundary between Go and Rust in production. Its core mission is maintaining process stability, panic containment, and memory safety.

We take the security and integrity of this boundary seriously.

---

## Supported Versions

| Version | Supported |
| :--- | :--- |
| `v0.0.3` | :white_check_mark: |
| `< v0.0.3` | :x: |

Only the latest release is supported. There are no backport branches: a fix lands on `main` and ships in the next release, so the remedy for an older version is to upgrade. v0.0.2 and earlier contain the `WaitBuffer` write-after-free fixed in v0.0.3 (`CHANGELOG.md`, `[0.0.3]`, Security).

`tools/ci/lint.sh version-parity` fails when the supported row above disagrees with the version in `Cargo.toml`.

---

## Reporting a Vulnerability

If you discover a security vulnerability—especially concerning:
- A panic payload or format string that escapes `ffi_guard_code` and triggers `SIGABRT` or process crash.
- A memory corruption, use-after-free, or double-free across the FFI boundary.
- A Go pointer retention bug violating cgo pointer passing rules (`cgocheck`).
- A race condition in semaphore accounting or completion ticket dispatch that could lead to deadlock or unbounded thread exhaustion.

**Please do NOT report security vulnerabilities through public GitHub issues.**

Instead, report vulnerabilities privately via:
1. **GitHub Security Advisory:** Submit an advisory on the [Gusset Security tab](https://github.com/bharathvbcr/gusset/security/advisories/new).
2. **Email:** Contact the maintainer directly at `bharathchandra18@gmail.com`.

Please include:
- A clear description of the vulnerability.
- A minimal reproducible example (e.g. a failing test case in Go or Rust).
- The operating system, architecture, Go version, and Rust version used.

---

## Response Process

- We will acknowledge receipt of your vulnerability report within 48 hours.
- We will provide a detailed assessment and proposed mitigation timeline.
- A fix ships with a test that fails without it. It is merged to `main` with `Gusset Matrix CI` green, and the patched release is tagged on a `main` commit whose `Gusset Matrix CI` run is green. We then publish a GitHub security advisory naming the affected and patched versions and the fix commit.
- What that gate runs is listed below. AddressSanitizer over the test suites, Miri and the long fuzz runs are nightly jobs on `main`. They are not a precondition of the tag; a failing nightly opens an issue.

---

## What CI checks, and where

From `.github/workflows/`. Linux means GitHub's Ubuntu runners on glibc unless a job says musl. AddressSanitizer never runs on macOS: Go's `-asan` is Linux-only, and every Rust ASan leg runs on a Linux runner. On macOS, `-race` runs only in `allocator-api` and the `r14` harness.

**Every push to `main` and every pull request into it** (`Gusset Matrix CI`, `matrix.yml`; this is the release gate):

| Job | Platforms | What runs |
| :--- | :--- | :--- |
| `unit` | Linux x86-64, Linux arm64, macOS arm64 | `cargo clippy --all-targets -D warnings`; `cargo test --workspace` in dev and release; `go test ./...`, and again under `GOEXPERIMENT=simd` |
| `race` | Linux | `go test -race` over the root package, `tests/pitfalls`, `tests/panic_zoo` and `internal`; the same packages under `GOGC=1`, 5 times |
| `cgocheck2` | Linux | `GOEXPERIMENT=cgocheck2 go test ./...` |
| `soak` | Linux, macOS | every Soak test: the OS-thread cap and an empty `goroutineleak` profile after drain |
| `memlimit` | Linux | allocator stats and `AdviseMemoryLimit` under `GOMEMLIMIT=256MiB` |
| `allocator-api` | Linux, macOS | the Rust 1.100+ allocator surface: Rust and Go suites, `go test -race` interop stress, and the fallback build on the same compiler |
| `go-floor`, `rust-floor` | Linux | `go test ./...` on Go 1.26; clippy, `cargo test` and `go test ./...` on Rust 1.97 |
| `r14`, `r14-musl` | Linux x86-64 and arm64, macOS arm64 and x86-64 (Rosetta), musl | the sealed second-staticlib harness. Its binaries run under `-race`, `cgocheck2`, Go `-asan` and with Gusset and both engines built `-Zsanitizer=address`; the two ASan scenarios run on Linux glibc only |
| `musl` | Linux musl (Alpine) | the Go tests against a musl build, and the worker stack size read-back |
| `cross` | Linux | `cargo check` for every supported target |
| `fuzz` | Linux | every Go native fuzz target for 45 s (`make fuzz`) |
| `lint` | Linux | `tools/ci/lint.sh`: gofmt, go vet, gussetvet, staticcheck, govulncheck, the forbidden-pattern audit, version parity, clippy, cargo fmt, cargo-deny |
| `bench` | Linux | interleaved benchmark comparison against the base revision; README numbers checked against `bench/results` |

The adopter walkthrough (`adopter.yml`, Linux and macOS) also runs on every push to `main` and every pull request.

**Daily on `main`** (`Gusset Nightly Sanity`, `nightly.yml`, all Linux x86-64):

| Job | What runs |
| :--- | :--- |
| `sanitizer` | AddressSanitizer: nine Rust integration test files built `-Zsanitizer=address` (runtime, shutdown, null status, bounded engine errors, drain logs, counting buffers, allocator API, allocator counting, SIGPIPE), and `go test -asan` asserting that R4's deliberate cross-free is detected. The Go suite as a whole does not run under ASan |
| `miri` | `cargo miri test -p gusset --lib` (tests that need `pipe(2)` are ignored under Miri), then a 32-seed sweep of `pool::ring` |
| `hammer` | the chaos hammer for 5 minutes under `-race` and 5 without, both with `GOGC=1` |
| `hammer-heap` | the hammer with glibc heap checks (`GLIBC_TUNABLES=glibc.malloc.tcache_count=0:glibc.malloc.perturb=165`), then with Gusset built `-Zsanitizer=address` linked into a `go test -asan` binary |
| `cargo-fuzz` | five libFuzzer targets over the Rust boundary, built with cargo-fuzz's default AddressSanitizer: four for 300 s each, and `ffi_exports` (the raw C exports) as five 60 s processes |
| `go-fuzz` | every Go native fuzz target for 240 s |
| `step5-snippet` | the adopter walkthrough on the pinned nightly Rust, requiring the step 5 Allocator snippet to compile |

**Weekly** (`tip.yml`, Linux): `go test ./...` and the Soak tests on a Go toolchain built from tip.
