// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	errorcode "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type pauseFailureContainer struct {
	fakeDestroyContainer
}

func (*pauseFailureContainer) Spec(context.Context) (*oci.Spec, error) { return nil, nil }

type pauseFailureTask struct {
	containerd.Task
}

func (*pauseFailureTask) Update(context.Context, ...containerd.UpdateTaskOpts) error { return nil }

func TestPauseFailureCleansPackageUnderWriteGuard(t *testing.T) {
	for _, stage := range []string{"memory-prepare", "memory-validation", "finalize"} {
		t.Run(stage, func(t *testing.T) {
			installCleanupTemplateTestHooks(t)
			origSpec, origLayout, origRootfs := pauseSnapshotSpec, pauseSnapshotLayout, pauseSnapshotRootfs
			origMemory, origPrepare, origRelease := pauseSnapshotMemory, pausePrepareMetadata, pauseReleaseMetadata
			origCleanup, origCommit, origDeactivate, origFinalize := pauseCleanupObjects, pauseCommitRootfs, pauseDeactivateObjects, pauseFinalizePackage
			t.Cleanup(func() {
				pauseSnapshotSpec, pauseSnapshotLayout, pauseSnapshotRootfs = origSpec, origLayout, origRootfs
				pauseSnapshotMemory, pausePrepareMetadata, pauseReleaseMetadata = origMemory, origPrepare, origRelease
				pauseCleanupObjects, pauseCommitRootfs, pauseDeactivateObjects, pauseFinalizePackage = origCleanup, origCommit, origDeactivate, origFinalize
			})
			id := "snap-failure-" + stage
			home := filepath.Join(t.TempDir(), id)
			layout := snapshotWorkLayout{
				Backend: cow.BackendS3, Kind: storage.SnapshotKindPause, SnapshotID: id,
				Home: home, TmpHome: home, MetaDir: filepath.Join(home, "metadata"),
				MetaWork: filepath.Join(home, "metadata"),
			}
			t.Cleanup(func() { storage.DeleteSnapshotCatalogFor(cow.BackendS3, id) })
			sb := newCubeboxWithStatusForTest("sb-pause-failure", cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
			sb.FirstContainer().Config = &cubebox.ContainerConfig{
				VolumeMounts: []*cubebox.VolumeMounts{{Name: "root", ContainerPath: "/"}},
			}
			sb.FirstContainer().Container = &pauseFailureContainer{fakeDestroyContainer{task: &pauseFailureTask{}}}
			s := &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: sb}}}
			pauseSnapshotSpec = func(*service, context.Context, string) (*CubeboxSnapshotSpec, error) {
				return &CubeboxSnapshotSpec{Resource: []byte(`{"cpu":1,"memory":128}`)}, nil
			}
			pauseSnapshotLayout = func(string, string, string, string, string) (snapshotWorkLayout, error) { return layout, nil }
			pauseSnapshotRootfs = func(context.Context, string, string, string) (*storage.CowSnapshotObject, error) {
				return &storage.CowSnapshotObject{Name: "live-rootfs", Kind: "volume"}, nil
			}
			pausePrepareMetadata = func(l snapshotWorkLayout, _ context.Context) error { return os.MkdirAll(l.MetaDir, 0o755) }
			pauseReleaseMetadata = func(snapshotWorkLayout, context.Context) {}
			pauseCleanupObjects = func(context.Context, *log.CubeWrapperLogEntry, string, *storage.CowSnapshotObject, *storage.CowSnapshotObject) {
			}
			pauseSnapshotMemory = func(_ context.Context, _ *log.CubeWrapperLogEntry, _ *cubeboxstore.CubeBox, _, _ string, size uint64, _ string) (*storage.CowSnapshotObject, string, error) {
				if stage == "memory-prepare" {
					return nil, "", errors.New("injected ENOSPC")
				}
				object := &storage.CowSnapshotObject{Name: "tpl-" + id + "-memory", Kind: "volume", DevPath: "/dev/fake-memory", SizeBytes: size}
				if stage == "memory-validation" {
					object.DevPath = ""
				}
				return object, "full", nil
			}
			pauseCommitRootfs = func(context.Context, string, *storage.CowSnapshotObject, string) (*storage.CowSnapshotObject, error) {
				return &storage.CowSnapshotObject{Name: "tpl-" + id + "-rootfs", Kind: "snapshot"}, nil
			}
			pauseDeactivateObjects = func(context.Context, *log.CubeWrapperLogEntry, string, *storage.CowSnapshotObject, *storage.CowSnapshotObject) error {
				return nil
			}
			sealed := map[string]bool{}
			finalizeCalled := false
			pauseFinalizePackage = func(ctx context.Context, backend, snapshotID string) error {
				finalizeCalled = true
				entry, err := storage.GetLocalSnapshotFor(ctx, backend, snapshotID)
				require.NoError(t, err)
				entry.MemoryVol += "-snap"
				entry.MemoryKind = "snapshot"
				entry.MetadataVol = storage.S3MetadataSnapshotName(id)
				entry.MetadataKind = "snapshot"
				require.NoError(t, storage.WriteSnapshotCatalogFor(backend, entry))
				sealed[entry.MemoryVol], sealed[entry.MetadataVol] = true, true
				return errors.New("injected finalize failure after sealing")
			}
			getLocalSnapshotForFn = storage.GetLocalSnapshotFor
			listCubeboxesForTest = func() []*cubeboxstore.CubeBox { return nil }
			cleanupIsCowBackend = func() bool { return true }
			metadataCleaned, objectsCleaned, localCleaned := false, false, false
			assertExternalOperationsBlocked := func() {
				t.Helper()
				require.True(t, storage.S3PackageOperationInProgress(id))
				_, release, err := storage.BeginS3PackageWrite(context.Background(), cow.BackendS3, id)
				if release != nil {
					release()
				}
				require.Error(t, err)
				release, err = storage.BeginS3PackageCleanup(context.Background(), cow.BackendS3, id)
				if release != nil {
					release()
				}
				require.Error(t, err)
			}
			cleanupReleaseS3Metadata = func(context.Context, string, string) error {
				assertExternalOperationsBlocked()
				metadataCleaned = true
				return nil
			}
			cleanupObjectsFor = func(_ context.Context, _ string, refs []storage.CowObjectRef) error {
				assertExternalOperationsBlocked()
				objectsCleaned = true
				for _, ref := range refs {
					delete(sealed, ref.Name)
				}
				return nil
			}
			cleanupTemplateLocalDataFn = func(context.Context, string, string) error {
				assertExternalOperationsBlocked()
				localCleaned = true
				return os.RemoveAll(home)
			}
			rsp, err := s.updateWithPauseCow(context.Background(), &cubebox.UpdateCubeSandboxRequest{
				SandboxID: sb.ID, RequestID: "pause-failure",
				Annotations: map[string]string{
					constants.MasterAnnotationPauseSnapshotID: id,
					constants.MasterAnnotationStorageBackend:  cow.BackendS3,
				},
			}, sb)
			require.NoError(t, err)
			require.Equal(t, errorcode.ErrorCode_Unknown, rsp.GetRet().GetRetCode(), rsp.GetRet().GetRetMsg())
			require.Equal(t, stage == "finalize", finalizeCalled)
			require.True(t, metadataCleaned, "pause failure must reach metadata cleanup")
			require.True(t, objectsCleaned, "pause failure must sweep sealed object names")
			require.True(t, localCleaned)
			require.Empty(t, sealed)
			require.NoDirExists(t, home)
			_, err = storage.GetLocalSnapshotFor(context.Background(), cow.BackendS3, id)
			require.ErrorIs(t, err, storage.ErrSnapshotCatalogNotFound)
			require.False(t, storage.S3PackageOperationInProgress(id))
		})
	}
}
