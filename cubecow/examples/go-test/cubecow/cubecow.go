// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package cubecow provides Go bindings for the cubecow FFI library.
//
// It wraps the C FFI functions exported by libcubecow.so, providing
// a safe, idiomatic Go API with proper error handling and memory management.
package cubecow

/*
#cgo LDFLAGS: -lcubecow
#include "cubecow.h"
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"
)

// Error codes matching the C FFI definitions.
const (
	ErrNotFound          = -1
	ErrAlreadyExists     = -2
	ErrResourceExhausted = -3
	ErrInvalidArg        = -4
	ErrIoError           = -6
	ErrConfigError       = -10
	ErrPreconditionFailed = -11
	ErrNullPointer       = -12
	ErrInvalidUtf8       = -13
	ErrPanic             = -99
)

// CubecowError represents an error returned by the cubecow engine.
type CubecowError struct {
	Code    int
	Message string
}

func (e *CubecowError) Error() string {
	return fmt.Sprintf("cubecow error (code=%d): %s", e.Code, e.Message)
}

// ErrorCodeName returns a human-readable name for an error code.
func ErrorCodeName(code int) string {
	switch code {
	case ErrNotFound:
		return "NotFound"
	case ErrAlreadyExists:
		return "AlreadyExists"
	case ErrResourceExhausted:
		return "ResourceExhausted"
	case ErrInvalidArg:
		return "InvalidArg"
	case ErrIoError:
		return "IoError"
	case ErrConfigError:
		return "ConfigError"
	case ErrPreconditionFailed:
		return "PreconditionFailed"
	case ErrNullPointer:
		return "NullPointer"
	case ErrInvalidUtf8:
		return "InvalidUtf8"
	case ErrPanic:
		return "Panic"
	default:
		return "Unknown"
	}
}

// lastError retrieves the last error message from the FFI layer.
func lastError() string {
	p := C.cubecow_last_error()
	if p == nil {
		return "unknown error"
	}
	return C.GoString(p)
}

// makeError creates a CubecowError from a return code.
func makeError(code C.int32_t) error {
	return &CubecowError{
		Code:    int(code),
		Message: lastError(),
	}
}

// Engine represents an initialized cubecow engine instance.
type Engine struct {
	ptr unsafe.Pointer
}

// Volume holds volume information returned by the engine.
type Volume struct {
	Name          string `json:"name"`
	SizeBytes     uint64 `json:"size_bytes"`
	DevicePath    string `json:"device_path"`
	SnapshotCount int32  `json:"snapshot_count"`
	CreatedAt     string `json:"created_at"`
}

// Snapshot holds snapshot information returned by the engine.
type Snapshot struct {
	Name         string `json:"name"`
	SizeBytes    uint64 `json:"size_bytes"`
	DevicePath   string `json:"device_path"`
	OriginVolume string `json:"origin_volume"`
	CreatedAt    string `json:"created_at"`
}

// VolumeBlockInfo holds block-level information for a volume.
type VolumeBlockInfo struct {
	NumBlocks uint64
	BlockSize uint32
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// Init initializes the cubecow engine from a config file path.
// Returns an Engine handle or an error.
func Init(configPath string) (*Engine, error) {
	cPath := C.CString(configPath)
	defer C.free(unsafe.Pointer(cPath))

	ptr := C.cubecow_init(cPath)
	if ptr == nil {
		return nil, fmt.Errorf("cubecow_init failed: %s", lastError())
	}
	return &Engine{ptr: ptr}, nil
}

// InitWithoutLogging initializes the engine without setting up logging.
// Use this when the host application manages its own logging.
func InitWithoutLogging(configPath string) (*Engine, error) {
	cPath := C.CString(configPath)
	defer C.free(unsafe.Pointer(cPath))

	ptr := C.cubecow_init_without_logging(cPath)
	if ptr == nil {
		return nil, fmt.Errorf("cubecow_init_without_logging failed: %s", lastError())
	}
	return &Engine{ptr: ptr}, nil
}

// InitFromJSON initializes the cubecow engine from an in-memory JSON
// configuration string.
//
// This is the preferred entry point when the caller assembles the
// cubecow configuration in memory (e.g. by translating a host-side
// inline config into the AppConfig schema), instead of pointing at a
// TOML file on disk. The JSON schema matches the Rust `AppConfig`
// structure exposed by libcubecow, with top-level keys `storage`,
// `log`, optional `disk`, optional `cubecow`, and optional `backend`.
func InitFromJSON(configJSON string) (*Engine, error) {
	cJSON := C.CString(configJSON)
	defer C.free(unsafe.Pointer(cJSON))

	ptr := C.cubecow_init_from_json(cJSON)
	if ptr == nil {
		return nil, fmt.Errorf("cubecow_init_from_json failed: %s", lastError())
	}
	return &Engine{ptr: ptr}, nil
}

// InitWithoutLoggingFromJSON is like InitFromJSON but skips setting up
// the libcubecow tracing subscriber. Use this when the host application
// manages its own logging.
func InitWithoutLoggingFromJSON(configJSON string) (*Engine, error) {
	cJSON := C.CString(configJSON)
	defer C.free(unsafe.Pointer(cJSON))

	ptr := C.cubecow_init_without_logging_from_json(cJSON)
	if ptr == nil {
		return nil, fmt.Errorf("cubecow_init_without_logging_from_json failed: %s", lastError())
	}
	return &Engine{ptr: ptr}, nil
}

// Shutdown destroys the engine and releases all resources.
// After calling Shutdown, the Engine must not be used.
func (e *Engine) Shutdown() {
	if e.ptr != nil {
		C.cubecow_shutdown(e.ptr)
		e.ptr = nil
	}
}

// ---------------------------------------------------------------------------
// Volume operations
// ---------------------------------------------------------------------------

// CreateVolume creates a new volume.
// Returns the device path on success.
func (e *Engine) CreateVolume(name string, sizeBytes uint64) (string, error) {
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	var cDevicePath *C.char
	rc := C.cubecow_create_volume(e.ptr, cName, C.uint64_t(sizeBytes), &cDevicePath)
	if rc != 0 {
		return "", makeError(rc)
	}
	devicePath := C.GoString(cDevicePath)
	C.cubecow_free_string(cDevicePath)
	return devicePath, nil
}

// DeleteVolume deletes a volume by name.
func (e *Engine) DeleteVolume(name string) error {
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	rc := C.cubecow_delete_volume(e.ptr, cName)
	if rc != 0 {
		return makeError(rc)
	}
	return nil
}

// ResizeVolume resizes a volume (expand only).
// Returns (oldSize, newSize) on success.
func (e *Engine) ResizeVolume(name string, newSizeBytes uint64) (uint64, uint64, error) {
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	var oldSize, newSize C.uint64_t
	rc := C.cubecow_resize_volume(e.ptr, cName, C.uint64_t(newSizeBytes), &oldSize, &newSize)
	if rc != 0 {
		return 0, 0, makeError(rc)
	}
	return uint64(oldSize), uint64(newSize), nil
}

// GetVolumeInfo retrieves volume information by name.
func (e *Engine) GetVolumeInfo(name string) (*Volume, error) {
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	var sizeBytes C.uint64_t
	var cDevicePath, cCreatedAt *C.char
	var snapCount C.int32_t

	rc := C.cubecow_get_volume_info(
		e.ptr, cName,
		&sizeBytes, &cDevicePath, &snapCount, &cCreatedAt,
	)
	if rc != 0 {
		return nil, makeError(rc)
	}

	vol := &Volume{
		Name:          name,
		SizeBytes:     uint64(sizeBytes),
		DevicePath:    C.GoString(cDevicePath),
		SnapshotCount: int32(snapCount),
		CreatedAt:     C.GoString(cCreatedAt),
	}

	C.cubecow_free_string(cDevicePath)
	C.cubecow_free_string(cCreatedAt)

	return vol, nil
}

// GetVolumeBlockInfo retrieves block-level info for a volume.
func (e *Engine) GetVolumeBlockInfo(name string) (*VolumeBlockInfo, error) {
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	var numBlocks C.uint64_t
	var blockSize C.uint32_t

	rc := C.cubecow_get_volume_block_info(e.ptr, cName, &numBlocks, &blockSize)
	if rc != 0 {
		return nil, makeError(rc)
	}

	return &VolumeBlockInfo{
		NumBlocks: uint64(numBlocks),
		BlockSize: uint32(blockSize),
	}, nil
}

// ListVolumesResult holds the result of a ListVolumes call.
type ListVolumesResult struct {
	Volumes       []Volume
	NextPageToken string
	TotalCount    uint64
}

// ListVolumes lists volumes with pagination.
// Pass 0 for pageSize to list all volumes.
func (e *Engine) ListVolumes(pageSize uint64, pageToken string) (*ListVolumesResult, error) {
	var cPageToken *C.char
	if pageToken != "" {
		cPageToken = C.CString(pageToken)
		defer C.free(unsafe.Pointer(cPageToken))
	}

	var cJson, cNextToken *C.char
	var totalCount C.uint64_t

	rc := C.cubecow_list_volumes(
		e.ptr, C.uint64_t(pageSize), cPageToken,
		&cJson, &cNextToken, &totalCount,
	)
	if rc != 0 {
		return nil, makeError(rc)
	}

	result := &ListVolumesResult{
		TotalCount: uint64(totalCount),
	}

	if cJson != nil {
		jsonStr := C.GoString(cJson)
		C.cubecow_free_string(cJson)
		if err := json.Unmarshal([]byte(jsonStr), &result.Volumes); err != nil {
			return nil, fmt.Errorf("failed to parse volumes JSON: %w", err)
		}
	}

	if cNextToken != nil {
		result.NextPageToken = C.GoString(cNextToken)
		C.cubecow_free_string(cNextToken)
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// Snapshot operations
// ---------------------------------------------------------------------------

// CreateSnapshot creates a snapshot from a volume or another snapshot.
// Returns the DM device path on success.
func (e *Engine) CreateSnapshot(sourceName, snapshotName string) (string, error) {
	cSource := C.CString(sourceName)
	defer C.free(unsafe.Pointer(cSource))
	cSnap := C.CString(snapshotName)
	defer C.free(unsafe.Pointer(cSnap))

	var cDevicePath *C.char
	// Activate the snapshot so callers immediately get a usable device
	// path. Consumers that want a metadata-only snapshot should pass
	// `false` here and later invoke `cubecow_activate_volume`.
	rc := C.cubecow_create_snapshot(e.ptr, cSource, cSnap, C.bool(true), &cDevicePath)
	if rc != 0 {
		return "", makeError(rc)
	}
	devicePath := C.GoString(cDevicePath)
	C.cubecow_free_string(cDevicePath)
	return devicePath, nil
}

// DeleteSnapshot deletes a snapshot by name.
func (e *Engine) DeleteSnapshot(snapshotName string) error {
	cSnap := C.CString(snapshotName)
	defer C.free(unsafe.Pointer(cSnap))

	rc := C.cubecow_delete_snapshot(e.ptr, cSnap)
	if rc != 0 {
		return makeError(rc)
	}
	return nil
}

// ListSnapshotsResult holds the result of a ListSnapshots call.
type ListSnapshotsResult struct {
	Snapshots     []Snapshot
	NextPageToken string
}

// ListSnapshots lists snapshots of a volume with pagination.
func (e *Engine) ListSnapshots(volumeName string, pageSize uint64, pageToken string) (*ListSnapshotsResult, error) {
	cVolName := C.CString(volumeName)
	defer C.free(unsafe.Pointer(cVolName))

	var cPageToken *C.char
	if pageToken != "" {
		cPageToken = C.CString(pageToken)
		defer C.free(unsafe.Pointer(cPageToken))
	}

	var cJson, cNextToken *C.char

	rc := C.cubecow_list_snapshots(
		e.ptr, cVolName, C.uint64_t(pageSize), cPageToken,
		&cJson, &cNextToken,
	)
	if rc != 0 {
		return nil, makeError(rc)
	}

	result := &ListSnapshotsResult{}

	if cJson != nil {
		jsonStr := C.GoString(cJson)
		C.cubecow_free_string(cJson)
		if err := json.Unmarshal([]byte(jsonStr), &result.Snapshots); err != nil {
			return nil, fmt.Errorf("failed to parse snapshots JSON: %w", err)
		}
	}

	if cNextToken != nil {
		result.NextPageToken = C.GoString(cNextToken)
		C.cubecow_free_string(cNextToken)
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

// GetMetrics retrieves all metrics as a map.
func (e *Engine) GetMetrics() (map[string]uint64, error) {
	var cJson *C.char

	rc := C.cubecow_get_metrics(e.ptr, &cJson)
	if rc != 0 {
		return nil, makeError(rc)
	}

	metrics := make(map[string]uint64)
	if cJson != nil {
		jsonStr := C.GoString(cJson)
		C.cubecow_free_string(cJson)
		if err := json.Unmarshal([]byte(jsonStr), &metrics); err != nil {
			return nil, fmt.Errorf("failed to parse metrics JSON: %w", err)
		}
	}

	return metrics, nil
}
