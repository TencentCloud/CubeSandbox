// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build cgo

package cubecow

/*
#cgo CFLAGS: -I${SRCDIR}/../../third_party/cubecow/include
#cgo LDFLAGS: ${SRCDIR}/../../third_party/cubecow/lib/libcubecow.a -ldl -lpthread -lm -lrt
#include "cubecow.h"
#include <stdlib.h>

static inline int32_t cubecow_create_snapshot_go(
    CubecowEngineHandle engine,
    const char* source_name,
    const char* snapshot_name,
    int activate,
    char** out_device_path
) {
    return cubecow_create_snapshot(engine, source_name, snapshot_name, activate != 0, out_device_path);
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"runtime"
	"unsafe"
)

func Init(configPath string) (*Engine, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	cPath := C.CString(configPath)
	defer C.free(unsafe.Pointer(cPath))

	ptr := C.cubecow_init(cPath)
	if ptr == nil {
		return nil, fmt.Errorf("cubecow_init failed: %s", lastError())
	}
	return &Engine{ptr: unsafe.Pointer(ptr)}, nil
}

func InitWithoutLogging(configPath string) (*Engine, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	cPath := C.CString(configPath)
	defer C.free(unsafe.Pointer(cPath))

	ptr := C.cubecow_init_without_logging(cPath)
	if ptr == nil {
		return nil, fmt.Errorf("cubecow_init_without_logging failed: %s", lastError())
	}
	return &Engine{ptr: unsafe.Pointer(ptr)}, nil
}

func InitFromJSON(configJSON string) (*Engine, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	cJSON := C.CString(configJSON)
	defer C.free(unsafe.Pointer(cJSON))

	ptr := C.cubecow_init_from_json(cJSON)
	if ptr == nil {
		return nil, fmt.Errorf("cubecow_init_from_json failed: %s", lastError())
	}
	return &Engine{ptr: unsafe.Pointer(ptr)}, nil
}

func InitWithoutLoggingFromJSON(configJSON string) (*Engine, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	cJSON := C.CString(configJSON)
	defer C.free(unsafe.Pointer(cJSON))

	ptr := C.cubecow_init_without_logging_from_json(cJSON)
	if ptr == nil {
		return nil, fmt.Errorf("cubecow_init_without_logging_from_json failed: %s", lastError())
	}
	return &Engine{ptr: unsafe.Pointer(ptr)}, nil
}

func (e *Engine) Close() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ptr == nil {
		return
	}
	C.cubecow_shutdown(C.CubecowEngineHandle(e.ptr))
	e.ptr = nil
}

func (e *Engine) ResetNodeStorage() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ptr, err := e.openHandle()
	if err != nil {
		return err
	}
	defer e.mu.RUnlock()

	rc := C.cubecow_reset_node_storage(C.CubecowEngineHandle(ptr))
	if rc != 0 {
		return makeError(rc)
	}
	return nil
}

func (e *Engine) CreateVolume(name string, sizeBytes uint64) (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ptr, err := e.openHandle()
	if err != nil {
		return "", err
	}
	defer e.mu.RUnlock()

	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	var cDevicePath *C.char
	rc := C.cubecow_create_volume(C.CubecowEngineHandle(ptr), cName, C.uint64_t(sizeBytes), &cDevicePath)
	if rc != 0 {
		return "", makeError(rc)
	}
	return takeCString(cDevicePath), nil
}

func (e *Engine) DeleteVolume(name string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ptr, err := e.openHandle()
	if err != nil {
		return err
	}
	defer e.mu.RUnlock()

	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	rc := C.cubecow_delete_volume(C.CubecowEngineHandle(ptr), cName)
	if rc != 0 {
		return makeError(rc)
	}
	return nil
}

func (e *Engine) ResizeVolume(name string, newSizeBytes uint64) (uint64, uint64, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ptr, err := e.openHandle()
	if err != nil {
		return 0, 0, err
	}
	defer e.mu.RUnlock()

	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	var oldSize C.uint64_t
	var newSize C.uint64_t
	rc := C.cubecow_resize_volume(C.CubecowEngineHandle(ptr), cName, C.uint64_t(newSizeBytes), &oldSize, &newSize)
	if rc != 0 {
		return 0, 0, makeError(rc)
	}
	return uint64(oldSize), uint64(newSize), nil
}

func (e *Engine) GetVolumeInfo(name string) (*Volume, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ptr, err := e.openHandle()
	if err != nil {
		return nil, err
	}
	defer e.mu.RUnlock()

	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	var sizeBytes C.uint64_t
	var cDevicePath *C.char
	var snapCount C.int32_t
	var cCreatedAt *C.char

	rc := C.cubecow_get_volume_info(C.CubecowEngineHandle(ptr), cName, &sizeBytes, &cDevicePath, &snapCount, &cCreatedAt)
	if rc != 0 {
		return nil, makeError(rc)
	}

	return &Volume{
		Name:          name,
		SizeBytes:     uint64(sizeBytes),
		DevicePath:    takeCString(cDevicePath),
		SnapshotCount: int32(snapCount),
		CreatedAt:     takeCString(cCreatedAt),
	}, nil
}

func (e *Engine) GetVolumeBlockInfo(name string) (*VolumeBlockInfo, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ptr, err := e.openHandle()
	if err != nil {
		return nil, err
	}
	defer e.mu.RUnlock()

	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	var numBlocks C.uint64_t
	var blockSize C.uint32_t
	rc := C.cubecow_get_volume_block_info(C.CubecowEngineHandle(ptr), cName, &numBlocks, &blockSize)
	if rc != 0 {
		return nil, makeError(rc)
	}
	return &VolumeBlockInfo{
		NumBlocks: uint64(numBlocks),
		BlockSize: uint32(blockSize),
	}, nil
}

func (e *Engine) ListVolumes(pageSize uint64, pageToken string) (*ListVolumesResult, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ptr, err := e.openHandle()
	if err != nil {
		return nil, err
	}
	defer e.mu.RUnlock()

	var cPageToken *C.char
	if pageToken != "" {
		cPageToken = C.CString(pageToken)
		defer C.free(unsafe.Pointer(cPageToken))
	}

	var cJSON *C.char
	var cNextToken *C.char
	var totalCount C.uint64_t
	rc := C.cubecow_list_volumes(C.CubecowEngineHandle(ptr), C.uint64_t(pageSize), cPageToken, &cJSON, &cNextToken, &totalCount)
	if rc != 0 {
		return nil, makeError(rc)
	}

	result := &ListVolumesResult{TotalCount: uint64(totalCount)}
	jsonStr := takeCString(cJSON)
	if jsonStr != "" {
		if err := json.Unmarshal([]byte(jsonStr), &result.Volumes); err != nil {
			return nil, fmt.Errorf("unmarshal cubecow_list_volumes response: %w", err)
		}
	}
	result.NextPageToken = takeCString(cNextToken)
	return result, nil
}

func (e *Engine) CreateSnapshot(sourceName, snapshotName string, activate bool) (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ptr, err := e.openHandle()
	if err != nil {
		return "", err
	}
	defer e.mu.RUnlock()

	cSourceName := C.CString(sourceName)
	defer C.free(unsafe.Pointer(cSourceName))
	cSnapshotName := C.CString(snapshotName)
	defer C.free(unsafe.Pointer(cSnapshotName))

	var cDevicePath *C.char
	rc := C.cubecow_create_snapshot_go(C.CubecowEngineHandle(ptr), cSourceName, cSnapshotName, cBoolInt(activate), &cDevicePath)
	if rc != 0 {
		return "", makeError(rc)
	}
	return takeCString(cDevicePath), nil
}

func (e *Engine) ActivateVolume(name string) (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ptr, err := e.openHandle()
	if err != nil {
		return "", err
	}
	defer e.mu.RUnlock()

	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	var cDevicePath *C.char
	rc := C.cubecow_activate_volume(C.CubecowEngineHandle(ptr), cName, &cDevicePath)
	if rc != 0 {
		return "", makeError(rc)
	}
	return takeCString(cDevicePath), nil
}

func (e *Engine) DeactivateVolume(name string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ptr, err := e.openHandle()
	if err != nil {
		return err
	}
	defer e.mu.RUnlock()

	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	rc := C.cubecow_deactivate_volume(C.CubecowEngineHandle(ptr), cName)
	if rc != 0 {
		return makeError(rc)
	}
	return nil
}

func (e *Engine) DeleteSnapshot(snapshotName string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ptr, err := e.openHandle()
	if err != nil {
		return err
	}
	defer e.mu.RUnlock()

	cSnapshotName := C.CString(snapshotName)
	defer C.free(unsafe.Pointer(cSnapshotName))

	rc := C.cubecow_delete_snapshot(C.CubecowEngineHandle(ptr), cSnapshotName)
	if rc != 0 {
		return makeError(rc)
	}
	return nil
}

func (e *Engine) ListSnapshots(volumeName string, pageSize uint64, pageToken string) (*ListSnapshotsResult, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ptr, err := e.openHandle()
	if err != nil {
		return nil, err
	}
	defer e.mu.RUnlock()

	cVolumeName := C.CString(volumeName)
	defer C.free(unsafe.Pointer(cVolumeName))

	var cPageToken *C.char
	if pageToken != "" {
		cPageToken = C.CString(pageToken)
		defer C.free(unsafe.Pointer(cPageToken))
	}

	var cJSON *C.char
	var cNextToken *C.char
	rc := C.cubecow_list_snapshots(C.CubecowEngineHandle(ptr), cVolumeName, C.uint64_t(pageSize), cPageToken, &cJSON, &cNextToken)
	if rc != 0 {
		return nil, makeError(rc)
	}

	result := &ListSnapshotsResult{}
	jsonStr := takeCString(cJSON)
	if jsonStr != "" {
		if err := json.Unmarshal([]byte(jsonStr), &result.Snapshots); err != nil {
			return nil, fmt.Errorf("unmarshal cubecow_list_snapshots response: %w", err)
		}
	}
	result.NextPageToken = takeCString(cNextToken)
	return result, nil
}

func (e *Engine) GetMetrics() (map[string]uint64, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ptr, err := e.openHandle()
	if err != nil {
		return nil, err
	}
	defer e.mu.RUnlock()

	var cJSON *C.char
	rc := C.cubecow_get_metrics(C.CubecowEngineHandle(ptr), &cJSON)
	if rc != 0 {
		return nil, makeError(rc)
	}

	metrics := make(map[string]uint64)
	jsonStr := takeCString(cJSON)
	if jsonStr == "" {
		return metrics, nil
	}
	if err := json.Unmarshal([]byte(jsonStr), &metrics); err != nil {
		return nil, fmt.Errorf("unmarshal cubecow_get_metrics response: %w", err)
	}
	return metrics, nil
}

func cBoolInt(v bool) C.int {
	if v {
		return 1
	}
	return 0
}

func lastError() string {
	p := C.cubecow_last_error()
	if p == nil {
		return "unknown error"
	}
	return C.GoString(p)
}

func makeError(rc C.int32_t) error {
	code, action := MapError(int32(rc))
	return &CowError{
		Code:    code,
		Action:  action,
		RawRC:   int32(rc),
		Message: lastError(),
	}
}

func takeCString(s *C.char) string {
	if s == nil {
		return ""
	}
	goString := C.GoString(s)
	C.cubecow_free_string(s)
	return goString
}
