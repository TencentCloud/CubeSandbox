// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubecow

import (
	"sync"
	"unsafe"
)

type Engine struct {
	ptr unsafe.Pointer
	mu  sync.RWMutex
}

type Volume struct {
	Name          string `json:"name"`
	SizeBytes     uint64 `json:"size_bytes"`
	DevicePath    string `json:"device_path"`
	SnapshotCount int32  `json:"snapshot_count"`
	CreatedAt     string `json:"created_at"`
}

type Snapshot struct {
	Name         string `json:"name"`
	SizeBytes    uint64 `json:"size_bytes"`
	DevicePath   string `json:"device_path"`
	OriginVolume string `json:"origin_volume"`
	CreatedAt    string `json:"created_at"`
}

type VolumeBlockInfo struct {
	NumBlocks uint64
	BlockSize uint32
}

type ListVolumesResult struct {
	Volumes       []Volume
	NextPageToken string
	TotalCount    uint64
}

type ListSnapshotsResult struct {
	Snapshots     []Snapshot
	NextPageToken string
}

func (e *Engine) openHandle() (unsafe.Pointer, error) {
	if e == nil {
		return nil, &CowError{Code: ErrClosed, Action: ActBug, RawRC: int32(ErrClosed), Message: "engine is nil"}
	}
	e.mu.RLock()
	if e.ptr == nil {
		e.mu.RUnlock()
		return nil, &CowError{Code: ErrClosed, Action: ActBug, RawRC: int32(ErrClosed), Message: "engine is closed"}
	}
	return e.ptr, nil
}
