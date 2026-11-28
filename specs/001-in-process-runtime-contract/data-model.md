# Data Model

## 1. Entities & Types

No database. The entities are the `#[repr(C)]` types that cross the boundary, the Go types that wrap them, and the status codes. Layouts are asserted statically in Rust and re-checked by Go at initialisation (R12).

| Entity | Defined in | Notes |
| :--- | :--- | :--- |
| `CallHeader` | `crates/gusset/src/header.rs`, `internal/ffi/gusset.h` | 40 bytes per `AGENTS.md`; carries the opcode, flags, trace ids and a relative `timeout_ns`. Absolute monotonic times never cross. |
| `FfiStatus` | `crates/gusset/src/ffi/status.rs` | 48 bytes per `AGENTS.md`; code plus a Rust-allocated message freed only with `gusset_status_free`. |
| `AbiLayout` | `crates/gusset/src/ffi/fields.rs` | 36 bytes, ABI version 2; `gusset_abi_fields` returns the named field offsets separately so an older caller is not overwritten. |
| `AllocStats` | `crates/gusset/src/alloc.rs`, `stats.go` | live bytes, peak bytes, allocation count. |
| `RingShared`, `Slot` | `crates/gusset/src/pool/ring.rs` | Completion ring records; C never declares them, Go reads them through the `GUSSET_RING_*` offsets checked by `tests/rust/constants_match.rs`. |
| `Handle`, `Buffer`, `Error` | `handle.go`, `buffer.go`, `internal/ffi/ffi.go` | Go-side lifecycle objects; the exported surface is pinned in `testdata/public_api.txt`. |

## 2. Status codes (`internal/ffi/gusset.h`)

`FFI_OK` 0, `FFI_ERR` 1, `FFI_PANIC` 2, `FFI_POISONED` 3, `FFI_BAD_ARG` 4. A ticket that completes with `FFI_PANIC` poisons its handle (R10).

## 3. Invariants

- I1: memory is freed by the allocator that created it, through an exported `*_free`.
- I2: no Rust panic crosses the boundary; a caught panic poisons the handle.
- I3: a call cannot outlive its deadline by more than one work unit.
- I4: in-flight calls per handle never exceed the pool size.
- I5: heavy Rust work runs on Rust-spawned threads with explicit stack size.
- I6: Go initialisation refuses an ABI version, struct size or named-field offset mismatch.

Source: `docs/PLAN.md` lines 53-58.

## 4. Serialisation

Completion records are native-endian `u64` words: word 0 is the ticket (bit 63 flags an inline result), word 1 the result length, then the result bytes padded to 8 (`GUSSET_FLAG_INLINE_COMPLETION` in `internal/ffi/gusset.h`). Inline results are at most 48 bytes; larger results are fetched with `gusset_take`.
