// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandboxstatus

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/pkgs/sandboxrestart"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newTestDB(t *testing.T) {
	t.Helper()
	name := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	client, err := gorm.Open(sqlite.Open(name), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, client.AutoMigrate(&models.SandboxStatus{}))
	require.NoError(t, Init(client))
	t.Cleanup(func() {
		dbMu.Lock()
		db = nil
		dbMu.Unlock()
	})
}

func TestUpsertIgnoresOlderSeqAndSoftDelete(t *testing.T) {
	newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	code := int32(1)
	applied, _, err := Upsert(ctx, &models.SandboxStatus{
		SandboxID: "sb-1", HostID: "host-a", Phase: "Failed",
		RestartPolicy: "OnFailure", RestartState: "BackOff",
		RestartCount: 1, LastExitCode: &code, LastExitReason: "Error",
		StatusSeq: 2, ReportedAt: now,
	})
	require.NoError(t, err)
	require.True(t, applied)

	applied, _, err = Upsert(ctx, &models.SandboxStatus{
		SandboxID: "sb-1", HostID: "host-stale", RestartState: "Running",
		StatusSeq: 1, ReportedAt: now,
	})
	require.NoError(t, err)
	require.False(t, applied)
	got, err := Get(ctx, "sb-1")
	require.NoError(t, err)
	require.Equal(t, "host-a", got.HostID)
	require.Equal(t, int64(2), got.StatusSeq)
	require.Equal(t, "BackOff", got.RestartState)

	require.NoError(t, SoftDelete(ctx, "sb-1"))
	_, err = Get(ctx, "sb-1")
	require.ErrorIs(t, err, ErrNotFound)

	applied, _, err = Upsert(ctx, &models.SandboxStatus{
		SandboxID: "sb-1", HostID: "host-b", RestartState: "Running",
		StatusSeq: 3, ReportedAt: now.Add(time.Second),
	})
	require.NoError(t, err)
	require.False(t, applied)
	_, err = Get(ctx, "sb-1")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSoftDeleteWithoutRowBlocksLaterReport(t *testing.T) {
	newTestDB(t)
	ctx := context.Background()
	require.NoError(t, SoftDelete(ctx, "sb-gone"))
	applied, _, err := Upsert(ctx, &models.SandboxStatus{
		SandboxID: "sb-gone", HostID: "host-a", RestartState: "BackOff",
		StatusSeq: 3, ReportedAt: time.Now(),
	})
	require.NoError(t, err)
	require.False(t, applied)
	_, err = Get(ctx, "sb-gone")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDisplayPolicyUsesK8sNames(t *testing.T) {
	for _, raw := range []string{"RESTART_POLICY_ON_FAILURE", "always", "", "nope"} {
		if got, want := DisplayPolicy(raw), sandboxrestart.Display(raw); got != want {
			t.Fatalf("DisplayPolicy(%q)=%q want %q", raw, got, want)
		}
	}
}

func TestBatchGetOmitsMissing(t *testing.T) {
	newTestDB(t)
	ctx := context.Background()
	_, _, err := Upsert(ctx, &models.SandboxStatus{SandboxID: "sb-1", StatusSeq: 1, ReportedAt: time.Now()})
	require.NoError(t, err)
	_, _, err = Upsert(ctx, &models.SandboxStatus{SandboxID: "sb-2", StatusSeq: 1, ReportedAt: time.Now()})
	require.NoError(t, err)
	got, err := BatchGet(ctx, []string{"sb-2", "missing", "sb-1"})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Contains(t, got, "sb-1")
	require.Contains(t, got, "sb-2")
}
