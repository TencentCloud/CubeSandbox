// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rollback restore only replaces the host path and keeps the rate limit; the
// saved VMM snapshot config stays authoritative for discard, so the request
// JSON must never carry a discard key even when the old snapshot spec has one.
func TestRollbackDisksExcludeDiscardAndReplaceRootfs(t *testing.T) {
	spec := &CubeboxSnapshotSpec{
		Disk: json.RawMessage(`[
			{"path":"/dev/mapper/old-root","volume_source":"cube_rootfs_rw","discard":true,"legacy_extra":"ignored",
			 "rate_limiter_config":{"bandwidth":{"size":1048576,"one_time_burst":0,"refill_time":1000}}},
			{"path":"/dev/mapper/data","discard":true}
		]`),
	}

	disks, err := rollbackDisksFromSnapshotSpec(spec, "cube_rootfs_rw", "/dev/mapper/old-root", "/dev/mapper/new-root")
	require.NoError(t, err)

	body, err := json.Marshal(disks)
	require.NoError(t, err)
	assert.JSONEq(t, `[
		{"path":"/dev/mapper/new-root","id":"disk-0",
		 "rate_limiter_config":{"bandwidth":{"size":1048576,"one_time_burst":0,"refill_time":1000}}},
		{"path":"/dev/mapper/data","id":"disk-1"}
	]`, string(body), "rollback JSON must replace the rootfs path and rate limit but never project discard")
}
