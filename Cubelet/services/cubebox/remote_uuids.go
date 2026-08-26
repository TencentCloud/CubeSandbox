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

// uploadRemoteUUIDsIfS3 runs cubecow_export_snapshot (via UploadSnapshot)
// after Pause / CommitSandbox has already sealed package disks to
// snapshots. Templates (AppSnapshot) do not export. XFS is a no-op.
// Export failure is logged and returns empty — the customer-facing RPC
// still succeeds; Master records remote_status=failed without uuids
// (same-node resume does not need them; cross-node needs ready + uuids).
func uploadRemoteUUIDsIfS3(ctx context.Context, backend, snapshotID string) string {
	normalized, err := cow.NormalizeBackend(backend)
	if err != nil || normalized != cow.BackendS3 {
		return ""
	}
	uuids, err := storage.UploadSnapshot(ctx, normalized, snapshotID)
	if err != nil {
		log.G(ctx).Warnf("s3 export snapshot %s failed (customer RPC continues): %v", snapshotID, err)
		return ""
	}
	raw := uuids.JSON()
	if raw == "" {
		log.G(ctx).Warnf("s3 export snapshot %s returned empty remote_uuids (customer RPC continues)", snapshotID)
		return ""
	}
	return raw
}
