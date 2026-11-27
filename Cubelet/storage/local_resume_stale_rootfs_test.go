// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build cgo

package storage

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// These tests run against the real cubecow reflink engine (copy_file_range
// clones, so any local filesystem works) rather than a fake listing: the bug
// in #1847 lives in how that backend answers for an existing snapshot.

const (
	staleResumeSandboxID  = "sb1847"
	staleResumeTemplateID = "snap-1847" // the pause snapshot's rootfs is tpl-snap-1847-rootfs
)

var (
	pauseRootfsData = []byte("rootfs saved by pause")
	staleRootfsData = []byte("half-written by a failed resume")
)

func newReflinkTestEngine(t *testing.T) *cubecow.Engine {
	t.Helper()
	root := t.TempDir()
	method, level := cowCloneCFR, "warn"
	cfg := &Config{}
	cfg.Cow.Log.Level = &level
	payload, err := cfg.buildCowInitJSON(CowBackendConfig{
		Kind:    cowBackendReflink,
		Reflink: CowReflinkBackendConfig{RootDir: &root, CloneMethod: &method},
	})
	require.NoError(t, err)
	engine, err := cubecow.InitWithoutLoggingFromJSON(string(payload))
	require.NoError(t, err)
	t.Cleanup(engine.Close)
	return engine
}

// seedFailedResume builds the on-disk state #1847 reports: a READY pause
// snapshot rootfs, plus the sb-<id>-rootfs-gen0 a failed resume derived from
// it and wrote into.
func seedFailedResume(t *testing.T, engine *cubecow.Engine) (pausePath, gen0Path string) {
	t.Helper()
	buildRootfs := "tpl-tpl1847-build-rootfs"
	devPath, err := engine.CreateVolume(buildRootfs, 1<<20)
	require.NoError(t, err)
	writeAt0(t, devPath, pauseRootfsData)
	pausePath, err = engine.CreateSnapshotFromVolume(buildRootfs, cowTemplateRootfsName(staleResumeTemplateID), true)
	require.NoError(t, err)

	gen0, err := newCowVolumeManager(engine).CreateSandboxRootfsFromTemplate(
		context.Background(), staleResumeSandboxID, staleResumeTemplateID, 0, 0)
	require.NoError(t, err)
	writeAt0(t, gen0.FilePath, staleRootfsData)
	return pausePath, gen0.FilePath
}

// useOwnProcRoot points procRoot at ownProcRoot for the rest of the test.
func useOwnProcRoot(t *testing.T) {
	t.Helper()
	root := ownProcRoot(t)
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
}

func resumeCreateContext(withPausedRow bool) *workflow.CreateContext {
	opts := pauseResumeCreateContext(staleResumeSandboxID, map[string]string{
		constants.MasterAnnotationPauseSnapshotID:  staleResumeTemplateID,
		constants.MasterAnnotationDesiredSandboxID: staleResumeSandboxID,
	})
	if withPausedRow {
		row := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: staleResumeSandboxID, SandboxID: staleResumeSandboxID}}
		opts.PausedTombstone = row
		opts.PausedTombstoneGuard = &workflow.PausedTombstoneGuard{Epoch: 1, OrigRow: row}
	}
	return opts
}

func writeAt0(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteAt(data, 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func requirePrefix(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(got, want), "%s starts with %q, want %q", path, got[:len(want)], want)
}

func TestResumeRetryReplacesStaleRootfsOnReflink(t *testing.T) {
	useOwnProcRoot(t)
	engine := newReflinkTestEngine(t)
	pausePath, gen0Path := seedFailedResume(t, engine)
	l := &local{cowManager: newCowVolumeManager(engine)}

	volume, err := l.allocateSnapshotRootfs(context.Background(), resumeCreateContext(true),
		staleResumeSandboxID, staleResumeTemplateID, 0, &StorageInfo{Volumes: map[string]*BackendFileInfo{}})
	require.NoError(t, err)
	require.Equal(t, SandboxRootfsName(staleResumeSandboxID, 0), volume.VolumeName)
	require.Equal(t, gen0Path, volume.FilePath)
	// The resumed rootfs is a fresh copy of the pause snapshot, not the
	// failed attempt's writes, and the pause snapshot itself is intact.
	requirePrefix(t, volume.FilePath, pauseRootfsData)
	requirePrefix(t, pausePath, pauseRootfsData)
}

func TestResumeRetryKeepsExistingRootfsWhenNotProvablyStale(t *testing.T) {
	useOwnProcRoot(t)
	gen0 := SandboxRootfsName(staleResumeSandboxID, 0)
	cases := []struct {
		name   string
		opts   *workflow.CreateContext
		result *StorageInfo
		// holdOpen keeps gen0 open for the call, as the VM of a failed
		// resume does when it outlives its rollback.
		holdOpen bool
	}{
		{
			name:     "still open by a process",
			opts:     resumeCreateContext(true),
			holdOpen: true,
		},
		{
			// No local PAUSED row: nothing has proved the old runtime is gone.
			name: "no paused row",
			opts: resumeCreateContext(false),
		},
		{
			// A second Default emptyDir volume of the same create collides with
			// the rootfs this create derived a moment ago.
			name: "derived by this create",
			opts: resumeCreateContext(true),
			result: &StorageInfo{Volumes: map[string]*BackendFileInfo{
				"cube_rootfs_rw": {Name: "cube_rootfs_rw", VolumeName: gen0},
			}},
		},
		{
			name: "not a pause resume",
			opts: func() *workflow.CreateContext {
				opts := resumeCreateContext(true)
				delete(opts.ReqInfo.Annotations, constants.MasterAnnotationPauseSnapshotID)
				return opts
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := newReflinkTestEngine(t)
			pausePath, gen0Path := seedFailedResume(t, engine)
			l := &local{cowManager: newCowVolumeManager(engine)}
			if tc.holdOpen {
				f, err := os.Open(gen0Path)
				require.NoError(t, err)
				defer f.Close()
			}

			_, err := l.allocateSnapshotRootfs(context.Background(), tc.opts,
				staleResumeSandboxID, staleResumeTemplateID, 0, tc.result)
			require.ErrorIs(t, err, ErrCowObjectAlreadyExists)
			requirePrefix(t, gen0Path, staleRootfsData)
			requirePrefix(t, pausePath, pauseRootfsData)
		})
	}
}

// s3NamedCowManager reports the S3 store name so the reclaim guard sees an S3
// backend; the S3 store reuses its writable volume and is left alone.
type s3NamedCowManager struct{ *fakeCowVolumeManager }

func (s3NamedCowManager) Name() string { return cow.NameS3 }

func TestResumeRetryLeavesS3RootfsAlone(t *testing.T) {
	fake := &fakeCowVolumeManager{
		createSnapshotErr: fmt.Errorf("%w: name=%s", ErrCowObjectAlreadyExists, SandboxRootfsName(staleResumeSandboxID, 0)),
	}
	l := &local{s3CowManager: s3NamedCowManager{fake}}
	opts := resumeCreateContext(true)
	opts.ReqInfo.Annotations[constants.MasterAnnotationStorageBackend] = cow.BackendS3

	_, err := l.allocateSnapshotRootfs(context.Background(), opts,
		staleResumeSandboxID, staleResumeTemplateID, 0, &StorageInfo{Volumes: map[string]*BackendFileInfo{}})
	require.ErrorIs(t, err, ErrCowObjectAlreadyExists)
	require.Empty(t, fake.deleteCalls)
	require.Len(t, fake.createSnapshotCalls, 1)
}
