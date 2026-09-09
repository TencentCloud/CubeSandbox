// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	errorcode "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func installCleanupTemplateTestHooks(t *testing.T) {
	t.Helper()
	origGet := getLocalSnapshotForFn
	origList := listCubeboxesForTest
	origCow := cleanupIsCowBackend
	origLocal := cleanupTemplateLocalDataFn
	origRel := cleanupReleaseS3Metadata
	origObj := cleanupObjectsFor
	t.Cleanup(func() {
		getLocalSnapshotForFn = origGet
		listCubeboxesForTest = origList
		cleanupIsCowBackend = origCow
		cleanupTemplateLocalDataFn = origLocal
		cleanupReleaseS3Metadata = origRel
		cleanupObjectsFor = origObj
	})
	cleanupIsCowBackend = func() bool { return false }
	cleanupReleaseS3Metadata = func(context.Context, string, string) error { return nil }
	cleanupObjectsFor = func(context.Context, string, []storage.CowObjectRef) error { return nil }
}

func TestCleanupTemplateKeepsLivePauseAndHonorLiveFalseDeletes(t *testing.T) {
	installCleanupTemplateTestHooks(t)
	snap := "snap-keep-reg-0000000000000001"
	getLocalSnapshotForFn = func(_ context.Context, _, id string) (*storage.SnapshotCatalogEntry, error) {
		return &storage.SnapshotCatalogEntry{SnapshotID: id, Kind: storage.CatalogKindPauseSnapshot}, nil
	}
	live := newCubeboxWithStatusForTest("sb-live-keep", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	stampPauseSnapshotID(live, snap)
	live.AddLabels(map[string]string{constants.MasterAnnotationRuntimeRestoreSnapshotID: snap})
	listCubeboxesForTest = func() []*cubeboxstore.CubeBox { return []*cubeboxstore.CubeBox{live} }

	deleted := 0
	cleanupTemplateLocalDataFn = func(context.Context, string, string) error {
		deleted++
		return nil
	}

	s := &service{}
	req := &cubebox.CleanupTemplateRequest{
		TemplateID: snap,
		Objects:    []*cubebox.CowObjectRef{{Name: "n", Kind: "snapshot", Role: "rootfs"}},
	}
	rsp, err := s.cleanupTemplate(context.Background(), req, true)
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_Success, rsp.GetRet().GetRetCode(), rsp.GetRet().GetRetMsg())
	require.Equal(t, 0, deleted, "Master Resume Cleanup of a live pause package must no-op")

	rsp, err = s.cleanupTemplate(context.Background(), req, false)
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_Success, rsp.GetRet().GetRetCode(), rsp.GetRet().GetRetMsg())
	require.Equal(t, 1, deleted, "next Pause / Destroy GC (honorLive=false) must still delete")
}

func TestCleanupTemplateForgedPauseLabelDoesNotKeep(t *testing.T) {
	installCleanupTemplateTestHooks(t)
	snap := "snap-keep-forged-0000000000001"
	getLocalSnapshotForFn = func(_ context.Context, _, id string) (*storage.SnapshotCatalogEntry, error) {
		return &storage.SnapshotCatalogEntry{SnapshotID: id, Kind: storage.CatalogKindPauseSnapshot}, nil
	}
	attacker := newCubeboxWithStatusForTest("sb-attacker", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	stampPauseSnapshotID(attacker, snap)
	listCubeboxesForTest = func() []*cubeboxstore.CubeBox { return []*cubeboxstore.CubeBox{attacker} }

	deleted := 0
	cleanupTemplateLocalDataFn = func(context.Context, string, string) error {
		deleted++
		return nil
	}

	s := &service{}
	req := &cubebox.CleanupTemplateRequest{
		TemplateID: snap,
		Objects:    []*cubebox.CowObjectRef{{Name: "n", Kind: "snapshot", Role: "rootfs"}},
	}
	rsp, err := s.cleanupTemplate(context.Background(), req, true)
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_Success, rsp.GetRet().GetRetCode(), rsp.GetRet().GetRetMsg())
	require.Equal(t, 1, deleted, "forged pause-id Label must not block origin GC of another sandbox's package")
}
