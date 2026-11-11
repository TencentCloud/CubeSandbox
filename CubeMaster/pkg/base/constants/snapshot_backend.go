// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package constants

import (
	"fmt"
	"strings"
)

const (
	// SnapshotBackendXFS is the default local CoW backend.
	SnapshotBackendXFS = "xfs"
	// SnapshotBackendS3 is the cluster-shared S3 backend.
	SnapshotBackendS3 = "s3"
	// SnapshotBackendJFS keeps volumes and snapshots directly on a JuiceFS
	// volume every node mounts. Another node restores a package straight
	// from the origin's tree once JuiceFS has uploaded its blocks.
	SnapshotBackendJFS = "jfs"

	// RemoteStatus* are remote publish states on snapshot / pause-snapshot
	// rows (s3 upload, jfs). Empty on xfs. Create starts pending;
	// after Pause/Commit persist uuids the row becomes inprogress; a
	// background loop queries Cubelet Status and writes ready/failed.
	RemoteStatusPending    = "pending"
	RemoteStatusInProgress = "inprogress"
	RemoteStatusRunning    = "running"
	RemoteStatusReady      = "ready"
	RemoteStatusFailed     = "failed"
)

// NormalizeSnapshotBackend maps create/pause backend input onto xfs | s3 | jfs.
// Empty and historical cubecow aliases become xfs.
func NormalizeSnapshotBackend(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", SnapshotBackendXFS, "cow", "cubecow", "reflink", "xfscow":
		return SnapshotBackendXFS, nil
	case SnapshotBackendS3:
		return SnapshotBackendS3, nil
	case SnapshotBackendJFS, "juicefs":
		return SnapshotBackendJFS, nil
	default:
		return "", fmt.Errorf("unsupported backend %q (want %q, %q or %q)", raw,
			SnapshotBackendXFS, SnapshotBackendS3, SnapshotBackendJFS)
	}
}

// ResolveSnapshotBackend returns the first non-empty candidate after
// NormalizeSnapshotBackend. All-empty input is xfs (Cubelet default).
func ResolveSnapshotBackend(values ...string) (string, error) {
	if normalized, ok, err := OptionalSnapshotBackend(values...); err != nil || ok {
		return normalized, err
	}
	return SnapshotBackendXFS, nil
}

// OptionalSnapshotBackend returns the first non-empty candidate after
// NormalizeSnapshotBackend. All-empty input is ("", false, nil) so callers
// can leave existing requests untouched.
func OptionalSnapshotBackend(values ...string) (string, bool, error) {
	for _, raw := range values {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		normalized, err := NormalizeSnapshotBackend(raw)
		if err != nil {
			return "", false, err
		}
		return normalized, true, nil
	}
	return "", false, nil
}

// IsS3Backend reports whether backend is the cluster-shared S3 store.
// Empty / cubecow aliases are xfs and return false.
func IsS3Backend(backend string) bool {
	normalized, err := NormalizeSnapshotBackend(backend)
	return err == nil && normalized == SnapshotBackendS3
}

// IsJFSBackend reports whether backend is the reflink store on JuiceFS.
func IsJFSBackend(backend string) bool {
	normalized, err := NormalizeSnapshotBackend(backend)
	return err == nil && normalized == SnapshotBackendJFS
}

// IsRemoteTrackedBackend reports whether rows of backend carry
// remote_status / export_uuids (s3 export, jfs shared volume).
func IsRemoteTrackedBackend(backend string) bool {
	return IsS3Backend(backend) || IsJFSBackend(backend)
}

// RemoteTrackedBackends lists the backends the remote_status poller scans.
func RemoteTrackedBackends() []string {
	return []string{SnapshotBackendS3, SnapshotBackendJFS}
}

// SnapshotRemoteStatus returns the default remote_status for a normalized
// backend. s3 and jfs start pending; xfs leaves the column empty.
func SnapshotRemoteStatus(backend string) string {
	if IsRemoteTrackedBackend(backend) {
		return RemoteStatusPending
	}
	return ""
}
