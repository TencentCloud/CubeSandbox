// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cow

import (
	"context"
	"encoding/json"
	"strings"
)

// Remote state values for snapshot cross-node readiness (Master remote_status).
const (
	RemoteStatePending = "pending" // never uploaded / cubecow NONE
	RemoteStateRunning = "running" // upload in progress / cubecow INPROGRESS
	RemoteStateReady   = "ready"   // remote_uuids present / cubecow DONE
	RemoteStateFailed  = "failed"
)

// cubecow_get_volume_info export_status values (s3-support).
const (
	ExportStatusNone       = "NONE"
	ExportStatusInProgress = "INPROGRESS"
	ExportStatusDone       = "DONE"
)

// RemoteUUIDs is the JSON blob Master stores so another node can Fetch.
// One cubecow uuid per disk that exists today: rootfs / memory / metadata.
type RemoteUUIDs struct {
	Rootfs   string `json:"rootfs,omitempty"`
	Memory   string `json:"memory,omitempty"`
	Metadata string `json:"metadata,omitempty"`
}

func (u *RemoteUUIDs) Empty() bool {
	if u == nil {
		return true
	}
	return strings.TrimSpace(u.Rootfs) == "" && strings.TrimSpace(u.Memory) == "" && strings.TrimSpace(u.Metadata) == ""
}

func (u *RemoteUUIDs) JSON() string {
	if u == nil || u.Empty() {
		return ""
	}
	raw, err := json.Marshal(u)
	if err != nil {
		return ""
	}
	return string(raw)
}

func ParseRemoteUUIDs(raw string) *RemoteUUIDs {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var u RemoteUUIDs
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		return nil
	}
	if u.Empty() {
		return nil
	}
	return &u
}

// VolumeRemoteInfo is local cubecow_get_volume_info for one snapshot disk.
// device_path empty means the snapshot volume exists but is not activated.
type VolumeRemoteInfo struct {
	Name       string `json:"name"`
	Role       string `json:"role"`
	Exists     bool   `json:"exists"`
	DevicePath string `json:"device_path,omitempty"`
}

// RemoteStatus is upload progress plus local volume info.
// State comes from cubecow export_status (NONE/empty/INPROGRESS/DONE).
type RemoteStatus struct {
	SnapshotID      string             `json:"snapshot_id"`
	State           string             `json:"state"`
	Message         string             `json:"message,omitempty"`
	RemoteUUIDs     *RemoteUUIDs       `json:"remote_uuids,omitempty"`
	LocalVolumes    []VolumeRemoteInfo `json:"local_volumes,omitempty"`
	RootfsDeletable *bool              `json:"rootfs_deletable,omitempty"`
}

// Uploader publishes local snapshot volumes to the remote store
// (cubecow_export_snapshot). XFS Stores do not implement it.
type Uploader interface {
	Upload(ctx context.Context, snapshotID string) (*RemoteUUIDs, error)
	UploadStatus(ctx context.Context, snapshotID string) (*RemoteStatus, error)
}

// Activator opens a snapshot that already exists on this node
// (cubecow_activate_volume). Snapshot-created volumes are inactive by
// default; ordinary volumes already have a device path.
type Activator interface {
	Activate(ctx context.Context, snapshotID string) error
}
