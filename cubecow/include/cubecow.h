/*
 * Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0
 */
/*
 * cubecow C FFI — public header
 *
 * cubecow is an xfs-reflink-based copy-on-write storage engine that
 * exposes thin-provisioned volume management and O(1) snapshots through
 * FICLONE. This header is the C-language counterpart of
 * `src/ffi.rs`; link against the `cubecow` cdylib / staticlib produced
 * by `cargo build --release`.
 *
 * Conventions:
 *   - Engine handle: opaque void* pointer (CubecowEngineHandle)
 *   - Return values: 0 on success, negative error code on failure
 *   - String outputs: heap-allocated C strings owned by cubecow;
 *     callers MUST free them with cubecow_free_string()
 *   - Errors: thread-local last_error retrieved with cubecow_last_error()
 *   - All functions are panic-safe (panics in Rust are caught and
 *     converted to COW_ERR_PANIC)
 *
 * NOTE: Function names retain the historical `cubecow_` prefix so
 * existing C/Go consumers do not need to rebuild against renamed
 * symbols. The product is now called cubecow, and the header has been
 * renamed accordingly; the ABI is unchanged.
 */

#ifndef CUBECOW_H
#define CUBECOW_H

#include <stddef.h>
#include <stdint.h>
#include <stdbool.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ------------------------------------------------------------------ */
/* Opaque handle types                                                 */
/* ------------------------------------------------------------------ */

typedef void* CubecowEngineHandle;

/* ------------------------------------------------------------------ */
/* Error codes                                                         */
/* ------------------------------------------------------------------ */

#define COW_OK                       0
#define COW_ERR_NOT_FOUND           -1
#define COW_ERR_ALREADY_EXISTS      -2
#define COW_ERR_RESOURCE_EXHAUSTED  -3
#define COW_ERR_INVALID_ARG         -4
#define COW_ERR_IO_ERROR            -6
#define COW_ERR_CONFIG_ERROR        -10
#define COW_ERR_PRECONDITION_FAILED -11
#define COW_ERR_NULL_POINTER        -12
#define COW_ERR_INVALID_UTF8        -13
#define COW_ERR_PANIC               -99

/* ------------------------------------------------------------------ */
/* Lifecycle                                                           */
/* ------------------------------------------------------------------ */

/*
 * Initialize the cubecow engine from a TOML config file path.
 *
 * Returns an opaque engine pointer on success, or NULL on failure.
 * On failure, call cubecow_last_error() for details.
 */
CubecowEngineHandle cubecow_init(const char* config_path);

/*
 * Initialize the cubecow engine without setting up logging.
 * Use this when the host application manages its own logging/tracing.
 */
CubecowEngineHandle cubecow_init_without_logging(const char* config_path);

/*
 * Initialize the cubecow engine from a JSON configuration string.
 *
 * The JSON document follows the same schema as the TOML file.
 */
CubecowEngineHandle cubecow_init_from_json(const char* config_json);

/*
 * Initialize the cubecow engine from a JSON configuration string
 * without setting up logging.
 */
CubecowEngineHandle cubecow_init_without_logging_from_json(const char* config_json);

/*
 * Shutdown and destroy the cubecow engine.
 *
 * After this call, the engine pointer is invalid and must not be used.
 */
void cubecow_shutdown(CubecowEngineHandle engine);

/*
 * Destructively wipe all cubecow-managed storage currently recoverable
 * by the engine. The engine remains valid after this call but callers
 * should typically destroy and recreate it so that in-memory state
 * matches the new node layout.
 *
 * Returns 0 on success, negative error code on failure.
 */
int32_t cubecow_reset_node_storage(CubecowEngineHandle engine);

/* ------------------------------------------------------------------ */
/* Error handling                                                      */
/* ------------------------------------------------------------------ */

/*
 * Get the last error message for the current thread.
 *
 * Returns a pointer to a thread-local string. The pointer is valid
 * until the next FFI call on the same thread. Do NOT free this pointer.
 */
const char* cubecow_last_error(void);

/*
 * Free a string allocated by cubecow FFI functions.
 * Must be used for all char* out-params. Passing NULL is safe (no-op).
 */
void cubecow_free_string(char* s);

