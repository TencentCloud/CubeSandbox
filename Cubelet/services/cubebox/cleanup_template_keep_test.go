// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

func TestCleanupTemplateRetiresOnlyXFSS3PausePackages(t *testing.T) {
	for _, tc := range []struct {
		name, backend, kind string
		marker, keep        bool
		wantRetired         int
	}{
		{name: "template", backend: "xfs-s3", kind: storage.CatalogKindTemplate},
		{name: "missing-template", backend: "xfs-s3"},
		{name: "pause-catalog", backend: "xfs-s3", kind: storage.CatalogKindPauseSnapshot, wantRetired: 1},
		{name: "pause-marker", backend: "xfs-s3", marker: true, wantRetired: 1},
		{name: "live-pause", backend: "xfs-s3", kind: storage.CatalogKindPauseSnapshot, keep: true, wantRetired: 1},
		{name: "local-gc", backend: "xfs", marker: true, wantRetired: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installCleanupTemplateTestHooks(t)
			origMarker, origRetire := isXFSS3Package, retireXFSS3Checkpoint
			t.Cleanup(func() { isXFSS3Package, retireXFSS3Checkpoint = origMarker, origRetire })
			const snap = "snap-cleanup-xfss3"
			getLocalSnapshotForFn = func(context.Context, string, string) (*storage.SnapshotCatalogEntry, error) {
				if tc.kind == "" {
					return nil, storage.ErrSnapshotCatalogNotFound
				}
				return &storage.SnapshotCatalogEntry{SnapshotID: snap, Kind: tc.kind}, nil
			}
			isXFSS3Package = func(context.Context, string) bool { return tc.marker }
			retired := 0
			retireXFSS3Checkpoint = func(_ context.Context, id string) { require.Equal(t, snap, id); retired++ }
			listCubeboxesForTest = func() []*cubeboxstore.CubeBox { return nil }
			if tc.keep {
				live := newCubeboxWithStatusForTest("sb-cleanup-xfss3", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
				stampPauseSnapshotID(live, snap)
				live.BindRestoreBase(snap)
				listCubeboxesForTest = func() []*cubeboxstore.CubeBox { return []*cubeboxstore.CubeBox{live} }
			}
			deleted := 0
			cleanupTemplateLocalDataFn = func(context.Context, string, string) error { deleted++; return nil }
			rsp, err := (&service{}).cleanupTemplate(context.Background(), &cubebox.CleanupTemplateRequest{
				TemplateID: snap, Backend: tc.backend, Objects: []*cubebox.CowObjectRef{{Name: "n", Kind: "snapshot", Role: "rootfs"}},
			}, true)
			require.NoError(t, err)
			require.Equal(t, errorcode.ErrorCode_Success, rsp.GetRet().GetRetCode(), rsp.GetRet().GetRetMsg())
			require.Equal(t, tc.wantRetired, retired)
			if tc.keep {
				require.Zero(t, deleted)
			} else {
				require.Equal(t, 1, deleted)
			}
		})
	}
}
