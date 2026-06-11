// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
// C FFI export layer for cubecow
//
// Provides `#[no_mangle] extern "C"` functions that can be called from
// C/Go via dynamic linking (cdylib). All functions follow these conventions:
//
// - Engine handle: opaque `*mut c_void` pointer
// - Return value: 0 on success, negative error code on failure
// - String output: via `*mut *mut c_char` out-param, caller frees with `cubecow_free_string`
// - Error details: thread-local last_error, retrieved via `cubecow_last_error`
// - Panic safety: all functions wrapped in `catch_unwind`
//
// # Safety
//
// All extern "C" functions in this module are inherently unsafe as they
// operate on raw pointers from C callers. Safety is ensured by:
// 1. Null-checking all pointer arguments before dereferencing
// 2. Validating UTF-8 encoding for string arguments
// 3. Wrapping all operations in `catch_unwind` to prevent panics across FFI
// 4. Using opaque handles with proper ownership semantics (Box::into_raw / Box::from_raw)

#![allow(unsafe_op_in_unsafe_fn)]

use std::cell::RefCell;
use std::collections::HashMap;
use std::ffi::{CStr, CString};
use std::os::raw::c_char;
use std::panic;
use std::ptr;

use crate::config::AppConfig;
use crate::pkg::errors::CubecowError;
use crate::Engine;

// The opaque handle exposed to C is a heap-allocated `Box<dyn Engine>`.
// Using `Box<Box<dyn Engine>>` (i.e. `*mut EngineHandle`) keeps the raw
// pointer thin (one machine word) while preserving the trait object's
// vtable on the inner box.
type EngineHandle = Box<dyn Engine>;

// ---------------------------------------------------------------------------
// Error codes (mapped from CubecowError variants)
// ---------------------------------------------------------------------------

const COW_OK: i32 = 0;
const COW_ERR_NOT_FOUND: i32 = -1;
const COW_ERR_ALREADY_EXISTS: i32 = -2;
const COW_ERR_RESOURCE_EXHAUSTED: i32 = -3;
const COW_ERR_INVALID_ARG: i32 = -4;
const COW_ERR_IO_ERROR: i32 = -6;
const COW_ERR_CONFIG_ERROR: i32 = -10;
const COW_ERR_PRECONDITION_FAILED: i32 = -11;
const COW_ERR_NULL_POINTER: i32 = -12;
const COW_ERR_INVALID_UTF8: i32 = -13;
const COW_ERR_PANIC: i32 = -99;

// ---------------------------------------------------------------------------
// Thread-local last error
// ---------------------------------------------------------------------------

thread_local! {
    static LAST_ERROR: RefCell<Option<CString>> = RefCell::new(None);
}

/// Store an error message in thread-local storage.
fn set_last_error(msg: &str) {
    LAST_ERROR.with(|cell| {
        *cell.borrow_mut() = CString::new(msg).ok();
    });
}

fn set_last_init_error(msg: &str) {
    eprintln!("cubecow init error: {msg}");
    set_last_error(msg);
}

/// Map a CubecowError to an error code and store the message.
fn handle_cow_error(e: &CubecowError) -> i32 {
    set_last_error(&e.to_string());
    match e {
        CubecowError::NotFound(_) => COW_ERR_NOT_FOUND,
        CubecowError::AlreadyExists(_) => COW_ERR_ALREADY_EXISTS,
        CubecowError::ResourceExhausted(_) => COW_ERR_RESOURCE_EXHAUSTED,
        CubecowError::InvalidArg(_) => COW_ERR_INVALID_ARG,
        CubecowError::IoError(_) => COW_ERR_IO_ERROR,
        CubecowError::ConfigError(_) => COW_ERR_CONFIG_ERROR,
        CubecowError::PreconditionFailed(_) => COW_ERR_PRECONDITION_FAILED,
    }
}

// ---------------------------------------------------------------------------
// Helper: safe C string conversion
// ---------------------------------------------------------------------------

/// Convert a C string pointer to a Rust &str. Returns Err on null or invalid UTF-8.
///
/// # Safety
///
/// The caller must ensure that `ptr` (if non-null) points to a valid,
/// null-terminated C string that remains valid for the lifetime `'a`.
unsafe fn c_str_to_str<'a>(ptr: *const c_char) -> Result<&'a str, i32> {
    if ptr.is_null() {
        set_last_error("null pointer passed for string argument");
        return Err(COW_ERR_NULL_POINTER);
    }
    // SAFETY: We have verified `ptr` is non-null. The caller guarantees it
    // points to a valid null-terminated C string with lifetime `'a`.
    CStr::from_ptr(ptr).to_str().map_err(|_| {
        set_last_error("invalid UTF-8 in string argument");
        COW_ERR_INVALID_UTF8
    })
}