/* ------------------------------------------------------------------ */
/* Volume operations                                                   */
/* ------------------------------------------------------------------ */

/*
 * Create a new volume.
 *
 * out_device_path receives the backend device path (caller frees with
 * cubecow_free_string). Pass NULL to ignore.
 */
int32_t cubecow_create_volume(
    CubecowEngineHandle engine,
    const char* name,
    uint64_t size_bytes,
    char** out_device_path);

/*
 * Delete a volume by name.
 */
int32_t cubecow_delete_volume(CubecowEngineHandle engine, const char* name);

/*
 * Resize a volume (expand only). out_old_size / out_new_size are
 * optional and may be NULL.
 */
int32_t cubecow_resize_volume(
    CubecowEngineHandle engine,
    const char* name,
    uint64_t new_size_bytes,
    uint64_t* out_old_size,
    uint64_t* out_new_size);

/*
 * Get volume information by name. All out-params are optional (NULL).
 * String out-params must be freed by the caller with cubecow_free_string().
 */
int32_t cubecow_get_volume_info(
    CubecowEngineHandle engine,
    const char* name,
    uint64_t* out_size_bytes,
    char** out_device_path,
    int32_t* out_snapshot_count,
    char** out_created_at);

/*
 * Get block-level info for a volume.
 */
int32_t cubecow_get_volume_block_info(
    CubecowEngineHandle engine,
    const char* name,
    uint64_t* out_num_blocks,
    uint32_t* out_block_size);

/*
 * List volumes as a JSON string.
 *
 * out_json receives a JSON array of volume objects with fields:
 *   { name, size_bytes, device_path, snapshot_count, created_at }
 * out_next_page_token is NULL when there are no further pages.
 * out_total_count receives the total volume count.
 *
 * String out-params must be freed by the caller.
 */
int32_t cubecow_list_volumes(
    CubecowEngineHandle engine,
    uint64_t page_size,
    const char* page_token,
    char** out_json,
    char** out_next_page_token,
    uint64_t* out_total_count);

/* ------------------------------------------------------------------ */
/* Snapshot operations                                                 */
/* ------------------------------------------------------------------ */

/*
 * Create a snapshot from a volume or another snapshot.
 *
 * The `activate` flag controls whether a backend device node is
 * materialised alongside the snapshot metadata:
 *   - activate = true:  also create the device node;
 *                       out_device_path receives its path.
 *   - activate = false: metadata-only; out_device_path is written as
 *                       an empty string. Call cubecow_activate_volume()
 *                       later to materialise the device.
 *
 * Regardless of `activate`, the snapshot is identified by snapshot_name
 * for every subsequent operation.
 */
int32_t cubecow_create_snapshot(
    CubecowEngineHandle engine,
    const char* source_name,
    const char* snapshot_name,
    bool activate,
    char** out_device_path);

/*
 * Activate a volume or snapshot by name (creates its backend device node).
 * Idempotent.
 */
int32_t cubecow_activate_volume(
    CubecowEngineHandle engine,
    const char* name,
    char** out_device_path);

/*
 * Deactivate a volume or snapshot by name (removes its backend device node).
 * The metadata entry is preserved. Idempotent.
 */
int32_t cubecow_deactivate_volume(CubecowEngineHandle engine, const char* name);

/*
 * Delete a snapshot by name.
 */
int32_t cubecow_delete_snapshot(CubecowEngineHandle engine, const char* snapshot_name);

/*
 * List snapshots of a volume as a JSON string.
 *
 * out_json receives a JSON array of snapshot objects with fields:
 *   { name, size_bytes, device_path, origin_volume, created_at }
 * out_next_page_token is NULL when there are no further pages.
 *
 * String out-params must be freed by the caller.
 */
int32_t cubecow_list_snapshots(
    CubecowEngineHandle engine,
    const char* volume_name,
    uint64_t page_size,
    const char* page_token,
    char** out_json,
    char** out_next_page_token);

/* ------------------------------------------------------------------ */
/* Observability                                                       */
/* ------------------------------------------------------------------ */

/*
 * Get all metrics as a JSON object string (key-value pairs).
 */
int32_t cubecow_get_metrics(CubecowEngineHandle engine, char** out_json);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* CUBECOW_H */
