# Contract: Gusset C ABI (ABI layout version 2)

Source of truth: `internal/ffi/exports.txt` (the export list), `internal/ffi/gusset.h` (the hand-maintained header), `crates/gusset/src/ffi/` (the implementation). `tests/rust/exports_match.rs` diffs the built archive's symbol table against the list; `tests/rust/header_match.rs` and `tests/rust/constants_match.rs` diff the header and constants.

## Exports (exactly 17)

| Export | Role |
| :--- | :--- |
| `gusset_abi_layout` | Version, size and alignment of every ABI struct; checked by Go at initialisation (R12). |
| `gusset_abi_fields` | Named field offsets, returned separately so `AbiLayout` stays 36 bytes. |
| `gusset_init` | One-time process initialisation; installs the panic hook. |
| `gusset_shutdown` | Process-global, one-way drain. |
| `gusset_handle_open` | Creates a handle: pool size, completion pipe write descriptor. Refuses a pool above `GUSSET_MAX_POOL_SIZE` with `FFI_BAD_ARG`. |
| `gusset_handle_close` | Destroys a handle. |
| `gusset_submit` | Submits a job from a `CallHeader` and an input of at most `GUSSET_MAX_INLINE_INPUT` bytes (copied before return, R16). |
| `gusset_take` | Takes a stored result; ownership rule is stated in the header. |
| `gusset_cancel` | Sets one job's atomic cancel flag. |
| `gusset_cancel_all` | Sets every cancel flag on a handle. |
| `gusset_handle_ring` | Exposes the completion ring to the Go reader. |
| `gusset_ring_release` | Releases the ring reference. |
| `gusset_status_free` | Frees a Rust-allocated status message (the only way, R4). |
| `gusset_alloc_stats` | Reads `AllocStats`. |
| `gusset_drain_logs` | Drains the bounded Rust log ring into Go. |
| `gusset_buf_alloc` | Allocates a Rust-owned buffer; length 0 or above `GUSSET_MAX_BUFFER_BYTES` is `FFI_BAD_ARG`. |
| `gusset_buf_free` | Frees a Rust-owned buffer. |

## Status codes

`FFI_OK` 0, `FFI_ERR` 1, `FFI_PANIC` 2, `FFI_POISONED` 3, `FFI_BAD_ARG` 4.

## Rules a caller must honour

- Strings and buffers cross as `(ptr, len)`; no NUL termination (R3).
- Go pointers are valid for the duration of the call only (R6).
- Out parameters are written only on `FFI_OK`.
- Each cgo import of an export carries `#cgo noescape` and `#cgo nocallback`; no export calls back into Go (R5).
- Memory is freed by the side that allocated it, through the matching `*_free` export (R4).
- Adding an export needs a row in `DECISIONS.md` and an edit to `exports.txt`, which the tests above then enforce.
