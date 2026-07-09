// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/google/uuid"
)

const (
	// Host directories for OpenClaw shared-files persistence.
	// Must be under CubeMaster's allowed_host_mount_prefixes (default: /data/shared/).
	openclawHostStateRoot     = "/data/shared/agenthub/openclaw"
	openclawHostSnapshotRoot  = "/data/shared/agenthub/openclaw-snapshots"
	openclawSandboxStatePath  = "/root/.openclaw"

	// Metadata key for host directory mount in sandbox labels.
	// Matches old Rust HOSTDIR_MOUNT_KEY.
	hostdirMountKey = "host-mount"
)

// newOpenclawPersistID generates a new persist ID (UUID without hyphens).
// Matches old Rust new_openclaw_persist_id.
func newOpenclawPersistID() string {
	return uuid.New().String()
}

// openclawHostStatePath returns the host path for an active OpenClaw state directory.
// Matches old Rust openclaw_host_state_path.
func openclawHostStatePath(persistID string) string {
	return filepath.Join(openclawHostStateRoot, persistID)
}

// openclawHostSnapshotPath returns the host path for a snapshot OpenClaw state directory.
// Matches old Rust openclaw_host_snapshot_path.
func openclawHostSnapshotPath(snapshotID string) string {
	return filepath.Join(openclawHostSnapshotRoot, snapshotID)
}

// prepareOpenclawStateDir creates the host directory for an OpenClaw state.
// Matches old Rust prepare_openclaw_state_dir.
func prepareOpenclawStateDir(persistID string) (string, error) {
	path := openclawHostStatePath(persistID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", fmt.Errorf("failed to create OpenClaw state directory %s: %w", path, err)
	}
	return path, nil
}

// copyOpenclawStateDir copies the contents of source dir to target dir using rsync.
// Matches old Rust copy_openclaw_state_dir_blocking.
// If source is empty or doesn't exist, it's a no-op.
func copyOpenclawStateDir(source, target string) error {
	if source == "" {
		return nil
	}
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		return nil
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("failed to create target OpenClaw state directory %s: %w", target, err)
	}
	cmd := exec.Command("rsync", "-a", "--delete", source+"/", target)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rsync OpenClaw state %s -> %s failed: %w: %s", source, target, err, string(output))
	}
	return nil
}

// openclawHostMountMetadata builds the JSON metadata for a host directory mount.
// Matches old Rust openclaw_host_mount_metadata.
// Returns a JSON array: [{"hostPath": "...", "mountPath": "/root/.openclaw"}]
func openclawHostMountMetadata(hostPath string) (string, error) {
	mounts := []map[string]string{
		{"hostPath": hostPath, "mountPath": openclawSandboxStatePath},
	}
	data, err := json.Marshal(mounts)
	if err != nil {
		return "", fmt.Errorf("failed to encode OpenClaw host mount metadata: %w", err)
	}
	return string(data), nil
}

// agenthubDistributionScope returns the distribution scope for a sandbox.
// For shared_files mode or template source, restricts to the current node.
// Matches old Rust agenthub_create_distribution_scope + agenthub_distribution_scope.
func agenthubDistributionScope(persistenceMode, rootfsSourceType string) []string {
	// Snapshot source with non-shared-files mode → no restriction (can be on any node)
	if rootfsSourceType == "snapshot" && persistenceMode != "shared_files" {
		return nil
	}
	// Otherwise, restrict to current node (host mount is node-local)
	nodeID := os.Getenv("AGENTHUB_HOST_MOUNT_NODE_ID")
	if nodeID == "" {
		nodeID = os.Getenv("CUBE_SANDBOX_NODE_IP")
	}
	if nodeID == "" {
		return nil
	}
	return []string{nodeID}
}
