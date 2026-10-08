# RFC: more than one Rust staticlib per Go binary (R14)

Status: **accepted**, `DECISIONS.md` 2026-10-08. Source: GP-FEAT-003.

R14 used to read: *exactly one Rust `staticlib` per Go binary; adopters with
several engines build an umbrella crate* whose `[lib] name` is `gusset`. This RFC
asked whether that can be relaxed, and under what rule. The answer is yes, for
engines that are **sealed** before the final link. Gusset's own runtime stays a
single, unsealed archive.

Everything marked *observed* was produced by `bench/r14/run.sh` on 2026-10-08,
and the `r14` CI job re-runs it on every commit. These numbers are not generated
under R15, and none of them belongs in the README.

---

## 1. What actually collides

A Rust `staticlib` is self-contained. It bundles the crate together with its
own copy of `core`, `alloc`, `std`, the panic runtime and the allocator shim.
What happens when two of them meet depends on how they were built.

**Under fat LTO** (Gusset's release profile), LTO has internalized almost all of
std. A few names stay global, and the link fails on them (*observed*: two
independent engines plus `libgusset.a`):

| Platform | Duplicate symbols |
| --- | --- |
| darwin/arm64, Apple ld | `_rust_eh_personality`, `std::panicking::EMPTY_PANIC` |
| linux/arm64 glibc, GNU ld 2.40 | `rust_eh_personality`, `std::panicking::EMPTY_PANIC`, `std::sys::args::unix::imp::ARGV_INIT_ARRAY` |
| linux/arm64 musl (Alpine) | `rust_eh_personality`, `std::panicking::EMPTY_PANIC` |

**Without LTO**, with both archives from one toolchain, the link **succeeds**
(*observed* on all six targets in §4). Both archives carry std members with
identical mangled names, and the linker pulls one copy for both engines. Nothing
reports an error, yet the engines now share one std. Engine B's panic hook
counted 1,872 panics against its own 928, because it also saw engine A's. And
A's counting `#[global_allocator]` ended at 9,448 live bytes, against 64 when
the engines are sealed, because B's allocations were landing in it.

So the old R14 was enforced only by an accident of the release profile. Where
the guard fired, it fired loudly; where it didn't, the failure was silent. The
duplicate names also only *name* the problem: the cause is two copies of std in
one symbol namespace. Localizing just those names fixes the case, not the class,
and on ELF it still crashes (§3).

## 2. Options

| | Option | Verdict |
| --- | --- | --- |
| A | **Umbrella crate**: one staticlib, engines as `rlib` dependencies, dispatched by `register_engine(opcode, …)` | **The default.** Zero cost, one std, one allocator. It needs every engine buildable from source by one toolchain in one Cargo graph |
| B | **Sealed staticlibs**: each extra engine partially linked into one object, with only its own C ABI left global | **Adopted** for engines that cannot join the umbrella (§3) |
| C | `cdylib` per engine, `dlopen(RTLD_LOCAL)` | Rejected. It brings back what static linking removed: loader paths, the macOS signing pitfall (`docs/dylib.md`), and a deploy that is no longer one file |
| D | Rust `dylib` with a shared std (`-C prefer-dynamic`) | Rejected. No stable Rust ABI, so every engine must be built in lockstep: the umbrella's constraint without its simplicity |
| E | Out of process (Phase 4, `docs/ipc.md`) | Right answer when an engine needs crash isolation, its own lifecycle or its own deploy. Orthogonal to R14 |
| F | Two Gusset runtimes in one process, one per engine | Rejected (§5) |

Option A already gives *source-level* modularity: separate crates, separate
owners, one opcode each. Option B is for what A cannot express: a vendor archive
with no source, an engine pinned to a different toolchain, or an engine whose
release cadence must not rebuild the umbrella.

## 3. Sealing

`go run ./tools/gussetseal -prefix PREFIX -o OUT.a IN.a` does these things:

1. **One object.** It partially links every member of the archive into one
   relocatable object: `cc -arch <the archive's> -r -Wl,-force_load` on Mach-O,
   `ld -r --whole-archive` on ELF. References between the engine and *its* std
   resolve here, privately. On darwin the architecture is read from the archive
   (`lipo -archs`). Left to default, the driver assumes the host's, and an
   x86_64 archive sealed on arm64 came out with nothing in it. A universal
   archive is refused: each slice needs its own seal.
2. **One export surface.** It makes every symbol except `PREFIX*` local:
   `-exported_symbols_list` on Mach-O, `objcopy --keep-global-symbol` on ELF.
   References to libc, pthreads and the system unwinder stay undefined and
   resolve at the final link.
3. **ELF: every COMDAT group renamed for this engine.** Every Rust CIE reaches
   its personality routine through `DW.ref.rust_eh_personality`, which sits in a
   COMDAT group of that name. Making the symbol local does **not** rename the
   group. The final linker keeps the first group of each name, so a second
   engine's unwind tables point into a discarded section. Its first panic jumps
   to a garbage address and the process dies, with or without a Go traceback.
   That is the trap in "just `objcopy --localize`" recipes. The tool renames
   every group, not only this one: ASan-instrumented code adds
   `asan.module_ctor` and `asan.module_dtor` groups, and any shared group is the
   same hazard.
4. **Commons renamed for this engine, except the image-wide ones.** A common
   symbol has no section, so `objcopy` silently leaves it global, and the final
   link merges same-named commons across engines. On ELF each one is renamed to
   `<name>.<PREFIX>`, a name only this engine has. On Mach-O, ld64 leaves a
   common *private external*, which still binds across object files, and has no
   `-d` to give it storage, so the seal refuses. Rust emits commons only under
   sanitizer instrumentation, and Go has no `-asan` on darwin.

   One common is meant to merge: ASan's `___asan_globals_registered`. Every
   instrumented module's constructor calls `__asan_register_elf_globals` with
   that flag and the linker's `__start_asan_globals`/`__stop_asan_globals`
   range, which spans every instrumented global in the binary. The flag is
   what makes the range register once. Given one flag per engine, each engine
   registered every global again, Gusset's included, and ASan aborted with an
   `odr-violation` on `gusset::ffi::alloc::LIVE_BYTES`. The seal leaves that
   common alone, so it merges with the unsealed archive's own, exactly as the
   crates inside one archive already share it.
5. **ELF: drop `.llvmbc`/`.llvmcmd`.** `ld -r` concatenates the copies a fat-LTO
   staticlib inherits from the prebuilt std rlibs into one invalid bitcode
   blob. When the LLVMgold plugin is installed, binutils' `ar` aborts on it with
   `LLVM ERROR: Invalid encoding`.

It then **re-reads the object** with `debug/elf` or `debug/macho`, not trusting
any tool's exit status, and writes the archive only when all of these hold:
nothing outside `PREFIX*` is global (a private external counts as global), no
COMDAT group or common could be shared beyond the image-wide list, at least one
`PREFIX*` symbol exists, and the prefix does not claim `gusset` or Rust's own
names (`rust_`, `__rust`, `_R`, `_ZN`). After `ar` has run, it **re-reads the
archive** too: its only member must be byte-identical to the object that passed,
or the archive is deleted. The object check runs standalone as `-verify OBJ.o`.

`gussetseal` runs on darwin and Linux, which is Gusset's whole supported set
(`docs/platforms.md`). Gusset is unix-only and fails its own build on Windows,
so there is no Windows binary to seal an engine beside.

## 4. The demonstration

`bench/r14/run.sh` links two engines, each a standalone `staticlib`, into one Go
binary **together with `libgusset.a`, unsealed**:

- **Engine A** has its own counting `#[global_allocator]`, a worker thread per
  call, a caught `panic!`, and allocator and thread-local probes.
- **Engine B** uses the default allocator, panics with a `u32` payload, installs
  a counting panic hook, and has the same probes.

The host runs 32 goroutines × 200 iterations into both engines (every seventh
call panics in each), while it drives Gusset's echo and panic paths: 16 handles,
each expected to return `ErrPanic` carrying `plain panic` and then `ErrPoisoned`.
It then runs two pinned checks:

- **Thread-locals.** On one OS thread it alternates 200 panics between the
  engines. Each engine's thread-local counter must advance exactly once per
  round, and neither copy may still think the thread is unwinding.
- **Allocators.** It holds 64 MiB in B, which must not move A's counter or
  Gusset's `Stats().LiveBytes`. It then holds 64 MiB in A, which A must count.

Columns are the target, and how it ran on an Apple M-series host: natively,
under Rosetta, or in podman containers (arm64 native, x86-64 under qemu-user).
For x86-64 glibc, cargo cross-built the archives natively and `run.sh check` ran
emulated, because rustc crashes under qemu-user in the Debian image. The x86-64
musl leg ran whole under emulation.

| Scenario | Expectation | darwin arm64 | darwin x86-64 (Rosetta) | linux-gnu arm64 | linux-gnu x86-64 (qemu) | linux-musl arm64 | linux-musl x86-64 (qemu) |
| --- | --- | --- | --- | --- | --- | --- | --- |
| naive (fat LTO) | link fails on `rust_eh_personality` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| nocomdat: localize-only seal | `gussetseal -verify` refuses it; the binary crashes on a panic | n/a: no COMDAT in Mach-O | n/a | ✅ | ✅ | ✅ | ✅ |
| sealed, 3 runs | all checks above pass | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| sealed, `-race` | pass | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| sealed, `GOEXPERIMENT=cgocheck2` | pass | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| sealed, `go build -asan` | pass | n/a: Go `-asan` is linux-gnu only | n/a | ✅ | not run: ASan aborts under qemu-user | n/a | n/a |
| asan-rust: Gusset and both engines `-Zsanitizer=address` | pass, exactly one `___asan_globals_registered` | n/a | n/a | ✅ | not run: as above | n/a | n/a |
| nolto naive | links, then **fails** the isolation checks | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| nolto sealed | pass | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| skew: B on Rust 1.97, A and Gusset on 1.99 | pass | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| `-linkmode=internal` | fails, on `gusset_*` as well | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |

All cells are *observed*, on the final code. Linux arm64 glibc ran four times,
three of them on a tree shared with the macOS host. `tools/gussetseal`'s tests,
eight on each platform with no skips, pass on all six.

To reproduce, run `bench/r14/run.sh`, or `R14_TARGET=<triple> bench/r14/run.sh`
for another target, adding `GOARCH` when the architecture differs. The `r14`
CI job runs it on linux x86-64 and arm64 and on macos arm64 and x86-64
(Rosetta); `r14-musl` runs it on Alpine x86-64. Both fail on any skipped test or
scenario.

## 5. What a sealed engine shares, and what it does not

| | Per sealed copy | Shared across the process | Evidence |
| --- | --- | --- | --- |
| Panic runtime, payloads, `catch_unwind` | ✅ | | concurrent panics in all three archives |
| Panic hook (`set_hook`) | ✅ | | B's exact hook count; the nolto naive link breaks it |
| Personality routine | ✅ (local, plus a renamed `DW.ref` group on ELF) | | nocomdat control |
| Unwinder (`_Unwind_*`) | on musl ✅ (std bundles libunwind, and the seal localizes it) | on glibc and darwin ✅ (libgcc_s / libSystem, language-agnostic, finds FDEs by PC) | musl and glibc legs |
| `#[global_allocator]` | ✅ | | allocator check: B's 64 MiB invisible to A and Gusset |
| `thread_local!`, std's panic count | ✅ | | thread-local check: 200 alternating panics on one OS thread |
| libc heap, pthreads, signal dispositions, fds | | ✅ | the system's |
| stdout/stderr buffering | ✅ (separate `LineWriter`s on one fd) | the fd | *inferred*: output interleaved across engines is not line-atomic. Gusset logs to its ring, not stdout |

Rules that follow, all extensions of rules that already exist:

1. **Only the C ABI crosses a seal (R3, R4).** A `String`, `Vec`, `Box`, trait
   object or panic payload made by one copy is meaningless to another: different
   allocator, and possibly a different layout under toolchain skew. Memory goes
   back to the copy that allocated it, through that engine's own `*_free`.
2. **N heaps, one limit.** `gusset.Stats()` and `AdviseMemoryLimit` see only the
   allocator in `libgusset.a`. A sealed engine's heap is invisible to them, as
   Rust's is to `GOMEMLIMIT` ("Two heaps, one limit" in the pitfall catalogue).
   The adopter exports the engine's own counter and adds it in.
3. **A sealed engine has no Gusset firewall** unless its work is submitted
   through Gusset by an umbrella engine. It must catch its own panics at its C
   ABI.
4. **Unique prefixes.** Two engines exporting the same name is an ordinary
   duplicate-symbol link error: loud, not silent.
5. **Native objects only.** `-C linker-plugin-lto` archives are bitcode, which
   `objcopy` cannot seal.
6. **Size.** Each sealed engine carries its own std. *Observed* on darwin/arm64
   with fat LTO: sealed A and B are 321 KB and 303 KB of text, while the same two
   engines in one umbrella total 324 KB. Roughly 300 KB per extra engine.
7. **External linking.** Go's internal linker never reads the `-l` archives, so
   `-linkmode=internal` fails on Gusset's own symbols before any engine's. That
   requirement already existed.

## 6. Why not a second Gusset runtime (option F)

A seal keeps `PREFIX*` global, and an engine built on Gusset exports `gusset_*`,
so two Gusset-based engines collide on all 17 exports however they are sealed.
`gussetseal` refuses a `gusset` prefix for exactly this reason. Getting past
that would take:

- renaming the exports per runtime,
- one Go binding per runtime, because `internal/ffi` binds the fixed `gusset_*`
  names and is one package per build, and
- a meaning for the process-wide entry points (`Shutdown`, `Stats`, `Threads`,
  `DrainLogs`) when several runtimes sit behind them.

That is a new public surface (AGENTS.md: 17 exports, 13 Go entry points), and
the need is already met: one Gusset runtime dispatches to many engines by
opcode. Option F was neither built nor measured.

## 7. Not verified

Each of these is a check that did not run. None of them passed.

- **ASan on linux-gnu x86-64.** ASan's allocator aborts under qemu-user on this
  host even for a four-line C `malloc`/`free` and a plain `go build -asan`
  binary (`sanitizer_allocator_primary32.h:292 … kNumPossibleRegions`), so the
  `asan` and `asan-rust` scenarios did not run on that architecture here. They
  run natively on linux-gnu arm64, and the `r14` job's `ubuntu-latest` leg runs
  them on x86-64.
- **The `r14` and `r14-musl` CI jobs themselves** have not run on GitHub yet.
  Their steps are the commands above, `actionlint` passes, and every target
  they cover was run locally as described.
- **Windows** is not a gap: Gusset fails its own build there
  (`docs/platforms.md`), so no binary exists to seal an engine beside.
