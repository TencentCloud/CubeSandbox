// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// useTestMintStorage gives the mint tests a local store with the fake engine
// behind the XFS manager.
func useTestMintStorage(t *testing.T, engine *fakeCowEngine) {
	t.Helper()
	cfg := makeTestConfig(t)
	cfg.StorageBackend = "cubecow"
	s := &local{config: cfg, cowManager: &XfsCow{engine: engine}, s3CowManager: &S3Cow{engine: engine}}
	require.NoError(t, s.init(&plugin.InitContext{Context: context.Background()}))
	previous := localStorage
	localStorage = s
	t.Cleanup(func() { localStorage = previous })
}

// seedOwnVolume registers a fake own volume under a fresh temp directory so
// the marker helpers run against a real directory.
func seedOwnVolume(t *testing.T, engine *fakeCowEngine, sid string) (ownVol, ownDir string) {
	t.Helper()
	ownVol = SandboxMemoryName(sid)
	ownDir = filepath.Join(t.TempDir(), ownVol)
	require.NoError(t, os.MkdirAll(ownDir, 0o755))
	if engine.volumeInfos == nil {
		engine.volumeInfos = map[string]*cubecow.Volume{}
	}
	engine.volumeInfos[ownVol] = &cubecow.Volume{DevicePath: filepath.Join(ownDir, ownVol), SizeBytes: 1 << 20}
	return ownVol, ownDir
}

func seedSandboxRecord(t *testing.T, sid string) {
	t.Helper()
	require.NoError(t, localStorage.writeBackendFileInfo(context.Background(), sid,
		&StorageInfo{Namespace: "default", SandboxID: sid}))
}

func TestDestroyWaitsForTheMintSerialDespiteCallerCancel(t *testing.T) {
	sid := "sb-mint-6"
	engine := &fakeCowEngine{}
	useTestMintStorage(t, engine)
	seedSandboxRecord(t, sid)
	unlock := mintSerials.Lock(sid)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // caller-side cancellation must not skip the teardown
	go func() {
		time.Sleep(50 * time.Millisecond)
		unlock()
	}()

	require.NoError(t, localStorage.Destroy(ctx, &workflow.DestroyContext{BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: sid}}))
	_, err := localStorage.readBackendFileInfoRaw(context.Background(), sid)
	require.Error(t, err)
}

func TestDeleteBackendFileInfoDropsTheFailoverCopy(t *testing.T) {
	sid := "sb-mint-7"
	engine := &fakeCowEngine{}
	useTestMintStorage(t, engine)
	seedSandboxRecord(t, sid)
	require.NoError(t, os.MkdirAll(localStorage.failoverDir(), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(localStorage.failoverDir(), sid), []byte(`{"sandboxId":"sb-mint-7"}`), 0o644))

	require.NoError(t, localStorage.deleteBackendFileInfo(context.Background(), sid))

	// A retired record must not be readable back from the failover copy: that
	// is what a mint checks before publishing its marker.
	_, err := localStorage.readBackendFileInfoRaw(context.Background(), sid)
	require.Error(t, err)
}

func TestPromoteSandboxMemoryOwnForPauseRenamesOwn(t *testing.T) {
	sid := "sb-mint-1"
	engine := &fakeCowEngine{}
	useTestMintStorage(t, engine)
	own, ownDir := seedOwnVolume(t, engine, sid)
	require.NoError(t, os.WriteFile(filepath.Join(ownDir, mintMarkerName), []byte("snap-8"), 0o644))

	base := &CowSnapshotObject{Name: "tpl-tpl-a-memory", Kind: cowKindSnapshot, DevPath: "/x/volumes/tpl-tpl-a-memory/tpl-tpl-a-memory"}
	obj, detail, ok := PromoteSandboxMemoryOwnForPause(context.Background(), cow.BackendXFS, sid, "snap-9", "snap-8", base)
	require.True(t, ok, detail)
	require.NotNil(t, obj)
	assert.Equal(t, "tpl-snap-9-memory", obj.Name)
	assert.Equal(t, cowKindSnapshot, obj.Kind)
	assert.Equal(t, [][3]string{{own, "tpl-snap-9-memory", "tpl-tpl-a-memory"}}, engine.promotedVolumes)
	_, err := os.Stat(ownDir)
	assert.True(t, os.IsNotExist(err), "the consumed mint must leave no marker or directory")
}

func TestPromoteSandboxMemoryOwnForPauseRefusesStaleSource(t *testing.T) {
	sid := "sb-mint-2"
	engine := &fakeCowEngine{}
	useTestMintStorage(t, engine)
	own, ownDir := seedOwnVolume(t, engine, sid)
	require.NoError(t, os.WriteFile(filepath.Join(ownDir, mintMarkerName), []byte("snap-8"), 0o644))

	base := &CowSnapshotObject{Name: "tpl-tpl-a-memory", Kind: cowKindSnapshot, DevPath: "/x/volumes/tpl-tpl-a-memory/tpl-tpl-a-memory"}
	obj, detail, ok := PromoteSandboxMemoryOwnForPause(context.Background(), cow.BackendXFS, sid, "snap-11", "snap-10", base)
	require.False(t, ok)
	assert.Nil(t, obj)
	assert.Contains(t, detail, "not minted from")
	assert.Empty(t, engine.promotedVolumes)
	assert.Contains(t, engine.deletedVolumes, own)
}

func TestReleaseImportedMemoryVolDropsUnmarkedOwnVolume(t *testing.T) {
	sid := "sb-mint-4"
	engine := &fakeCowEngine{}
	useTestMintStorage(t, engine)

	// A mint that died before writing its marker leaves no record and no
	// marker; Destroy still has to reclaim the conventionally named volume.
	localStorage.releaseImportedMemoryVol(context.Background(), cow.BackendXFS, &StorageInfo{SandboxID: sid})

	assert.Equal(t, []string{SandboxMemoryName(sid)}, engine.deletedVolumes)
}

func TestRunSandboxMemoryMintWritesMarker(t *testing.T) {
	sid := "sb-mint-3"
	own := SandboxMemoryName(sid)
	ownDir := filepath.Join(t.TempDir(), own)
	require.NoError(t, os.MkdirAll(ownDir, 0o755))
	engine := &fakeCowEngine{
		createVolumePath: filepath.Join(ownDir, own),
		volumeInfos: map[string]*cubecow.Volume{
			own: {DevicePath: filepath.Join(ownDir, own), SizeBytes: 1 << 20},
		},
	}
	useTestMintStorage(t, engine)
	seedSandboxRecord(t, sid)
	// A leftover from an earlier mint: its marker must go before the volume,
	// because cubecow's delete rmdirs a directory it expects to be empty.
	require.NoError(t, os.WriteFile(filepath.Join(ownDir, mintMarkerName), []byte("snap-old"), 0o644))
	engine.onDeleteVolume = func(name string) {
		if name != own {
			return
		}
		if _, err := os.Stat(filepath.Join(ownDir, mintMarkerName)); !os.IsNotExist(err) {
			t.Errorf("mint must remove the marker before deleting the own volume")
		}
	}

	runSandboxMemoryMint(context.Background(), cow.BackendXFS, sid, "tpl-snap-8-memory", "snap-8")

	assert.Equal(t, [][2]string{{"tpl-snap-8-memory", own}}, engine.createVolumeFromSnapshots)
	assert.Equal(t, []string{own}, engine.deletedVolumes)
	raw, err := os.ReadFile(filepath.Join(ownDir, mintMarkerName))
	require.NoError(t, err)
	assert.Equal(t, "snap-8", string(raw))
}
