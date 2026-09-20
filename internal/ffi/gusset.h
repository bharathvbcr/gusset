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

typedef struct {
    uint32_t version;
    uint32_t sizes[3];
    uint32_t aligns[3];
} AbiLayout;

typedef struct {
    size_t live_bytes;
    size_t peak_bytes;
    size_t alloc_count;
} AllocStats;

typedef struct GussetHandle GussetHandle;

void gusset_abi_layout(AbiLayout* out);
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
