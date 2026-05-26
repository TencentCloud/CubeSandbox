/*
 * Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0
 */
/*
 * cubecow.h - C FFI header for cubecow engine
 *           (xfs-reflink based copy-on-write storage backend)
 *
 * This header declares all extern "C" functions exported by libcubecow.so.
 *
 * NOTE: This file is a flattened copy of `include/cubecow.h` from the
 * cubecow project root, kept here so the cgo wrapper can be built
 * stand-alone (cgo's `#include` lookup is rooted at the package
 * directory, not the project root). Keep the two files in sync — when
 * `include/cubecow.h` changes, refresh this copy (or symlink it). The
 * function prototypes below MUST match exactly what `libcubecow.so`
 * actually exports; mismatches against the generated cdylib will
 * produce "undefined reference to cubecow_xxx" link errors.
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

void* cubecow_init(const char* config_path);
void* cubecow_init_without_logging(const char* config_path);
void* cubecow_init_from_json(const char* config_json);
void* cubecow_init_without_logging_from_json(const char* config_json);
void  cubecow_shutdown(void* engine);
int32_t cubecow_reset_node_storage(void* engine);

/* ------------------------------------------------------------------ */
/* Error handling                                                      */
/* ------------------------------------------------------------------ */

const char* cubecow_last_error(void);
void cubecow_free_string(char* s);

/* ------------------------------------------------------------------ */
/* Volume operations                                                   */
/* ------------------------------------------------------------------ */

int32_t cubecow_create_volume(
    void* engine,
    const char* name,
    uint64_t size_bytes,
    char** out_device_path);

int32_t cubecow_delete_volume(void* engine, const char* name);

int32_t cubecow_resize_volume(
    void* engine,
    const char* name,
    uint64_t new_size_bytes,
    uint64_t* out_old_size,
    uint64_t* out_new_size);

int32_t cubecow_get_volume_info(
    void* engine,
    const char* name,
    uint64_t* out_size_bytes,
    char** out_device_path,
    int32_t* out_snapshot_count,
    char** out_created_at);

int32_t cubecow_get_volume_block_info(
    void* engine,
    const char* name,
    uint64_t* out_num_blocks,
    uint32_t* out_block_size);

int32_t cubecow_list_volumes(
    void* engine,
    uint64_t page_size,
    const char* page_token,
    char** out_json,
    char** out_next_page_token,
    uint64_t* out_total_count);

/* ------------------------------------------------------------------ */
/* Snapshot operations                                                 */
/* ------------------------------------------------------------------ */

int32_t cubecow_create_snapshot(
    void* engine,
    const char* source_name,
    const char* snapshot_name,
    bool activate,
    char** out_device_path);

int32_t cubecow_activate_volume(
    void* engine,
    const char* name,
    char** out_device_path);

int32_t cubecow_deactivate_volume(void* engine, const char* name);

int32_t cubecow_delete_snapshot(void* engine, const char* snapshot_name);

int32_t cubecow_list_snapshots(
    void* engine,
    const char* volume_name,
    uint64_t page_size,
    const char* page_token,
    char** out_json,
    char** out_next_page_token);

/* ------------------------------------------------------------------ */
/* Observability                                                       */
/* ------------------------------------------------------------------ */

int32_t cubecow_get_metrics(void* engine, char** out_json);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* CUBECOW_H */
