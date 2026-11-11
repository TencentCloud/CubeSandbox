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
// Pause / CommitSandbox has already sealed its disks to snapshots. S3 runs
// cubecow_export_snapshot (via UploadSnapshot). JFS needs no export because
// every node opens the same namespace; its writeback readiness is polled
// separately through the snapshot status service.
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
