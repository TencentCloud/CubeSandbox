// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
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
	live.BindRestoreBase(snap)
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

func TestCleanupTemplateJFSOnNodeWithoutJFSIsANoop(t *testing.T) {
	installCleanupTemplateTestHooks(t)
	origJFS := cleanupJFSConfigured
	t.Cleanup(func() { cleanupJFSConfigured = origJFS })
	getLocalSnapshotForFn = func(context.Context, string, string) (*storage.SnapshotCatalogEntry, error) {
		return nil, storage.ErrSnapshotCatalogNotFound
	}
	cleanupIsCowBackend = func() bool { return true }
	swept := 0
	cleanupObjectsFor = func(context.Context, string, []storage.CowObjectRef) error {
		swept++
		return storage.ErrJFSNotConfigured
	}
	req := &cubebox.CleanupTemplateRequest{TemplateID: "tpl-shared-0000000000001", Backend: "jfs"}

	cleanupJFSConfigured = func() bool { return false }
	rsp, err := (&service{}).cleanupTemplate(context.Background(), req, true)
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_Success, rsp.GetRet().GetRetCode(), rsp.GetRet().GetRetMsg())
	require.Zero(t, swept)

	cleanupJFSConfigured = func() bool { return true }
	rsp, err = (&service{}).cleanupTemplate(context.Background(), req, true)
	require.NoError(t, err)
	require.NotEqual(t, errorcode.ErrorCode_Success, rsp.GetRet().GetRetCode())
	require.Equal(t, 1, swept)
}

func TestCleanupTemplateExcludesS3WritersUntilCleanupReturns(t *testing.T) {
	installCleanupTemplateTestHooks(t)
	id := "tpl-cleanup-writer"
	getLocalSnapshotForFn = func(context.Context, string, string) (*storage.SnapshotCatalogEntry, error) {
		return &storage.SnapshotCatalogEntry{SnapshotID: id}, nil
	}
	listCubeboxesForTest = func() []*cubeboxstore.CubeBox { return nil }
	cleanupIsCowBackend = func() bool { return true }
	metadataCalls, objectCalls, localCalls := 0, 0, 0
	assertWriterExcluded := func() {
		t.Helper()
		_, release, err := storage.BeginS3PackageWrite(context.Background(), cow.BackendS3, id)
		if release != nil {
			release()
		}
		require.ErrorContains(t, err, "active cleanup")
	}
	cleanupReleaseS3Metadata = func(context.Context, string, string) error {
		assertWriterExcluded()
		metadataCalls++
		return nil
	}
	failObjects := true
	cleanupObjectsFor = func(context.Context, string, []storage.CowObjectRef) error {
		assertWriterExcluded()
		objectCalls++
		if failObjects {
			return errors.New("object cleanup failed")
		}
		return nil
	}
	cleanupTemplateLocalDataFn = func(context.Context, string, string) error {
		assertWriterExcluded()
		localCalls++
		return nil
	}
	req := &cubebox.CleanupTemplateRequest{
		TemplateID: id, Backend: cow.BackendS3, SnapshotPath: t.TempDir(),
		Objects: []*cubebox.CowObjectRef{{Name: "rootfs", Kind: "snapshot", Role: "rootfs"}},
	}
	_, release, err := storage.BeginS3PackageWrite(context.Background(), cow.BackendS3, id)
	require.NoError(t, err)
	t.Cleanup(release)
	s := &service{}
	rsp, err := s.cleanupTemplate(context.Background(), req, true)
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_PreConditionFailed, rsp.GetRet().GetRetCode())
	require.Zero(t, metadataCalls+objectCalls+localCalls)
	release()

	rsp, err = s.cleanupTemplate(context.Background(), req, true)
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_Unknown, rsp.GetRet().GetRetCode())
	require.Equal(t, 1, metadataCalls)
	require.Equal(t, 1, objectCalls)
	require.Zero(t, localCalls)
	require.False(t, storage.S3PackageOperationInProgress(id), "failed cleanup must release the guard")

	failObjects = false
	rsp, err = s.cleanupTemplate(context.Background(), req, true)
	require.NoError(t, err)
	require.Equal(t, errorcode.ErrorCode_Success, rsp.GetRet().GetRetCode(), rsp.GetRet().GetRetMsg())
	require.Equal(t, 2, metadataCalls)
	require.Equal(t, 2, objectCalls)
	require.Equal(t, 1, localCalls)
	require.False(t, storage.S3PackageOperationInProgress(id))
}
