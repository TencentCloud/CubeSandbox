// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// uploadRemoteUUIDs publishes a package another node can restore from, after
// Pause / CommitSandbox has already sealed its disks to snapshots. s3 runs
// cubecow_export_snapshot (via UploadSnapshot); jfs reports the package's
// paths on the shared volume. Templates (AppSnapshot) do not export, and xfs
// is a no-op. Failure is logged and returns empty — the customer-facing RPC
// still succeeds; Master records remote_status=failed without uuids
// (same-node resume does not need them; cross-node needs ready + uuids).
func uploadRemoteUUIDs(ctx context.Context, backend, snapshotID string) string {
	normalized, err := cow.NormalizeBackend(backend)
	if err != nil || !storage.CrossNodeImportBackend(normalized) {
		return ""
	}
	uuids, err := storage.UploadSnapshot(ctx, normalized, snapshotID)
	if err != nil {
		log.G(ctx).Warnf("%s export snapshot %s failed (customer RPC continues): %v", normalized, snapshotID, err)
		return ""
	}
	raw := uuids.JSON()
	if raw == "" {
		log.G(ctx).Warnf("%s export snapshot %s returned empty remote_uuids (customer RPC continues)", normalized, snapshotID)
		return ""
	}
	return raw
}
