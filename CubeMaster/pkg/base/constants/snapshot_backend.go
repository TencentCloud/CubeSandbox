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

	// RemoteStatus* are S3 upload states on snapshot / pause-snapshot rows.
	// Empty on xfs (remote_status is S3-only). Create starts pending; after
	// Pause／Commit persist uuids the row becomes inprogress; a background
	// loop queries Cubelet Status and writes ready／failed.
	RemoteStatusPending    = "pending"
	RemoteStatusInProgress = "inprogress"
	RemoteStatusRunning    = "running"
	RemoteStatusReady      = "ready"
	RemoteStatusFailed     = "failed"
)

// NormalizeSnapshotBackend maps create/pause backend input onto xfs | s3.
// Empty and historical cubecow aliases become xfs.
func NormalizeSnapshotBackend(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", SnapshotBackendXFS, "cow", "cubecow", "reflink", "xfscow":
		return SnapshotBackendXFS, nil
	case SnapshotBackendS3:
		return SnapshotBackendS3, nil
	default:
		return "", fmt.Errorf("unsupported backend %q (want %q or %q)", raw, SnapshotBackendXFS, SnapshotBackendS3)
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

// SnapshotRemoteStatus returns the default remote_status for a normalized
// backend. S3 starts pending; xfs leaves the column empty.
func SnapshotRemoteStatus(backend string) string {
	if IsS3Backend(backend) {
		return RemoteStatusPending
	}
	return ""
}
