#ifndef GUSSET_H
#define GUSSET_H

/* Generated C bindings for Gusset Go-Rust runtime contract. */

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define FFI_OK 0
#define FFI_ERR 1
#define FFI_PANIC 2
#define FFI_POISONED 3
#define FFI_BAD_ARG 4

/* CallHeader.flags bits. Bit 0 opts a submission into the built-in diagnostic
 * engine, which selects behaviour from the first input byte. Production callers
 * never set it, so untrusted payload data cannot steer a call into a panic. */
#define GUSSET_FLAG_DIAGNOSTIC_ENGINE 1u
/* Bit 1: the caller reads completion records, not bare 8-byte tickets. With it
 * set, a successful result of at most GUSSET_INLINE_RESULT_MAX bytes arrives in
 * the pipe with its ticket and is never stored for gusset_take. Every record is
 * a whole number of native-endian u64 words, written atomically:
 *   word 0: ticket, with GUSSET_INLINE_RECORD_FLAG set if a result follows
 *   word 1: result length n (only when the flag is set)
 *   then n result bytes, zero-padded to a multiple of 8
 * Without the flag in word 0 the record is that one word, a bare ticket: take
 * the outcome with gusset_take as usual. A host that leaves this bit clear on
 * every submission only ever sees bare tickets. The library may deliver a
 * bare ticket for any job, including when the pipe could not be grown to hold
 * pool_size records of GUSSET_INLINE_RECORD_MAX bytes. */
#define GUSSET_FLAG_INLINE_COMPLETION 2u
#define GUSSET_INLINE_RECORD_FLAG (1ull << 63)
#define GUSSET_INLINE_RESULT_MAX 48u
#define GUSSET_INLINE_RECORD_MAX 64u

/* Limits and encodings a C host must honour. tests/constants_match.rs checks
 * each against the Rust constant and the Go constant that mirror it. */

/* Largest pool gusset_handle_open accepts; larger is refused, not clamped. The
 * completion pipe must be able to hold 8 bytes per worker (grown on Linux), and
 * GUSSET_INLINE_RECORD_MAX per worker for inline completions to be used. */
#define GUSSET_MAX_POOL_SIZE 1024u
/* Largest inline input gusset_submit copies; larger returns FFI_BAD_ARG, as
 * does a NULL input_ptr with a nonzero input_len. Use a buffer instead. */
#define GUSSET_MAX_INLINE_INPUT 4096u
/* Largest single Rust-owned buffer. */
#define GUSSET_MAX_BUFFER_BYTES 1073741824ull
/* Set on gusset_take's out_buf_id when the id is the result's own buffer,
 * which the caller frees (gusset_buf_free) once it has consumed the bytes.
 * Clear the bit before using the id. Ids are always below this bit. */
#define GUSSET_TAKE_OWNED_FLAG (1ull << 63)

typedef struct {
    uint8_t trace_id[16];
    uint8_t span_id[8];
    uint64_t timeout_ns;
    uint32_t flags;
    uint32_t reserved;
} CallHeader;

typedef struct {
    int32_t code;
    uint8_t* msg;
    size_t msg_len;
    const uint8_t* file;
    size_t file_len;
    uint32_t line;
} FfiStatus;

/* Sizes and aligns are ordered: CallHeader, FfiStatus, AbiLayout, AllocStats. */
typedef struct {
    uint32_t version;
    uint32_t sizes[4];
    uint32_t aligns[4];
} AbiLayout;

typedef struct {
    size_t live_bytes;
    size_t peak_bytes;
    size_t alloc_count;
} AllocStats;

typedef struct GussetHandle GussetHandle;

void gusset_abi_layout(AbiLayout* out);
/* Named-field offsets and sizes of the four structs above, in declaration
 * order: CallHeader, FfiStatus, AbiLayout, AllocStats. Writes min(cap, count)
 * entries into each non-null pointer and returns the full field count. */
uint32_t gusset_abi_fields(uint32_t* offsets, uint32_t* sizes, uint32_t cap);
int32_t gusset_init(void);
int32_t gusset_shutdown(uint32_t drain_ms);
int32_t gusset_handle_open(uint32_t pool_size, int32_t pipe_write_fd, GussetHandle** out_handle, FfiStatus* status);
int32_t gusset_handle_close(GussetHandle* handle, FfiStatus* status);
int32_t gusset_submit(GussetHandle* handle, const CallHeader* header, const uint8_t* input_ptr, size_t input_len, uint64_t buffer_id, uint64_t* out_ticket, FfiStatus* status);
int32_t gusset_take(GussetHandle* handle, uint64_t ticket, uint64_t* out_buf_id, uint8_t** out_ptr, size_t* out_len, FfiStatus* status);
int32_t gusset_cancel(GussetHandle* handle, uint64_t ticket, FfiStatus* status);
int32_t gusset_cancel_all(GussetHandle* handle, FfiStatus* status);
void gusset_status_free(FfiStatus* status);
void gusset_alloc_stats(AllocStats* out);
void gusset_drain_logs(uint8_t* buf, size_t len, size_t* out_written);
int32_t gusset_buf_alloc(GussetHandle* handle, size_t len, uint64_t* out_id, uint8_t** out_ptr, FfiStatus* status);
int32_t gusset_buf_free(GussetHandle* handle, uint64_t id, FfiStatus* status);

#ifdef __cplusplus
}
#endif

#endif /* GUSSET_H */