/// Allocate a C string from a Rust string. Caller must free with `cubecow_free_string`.
fn rust_string_to_c(s: &str) -> *mut c_char {
    match CString::new(s) {
        Ok(cs) => cs.into_raw(),
        Err(_) => ptr::null_mut(),
    }
}

/// Safely cast an opaque engine pointer to a `&dyn Engine` reference.
///
/// # Safety
///
/// The caller must ensure that `engine` (if non-null) was previously returned
/// by `cubecow_init()` (or one of its variants) and has not been passed to
/// `cubecow_shutdown()`. The engine must remain valid for the lifetime `'a`.
unsafe fn engine_ref<'a>(engine: *mut std::ffi::c_void) -> Result<&'a dyn Engine, i32> {
    if engine.is_null() {
        set_last_error("null engine pointer");
        return Err(COW_ERR_NULL_POINTER);
    }
    // SAFETY: We have verified `engine` is non-null. The caller guarantees it
    // was created by `cubecow_init` and has not been freed, so the pointer
    // is valid and properly aligned for `EngineHandle` (= `Box<dyn Engine>`).
    let handle = &*(engine as *const EngineHandle);
    Ok(handle.as_ref())
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

/// Initialize the cubecow engine from a config file path.
///
/// Returns an opaque engine pointer on success, or NULL on failure.
/// On failure, call `cubecow_last_error()` for details.
#[no_mangle]
pub extern "C" fn cubecow_init(config_path: *const c_char) -> *mut std::ffi::c_void {
    let result = panic::catch_unwind(|| {
        // SAFETY: `config_path` is provided by the C caller and is expected to be
        // a valid null-terminated string that outlives this call.
        let path = unsafe { c_str_to_str(config_path) }.ok()?;

        let config = match AppConfig::load(path) {
            Ok(c) => c,
            Err(e) => {
                set_last_init_error(&format!("failed to load config: {e}"));
                return None;
            }
        };

        match crate::initialize(config) {
            Ok(engine) => {
                // engine is `Box<dyn Engine>`; wrap it in another Box so the
                // raw pointer we hand to C is thin (one machine word) and
                // points at the trait object via the inner Box's vtable.
                let boxed: Box<EngineHandle> = Box::new(engine);
                Some(Box::into_raw(boxed) as *mut std::ffi::c_void)
            }
            Err(e) => {
                set_last_init_error(&format!("failed to initialize engine: {e}"));
                None
            }
        }
    });

    match result {
        Ok(Some(ptr)) => ptr,
        Ok(None) => ptr::null_mut(),
        Err(_) => {
            set_last_init_error("panic during cubecow_init");
            ptr::null_mut()
        }
    }
}

/// Initialize the cubecow engine without setting up logging.
///
/// Use this when the host application manages its own logging/tracing.
/// Returns an opaque engine pointer on success, or NULL on failure.
#[no_mangle]
pub extern "C" fn cubecow_init_without_logging(
    config_path: *const c_char,
) -> *mut std::ffi::c_void {
    let result = panic::catch_unwind(|| {
        // SAFETY: `config_path` is provided by the C caller and is expected to be
        // a valid null-terminated string that outlives this call.
        let path = unsafe { c_str_to_str(config_path) }.ok()?;

        let config = match AppConfig::load(path) {
            Ok(c) => c,
            Err(e) => {
                set_last_init_error(&format!("failed to load config: {e}"));
                return None;
            }
        };

        match crate::initialize_without_logging(config) {
            Ok(engine) => {
                let boxed: Box<EngineHandle> = Box::new(engine);
                Some(Box::into_raw(boxed) as *mut std::ffi::c_void)
            }
            Err(e) => {
                set_last_init_error(&format!("failed to initialize engine: {e}"));
                None
            }
        }
    });

    match result {
        Ok(Some(ptr)) => ptr,
        Ok(None) => ptr::null_mut(),
        Err(_) => {
            set_last_init_error("panic during cubecow_init_without_logging");
            ptr::null_mut()
        }
    }
}

/// Initialize the cubecow engine from a JSON configuration string.
///
/// Returns an opaque engine pointer on success, or NULL on failure.
/// On failure, call `cubecow_last_error()` for details.
#[no_mangle]
pub extern "C" fn cubecow_init_from_json(config_json: *const c_char) -> *mut std::ffi::c_void {
    let result = panic::catch_unwind(|| {
        // SAFETY: `config_json` is provided by the C caller and is expected to be
        // a valid null-terminated string that outlives this call.
        let config_json = unsafe { c_str_to_str(config_json) }.ok()?;

        let config = match AppConfig::from_json_str(config_json) {
            Ok(c) => c,
            Err(e) => {
                set_last_init_error(&format!("failed to parse config: {e}"));
                return None;
            }
        };

        match crate::initialize(config) {
            Ok(engine) => {
                let boxed: Box<EngineHandle> = Box::new(engine);
                Some(Box::into_raw(boxed) as *mut std::ffi::c_void)
            }
            Err(e) => {
                set_last_init_error(&format!("failed to initialize engine: {e}"));
                None
            }
        }
    });

    match result {
        Ok(Some(ptr)) => ptr,
        Ok(None) => ptr::null_mut(),
        Err(_) => {
            set_last_init_error("panic during cubecow_init_from_json");
            ptr::null_mut()
        }
    }
}

/// Initialize the cubecow engine from a JSON configuration string without
/// setting up logging.
///
/// Use this when the host application manages its own logging/tracing.
/// Returns an opaque engine pointer on success, or NULL on failure.
#[no_mangle]
pub extern "C" fn cubecow_init_without_logging_from_json(
    config_json: *const c_char,
) -> *mut std::ffi::c_void {
    let result = panic::catch_unwind(|| {
        // SAFETY: `config_json` is provided by the C caller and is expected to be
        // a valid null-terminated string that outlives this call.
        let config_json = unsafe { c_str_to_str(config_json) }.ok()?;

        let config = match AppConfig::from_json_str(config_json) {
            Ok(c) => c,
            Err(e) => {
                set_last_init_error(&format!("failed to parse config: {e}"));
                return None;
            }
        };

        match crate::initialize_without_logging(config) {
            Ok(engine) => {
                let boxed: Box<EngineHandle> = Box::new(engine);
                Some(Box::into_raw(boxed) as *mut std::ffi::c_void)
            }
            Err(e) => {
                set_last_init_error(&format!("failed to initialize engine: {e}"));
                None
            }
        }
    });

    match result {
        Ok(Some(ptr)) => ptr,
        Ok(None) => ptr::null_mut(),
        Err(_) => {
            set_last_init_error("panic during cubecow_init_without_logging_from_json");
            ptr::null_mut()
        }
    }
}

/// Shutdown and destroy the cubecow engine.
///
/// After this call, the engine pointer is invalid and must not be used.
#[no_mangle]
pub extern "C" fn cubecow_shutdown(engine: *mut std::ffi::c_void) {
    if engine.is_null() {
        return;
    }
    let _ = panic::catch_unwind(|| {
        // SAFETY: `engine` was created by `Box::into_raw` in `cubecow_init`.
        // After this call the pointer is consumed and must not be reused.
        // Reconstructing the outer `Box<EngineHandle>` drops the inner
        // `Box<dyn Engine>` (and therefore the concrete backend) in turn.
        let _ = unsafe { Box::from_raw(engine as *mut EngineHandle) };
    });
}

/// Destructively wipe all cubecow-managed storage currently recoverable by the engine.
///
/// Intended for host-level reset flows such as `cubecli unsafe init`.
/// The engine remains valid after this call, but callers should typically
/// destroy and recreate it so that in-memory pool state matches the new node
/// layout.
#[no_mangle]
pub extern "C" fn cubecow_reset_node_storage(engine: *mut std::ffi::c_void) -> i32 {
    let result = panic::catch_unwind(panic::AssertUnwindSafe(|| {
        // SAFETY: `engine` was created by `cubecow_init` and is valid.
        let eng = unsafe { engine_ref(engine) }?;
        eng.reset_node_storage()
            .map(|_| COW_OK)
            .map_err(|e| handle_cow_error(&e))
    }));

    match result {
        Ok(Ok(code)) => code,
        Ok(Err(code)) => code,
        Err(_) => {
            set_last_error("panic during cubecow_reset_node_storage");
            COW_ERR_PANIC
        }
    }
}

// ---------------------------------------------------------------------------
// Error handling
// ---------------------------------------------------------------------------

/// Get the last error message for the current thread.
///
/// Returns a pointer to a static thread-local string. The pointer is valid
/// until the next FFI call on the same thread. Do NOT free this pointer.
#[no_mangle]
pub extern "C" fn cubecow_last_error() -> *const c_char {
    LAST_ERROR.with(|cell| {
        let borrow = cell.borrow();
        match borrow.as_ref() {
            Some(cs) => cs.as_ptr(),
            None => ptr::null(),
        }
    })
}

/// Free a string allocated by cubecow FFI functions.
///
/// Must be used for all `*mut c_char` out-params returned by FFI functions.
/// Passing NULL is safe (no-op).
#[no_mangle]
pub extern "C" fn cubecow_free_string(s: *mut c_char) {
    if !s.is_null() {
        // SAFETY: `s` was allocated by `CString::into_raw` in `rust_string_to_c`.
        // The caller guarantees it has not been freed previously.
        let _ = unsafe { CString::from_raw(s) };
    }
}

// ---------------------------------------------------------------------------
// Volume operations
// ---------------------------------------------------------------------------

/// Create a new volume.
///
/// # Parameters
/// - `engine`: opaque engine pointer
/// - `name`: volume name (C string)
/// - `size_bytes`: volume size in bytes
/// - `out_device_path`: receives the device path (caller frees with `cubecow_free_string`)
///
/// # Returns
/// 0 on success, negative error code on failure.
#[no_mangle]
pub extern "C" fn cubecow_create_volume(
    engine: *mut std::ffi::c_void,
    name: *const c_char,
    size_bytes: u64,
    out_device_path: *mut *mut c_char,
) -> i32 {
    let result = panic::catch_unwind(panic::AssertUnwindSafe(|| {
        // SAFETY: `engine` was created by `cubecow_init` and is valid.
        let eng = unsafe { engine_ref(engine) }?;
        // SAFETY: `name` is a valid C string provided by the caller.
        let vol_name = unsafe { c_str_to_str(name) }?;

        match eng.create_volume(vol_name, size_bytes) {
            Ok(vol) => {
                if !out_device_path.is_null() {
                    // SAFETY: `out_device_path` is non-null and writable.
                    unsafe { *out_device_path = rust_string_to_c(&vol.device_path) };
                }
                Ok(COW_OK)
            }
            Err(e) => Err(handle_cow_error(&e)),
        }
    }));

    match result {
        Ok(Ok(code)) => code,
        Ok(Err(code)) => code,
        Err(_) => {
            set_last_error("panic during cubecow_create_volume");
            COW_ERR_PANIC
        }
    }
}

/// Delete a volume by name.
///
/// # Returns
/// 0 on success, negative error code on failure.
#[no_mangle]
pub extern "C" fn cubecow_delete_volume(engine: *mut std::ffi::c_void, name: *const c_char) -> i32 {
    let result = panic::catch_unwind(panic::AssertUnwindSafe(|| {
        // SAFETY: `engine` was created by `cubecow_init` and is valid.
        let eng = unsafe { engine_ref(engine) }?;
        // SAFETY: `name` is a valid C string provided by the caller.
        let vol_name = unsafe { c_str_to_str(name) }?;

        match eng.delete_volume(vol_name) {
            Ok(()) => Ok(COW_OK),
            Err(e) => Err(handle_cow_error(&e)),
        }
    }));

    match result {
        Ok(Ok(code)) => code,
        Ok(Err(code)) => code,
        Err(_) => {
            set_last_error("panic during cubecow_delete_volume");
            COW_ERR_PANIC
        }
    }
}

/// Resize a volume (expand only).
///
/// # Parameters
/// - `new_size_bytes`: new size in bytes (must be larger than current)
/// - `out_old_size`: receives the old size in bytes (optional, can be NULL)
/// - `out_new_size`: receives the new size in bytes (optional, can be NULL)
///
/// # Returns
/// 0 on success, negative error code on failure.
#[no_mangle]
pub extern "C" fn cubecow_resize_volume(
    engine: *mut std::ffi::c_void,
    name: *const c_char,
    new_size_bytes: u64,
    out_old_size: *mut u64,
    out_new_size: *mut u64,
) -> i32 {
    let result = panic::catch_unwind(panic::AssertUnwindSafe(|| {
        // SAFETY: `engine` was created by `cubecow_init` and is valid.
        let eng = unsafe { engine_ref(engine) }?;
        // SAFETY: `name` is a valid C string provided by the caller.
        let vol_name = unsafe { c_str_to_str(name) }?;

        match eng.resize_volume(vol_name, new_size_bytes) {
            Ok((old_size, new_size)) => {
                if !out_old_size.is_null() {
                    // SAFETY: `out_old_size` is non-null and writable.
                    unsafe { *out_old_size = old_size };
                }
                if !out_new_size.is_null() {
                    // SAFETY: `out_new_size` is non-null and writable.
                    unsafe { *out_new_size = new_size };
                }
                Ok(COW_OK)
            }
            Err(e) => Err(handle_cow_error(&e)),
        }
    }));

    match result {
        Ok(Ok(code)) => code,
        Ok(Err(code)) => code,
        Err(_) => {
            set_last_error("panic during cubecow_resize_volume");
            COW_ERR_PANIC
        }
    }
}

/// Get volume information by name.
///
/// # Parameters
/// - `out_size_bytes`: receives the volume size in bytes
/// - `out_device_path`: receives the device path (caller frees)
/// - `out_snapshot_count`: receives the snapshot count
/// - `out_created_at`: receives the creation timestamp (caller frees)
///
/// All out-params are optional (can be NULL).
///
/// # Returns
/// 0 on success, negative error code on failure.
#[no_mangle]
pub extern "C" fn cubecow_get_volume_info(
    engine: *mut std::ffi::c_void,
    name: *const c_char,
    out_size_bytes: *mut u64,
    out_device_path: *mut *mut c_char,
    out_snapshot_count: *mut i32,
    out_created_at: *mut *mut c_char,
) -> i32 {
    let result = panic::catch_unwind(panic::AssertUnwindSafe(|| {
        // SAFETY: `engine` was created by `cubecow_init` and is valid.
        let eng = unsafe { engine_ref(engine) }?;
        // SAFETY: `name` is a valid C string provided by the caller.
        let vol_name = unsafe { c_str_to_str(name) }?;

        match eng.get_volume_info(vol_name) {
            Ok(vol) => {
                // SAFETY: All out-param pointers are checked for null before
                // dereferencing. The caller guarantees non-null pointers are
                // valid and writable.
                unsafe {
                    if !out_size_bytes.is_null() {
                        *out_size_bytes = vol.size_bytes;
                    }
                    if !out_device_path.is_null() {
                        *out_device_path = rust_string_to_c(&vol.device_path);
                    }
                    if !out_snapshot_count.is_null() {
                        *out_snapshot_count = vol.snapshot_count;
                    }
                    if !out_created_at.is_null() {
                        *out_created_at = rust_string_to_c(&vol.created_at);
                    }
                }
                Ok(COW_OK)
            }
            Err(e) => Err(handle_cow_error(&e)),
        }
    }));

    match result {
        Ok(Ok(code)) => code,
        Ok(Err(code)) => code,
        Err(_) => {
            set_last_error("panic during cubecow_get_volume_info");
            COW_ERR_PANIC
        }
    }
}

/// Get block-level info for a volume.
///
/// # Parameters
/// - `out_num_blocks`: receives the number of blocks
/// - `out_block_size`: receives the block size in bytes
///
/// # Returns
/// 0 on success, negative error code on failure.
#[no_mangle]
pub extern "C" fn cubecow_get_volume_block_info(
    engine: *mut std::ffi::c_void,
    name: *const c_char,
    out_num_blocks: *mut u64,
    out_block_size: *mut u32,
) -> i32 {
    let result = panic::catch_unwind(panic::AssertUnwindSafe(|| {
        // SAFETY: `engine` was created by `cubecow_init` and is valid.
        let eng = unsafe { engine_ref(engine) }?;
        // SAFETY: `name` is a valid C string provided by the caller.
        let vol_name = unsafe { c_str_to_str(name) }?;

        match eng.get_volume_block_info(vol_name) {
            Ok(info) => {
                if !out_num_blocks.is_null() {
                    // SAFETY: `out_num_blocks` is non-null and writable.
                    unsafe { *out_num_blocks = info.num_blocks };
                }
                if !out_block_size.is_null() {
                    // SAFETY: `out_block_size` is non-null and writable.
                    unsafe { *out_block_size = info.block_size };
                }
                Ok(COW_OK)
            }
            Err(e) => Err(handle_cow_error(&e)),
        }
    }));

    match result {
        Ok(Ok(code)) => code,
        Ok(Err(code)) => code,
        Err(_) => {
            set_last_error("panic during cubecow_get_volume_block_info");
            COW_ERR_PANIC
        }
    }
}

/// List volumes as a JSON string.
///
/// Returns a JSON array of volume objects. The caller must free the
/// returned string with `cubecow_free_string`.
///
/// # Parameters
/// - `page_size`: max number of volumes to return (0 for all)
/// - `page_token`: optional pagination token (NULL for first page)
/// - `out_json`: receives the JSON string (caller frees)
/// - `out_next_page_token`: receives the next page token, or NULL if no more pages (caller frees)
/// - `out_total_count`: receives the total volume count
///
/// # Returns
/// 0 on success, negative error code on failure.
#[no_mangle]
pub extern "C" fn cubecow_list_volumes(
    engine: *mut std::ffi::c_void,
    page_size: u64,
    page_token: *const c_char,
    out_json: *mut *mut c_char,
    out_next_page_token: *mut *mut c_char,
    out_total_count: *mut u64,
) -> i32 {
    let result = panic::catch_unwind(panic::AssertUnwindSafe(|| {
        // SAFETY: `engine` was created by `cubecow_init` and is valid.
        let eng = unsafe { engine_ref(engine) }?;

        let token_filter = if page_token.is_null() {
            None
        } else {
            // SAFETY: `page_token` is non-null, caller guarantees valid C string.
            Some(unsafe { c_str_to_str(page_token) }?)
        };

        let (volumes, next_token, total) = eng.list_volumes(page_size as usize, token_filter);

        // Serialize volumes to JSON
        let json_items: Vec<serde_json::Value> = volumes
            .iter()
            .map(|v| {
                serde_json::json!({
                    "name": v.name,
                    "size_bytes": v.size_bytes,
                    "device_path": v.device_path,
                    "snapshot_count": v.snapshot_count,
                    "created_at": v.created_at,
                })
            })
            .collect();

        let json_str = serde_json::to_string(&json_items).unwrap_or_else(|_| "[]".to_string());

        // SAFETY: All out-param pointers are checked for null before
        // dereferencing. The caller guarantees non-null pointers are writable.
        unsafe {
            if !out_json.is_null() {
                *out_json = rust_string_to_c(&json_str);
            }
            if !out_next_page_token.is_null() {
                *out_next_page_token = match &next_token {
                    Some(t) => rust_string_to_c(t),
                    None => ptr::null_mut(),
                };
            }
            if !out_total_count.is_null() {
                *out_total_count = total as u64;
            }
        }

        Ok(COW_OK)
    }));

    match result {
        Ok(Ok(code)) => code,
        Ok(Err(code)) => code,
        Err(_) => {
            set_last_error("panic during cubecow_list_volumes");
            COW_ERR_PANIC
        }
    }
}

// ---------------------------------------------------------------------------
// Snapshot operations
// ---------------------------------------------------------------------------

/// Create a snapshot from a volume or another snapshot.
///
/// The `activate` flag controls whether a backend device node is
/// materialised alongside the snapshot metadata:
///
/// - `activate = true` (legacy behaviour): also create the device
///   node. `out_device_path` receives the path of the resulting node.
///   If activation fails, the snapshot is rolled back and a non-zero
///   error code is returned.
/// - `activate = false`: metadata-only. The snapshot exists in the
///   backend but has no exposed device node; `out_device_path` is
///   written as an empty string. Call `cubecow_activate_volume` later
///   to materialise the device.
///
/// # Referring to the snapshot afterwards
///
/// Regardless of the `activate` flag, the snapshot is **identified by
/// the `snapshot_name` string** passed in here. Callers should retain
/// that name and reuse it for every follow-up operation — there is no
/// opaque handle to track:
///
/// - `cubecow_activate_volume(name = snapshot_name)` to materialise the
///   device later;
/// - `cubecow_create_snapshot(source_name = snapshot_name, ...)` to
///   build a snapshot of this snapshot;
/// - `cubecow_delete_snapshot(snapshot_name)` to remove it;
/// - `cubecow_list_snapshots` to enumerate (the entry will appear with
///   an empty `device_path` until it is activated).
///
/// # Parameters
/// - `engine`: opaque engine pointer
/// - `source_name`: name of the source volume or snapshot (C string)
/// - `snapshot_name`: name for the new snapshot (C string)
/// - `activate`: whether to create the backend device node alongside
///   the snapshot metadata
/// - `out_device_path`: receives the device path (caller frees with
///   `cubecow_free_string`). Optional, can be NULL.
///
/// # Returns
/// 0 on success, negative error code on failure.
#[no_mangle]
pub extern "C" fn cubecow_create_snapshot(
    engine: *mut std::ffi::c_void,
    source_name: *const c_char,
    snapshot_name: *const c_char,
    activate: bool,
    out_device_path: *mut *mut c_char,
) -> i32 {
    let result = panic::catch_unwind(panic::AssertUnwindSafe(|| {
        // SAFETY: `engine` was created by `cubecow_init` and is valid.
        let eng = unsafe { engine_ref(engine) }?;
        // SAFETY: `source_name` is a valid C string provided by the caller.
        let src = unsafe { c_str_to_str(source_name) }?;
        // SAFETY: `snapshot_name` is a valid C string provided by the caller.
        let snap = unsafe { c_str_to_str(snapshot_name) }?;

        match eng.create_snapshot(src, snap, activate) {
            Ok(snapshot) => {
                if !out_device_path.is_null() {
                    // When `activate = false`, `snapshot.device_path` is
                    // empty; we still write an (empty) owned C string so
                    // the caller's free contract is uniform.
                    // SAFETY: `out_device_path` is non-null and writable.
                    unsafe { *out_device_path = rust_string_to_c(&snapshot.device_path) };
                }
                Ok(COW_OK)
            }
            Err(e) => Err(handle_cow_error(&e)),
        }
    }));

    match result {
        Ok(Ok(code)) => code,
        Ok(Err(code)) => code,
        Err(_) => {
            set_last_error("panic during cubecow_create_snapshot");
            COW_ERR_PANIC
        }
    }
}

/// Activate a volume or snapshot by name (creates its backend device node).
///
/// Must be called before issuing block I/O against a snapshot produced by
/// `cubecow_create_snapshot`, and can also be used to re-activate a volume /
/// snapshot that was previously deactivated via `cubecow_deactivate_volume`.
/// Idempotent.
///
/// # Parameters
/// - `engine`: opaque engine pointer
/// - `name`: volume or snapshot name (C string)
/// - `out_device_path`: receives the device path (caller frees with
///   `cubecow_free_string`). Optional, can be NULL.
///
/// # Returns
/// 0 on success, negative error code on failure.
#[no_mangle]
pub extern "C" fn cubecow_activate_volume(
    engine: *mut std::ffi::c_void,
    name: *const c_char,
    out_device_path: *mut *mut c_char,
) -> i32 {
    let result = panic::catch_unwind(panic::AssertUnwindSafe(|| {
        // SAFETY: `engine` was created by `cubecow_init` and is valid.
        let eng = unsafe { engine_ref(engine) }?;
        // SAFETY: `name` is a valid C string provided by the caller.
        let vol_name = unsafe { c_str_to_str(name) }?;

        match eng.activate_volume(vol_name) {
            Ok(vol) => {
                if !out_device_path.is_null() {
                    // SAFETY: `out_device_path` is non-null and writable.
                    unsafe { *out_device_path = rust_string_to_c(&vol.device_path) };
                }
                Ok(COW_OK)
            }
            Err(e) => Err(handle_cow_error(&e)),
        }
    }));

    match result {
        Ok(Ok(code)) => code,
        Ok(Err(code)) => code,
        Err(_) => {
            set_last_error("panic during cubecow_activate_volume");
            COW_ERR_PANIC
        }
    }
}

/// Deactivate a volume or snapshot by name (removes its backend device node).
///
/// The metadata entry is preserved; the entry can be re-exposed later via
/// `cubecow_activate_volume`. Idempotent.
///
/// # Parameters
/// - `engine`: opaque engine pointer
/// - `name`: volume or snapshot name (C string)
///
/// # Returns
/// 0 on success, negative error code on failure.
#[no_mangle]
pub extern "C" fn cubecow_deactivate_volume(
    engine: *mut std::ffi::c_void,
    name: *const c_char,
) -> i32 {
    let result = panic::catch_unwind(panic::AssertUnwindSafe(|| {
        // SAFETY: `engine` was created by `cubecow_init` and is valid.
        let eng = unsafe { engine_ref(engine) }?;
        // SAFETY: `name` is a valid C string provided by the caller.
        let vol_name = unsafe { c_str_to_str(name) }?;

        match eng.deactivate_volume(vol_name) {
            Ok(()) => Ok(COW_OK),
            Err(e) => Err(handle_cow_error(&e)),
        }
    }));

    match result {
        Ok(Ok(code)) => code,
        Ok(Err(code)) => code,
        Err(_) => {
            set_last_error("panic during cubecow_deactivate_volume");
            COW_ERR_PANIC
        }
    }
}

/// Delete a snapshot by name.
///
/// # Returns
/// 0 on success, negative error code on failure.
#[no_mangle]
pub extern "C" fn cubecow_delete_snapshot(
    engine: *mut std::ffi::c_void,
    snapshot_name: *const c_char,
) -> i32 {
    let result = panic::catch_unwind(panic::AssertUnwindSafe(|| {
        // SAFETY: `engine` was created by `cubecow_init` and is valid.
        let eng = unsafe { engine_ref(engine) }?;
        // SAFETY: `snapshot_name` is a valid C string provided by the caller.
        let snap = unsafe { c_str_to_str(snapshot_name) }?;

        match eng.delete_snapshot(snap) {
            Ok(()) => Ok(COW_OK),
            Err(e) => Err(handle_cow_error(&e)),
        }
    }));

    match result {
        Ok(Ok(code)) => code,
        Ok(Err(code)) => code,
        Err(_) => {
            set_last_error("panic during cubecow_delete_snapshot");
            COW_ERR_PANIC
        }
    }
}

/// List snapshots of a volume as a JSON string.
///
/// Returns a JSON array of snapshot objects. The caller must free the
/// returned string with `cubecow_free_string`.
///
/// # Parameters
/// - `volume_name`: name of the parent volume
/// - `page_size`: max number of snapshots to return (0 for all)
/// - `page_token`: optional pagination token (NULL for first page)
/// - `out_json`: receives the JSON string (caller frees)
/// - `out_next_page_token`: receives the next page token, or NULL if no more pages (caller frees)
///
/// # Returns
/// 0 on success, negative error code on failure.
#[no_mangle]
pub extern "C" fn cubecow_list_snapshots(
    engine: *mut std::ffi::c_void,
    volume_name: *const c_char,
    page_size: u64,
    page_token: *const c_char,
    out_json: *mut *mut c_char,
    out_next_page_token: *mut *mut c_char,
) -> i32 {
    let result = panic::catch_unwind(panic::AssertUnwindSafe(|| {
        // SAFETY: `engine` was created by `cubecow_init` and is valid.
        let eng = unsafe { engine_ref(engine) }?;
        // SAFETY: `volume_name` is a valid C string provided by the caller.
        let vol_name = unsafe { c_str_to_str(volume_name) }?;

        let token_filter = if page_token.is_null() {
            None
        } else {
            // SAFETY: `page_token` is non-null, caller guarantees valid C string.
            Some(unsafe { c_str_to_str(page_token) }?)
        };

        let (snapshots, next_token) =
            eng.list_snapshots(vol_name, page_size as usize, token_filter);

        let json_items: Vec<serde_json::Value> = snapshots
            .iter()
            .map(|s| {
                serde_json::json!({
                    "name": s.name,
                    "size_bytes": s.size_bytes,
                    "device_path": s.device_path,
                    "origin_volume": s.origin_volume,
                    "created_at": s.created_at,
                })
            })
            .collect();

        let json_str = serde_json::to_string(&json_items).unwrap_or_else(|_| "[]".to_string());

        // SAFETY: All out-param pointers are checked for null before
        // dereferencing. The caller guarantees non-null pointers are writable.
        unsafe {
            if !out_json.is_null() {
                *out_json = rust_string_to_c(&json_str);
            }
            if !out_next_page_token.is_null() {
                *out_next_page_token = match &next_token {
                    Some(t) => rust_string_to_c(t),
                    None => ptr::null_mut(),
                };
            }
        }

        Ok(COW_OK)
    }));

    match result {
        Ok(Ok(code)) => code,
        Ok(Err(code)) => code,
        Err(_) => {
            set_last_error("panic during cubecow_list_snapshots");
            COW_ERR_PANIC
        }
    }
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

/// Get all metrics as a JSON object string.
///
/// Returns a JSON object where keys are metric names and values are u64.
/// The caller must free the returned string with `cubecow_free_string`.
///
/// # Returns
/// 0 on success, negative error code on failure.
#[no_mangle]
pub extern "C" fn cubecow_get_metrics(
    engine: *mut std::ffi::c_void,
    out_json: *mut *mut c_char,
) -> i32 {
    let result = panic::catch_unwind(panic::AssertUnwindSafe(|| {
        // SAFETY: `engine` was created by `cubecow_init` and is valid.
        let eng = unsafe { engine_ref(engine) }?;

        let metrics: HashMap<String, u64> = eng.metrics();

        let json_str = serde_json::to_string(&metrics).unwrap_or_else(|_| "{}".to_string());

        if !out_json.is_null() {
            // SAFETY: `out_json` is non-null and writable.
            unsafe { *out_json = rust_string_to_c(&json_str) };
        }

        Ok(COW_OK)
    }));

    match result {
        Ok(Ok(code)) => code,
        Ok(Err(code)) => code,
        Err(_) => {
            set_last_error("panic during cubecow_get_metrics");
            COW_ERR_PANIC
        }
    }
}
