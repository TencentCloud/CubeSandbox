// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/containerd/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// useTestMintStorage gives the mint tests a local store with a real record
// DB and the fake engine behind the XFS manager.
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

func seedMintRecord(t *testing.T, sid, own, source, origin string) {
	t.Helper()
	info := &StorageInfo{
		Namespace:          "default",
		SandboxID:          sid,
		ImportedMemoryVol:  own,
		MintedMemorySource: source,
		MintedMemoryOrigin: origin,
	}
	require.NoError(t, localStorage.writeBackendFileInfo(context.Background(), sid, info))
}

func readMintRecord(t *testing.T, sid string) *StorageInfo {
	t.Helper()
	info, err := localStorage.readBackendFileInfo(context.Background(), sid)
	require.NoError(t, err)
	require.NotNil(t, info)
	return info
}

func TestPromoteSandboxMemoryOwnForPauseRenamesOwn(t *testing.T) {
	sid := "sb-mint-1"
	own := SandboxMemoryName(sid)
	engine := &fakeCowEngine{volumeInfos: map[string]*cubecow.Volume{
		own: {DevicePath: "/x/volumes/" + own + "/" + own, SizeBytes: 1 << 20},
	}}
	useTestMintStorage(t, engine)
	seedMintRecord(t, sid, own, "snap-8", "tpl-tpl-a-memory")

	obj, detail, ok := PromoteSandboxMemoryOwnForPause(context.Background(), cow.BackendXFS, sid, "snap-9", "snap-8")
	require.True(t, ok, detail)
	require.NotNil(t, obj)
	assert.Equal(t, "tpl-snap-9-memory", obj.Name)
	assert.Equal(t, cowKindSnapshot, obj.Kind)
	assert.Equal(t, [][3]string{{own, "tpl-snap-9-memory", "tpl-tpl-a-memory"}}, engine.promotedVolumes)

	// The consumed mint leaves nothing behind on the record.
	info := readMintRecord(t, sid)
	assert.Empty(t, info.ImportedMemoryVol)
	assert.Empty(t, info.MintedMemorySource)
	assert.Empty(t, info.MintedMemoryOrigin)
}

func TestPromoteSandboxMemoryOwnForPauseRefusesStaleSource(t *testing.T) {
	sid := "sb-mint-2"
	own := SandboxMemoryName(sid)
	engine := &fakeCowEngine{volumeInfos: map[string]*cubecow.Volume{
		own: {DevicePath: "/x/volumes/" + own + "/" + own, SizeBytes: 1 << 20},
	}}
	useTestMintStorage(t, engine)
	// The record still names the previous restore; this pause is relative
	// to a newer one.
	seedMintRecord(t, sid, own, "snap-8", "tpl-tpl-a-memory")

	obj, detail, ok := PromoteSandboxMemoryOwnForPause(context.Background(), cow.BackendXFS, sid, "snap-11", "snap-10")
	require.False(t, ok)
	assert.Nil(t, obj)
	assert.Empty(t, detail)
	assert.Empty(t, engine.promotedVolumes)

	// The stale mint is dropped so the clone ladder cannot clone the old
	// base either.
	info := readMintRecord(t, sid)
	assert.Empty(t, info.ImportedMemoryVol)
	assert.Empty(t, info.MintedMemorySource)
	assert.Empty(t, info.MintedMemoryOrigin)
}

func TestRunSandboxMemoryMintRecordsOwnVolume(t *testing.T) {
	sid := "sb-mint-3"
	own := SandboxMemoryName(sid)
	engine := &fakeCowEngine{}
	useTestMintStorage(t, engine)
	seedMintRecord(t, sid, "", "", "")

	runSandboxMemoryMint(context.Background(), cow.BackendXFS, sid, "tpl-snap-8-memory", "snap-8", "tpl-tpl-a-memory")

	assert.Equal(t, [][2]string{{"tpl-snap-8-memory", own}}, engine.createVolumeFromSnapshots)
	info := readMintRecord(t, sid)
	assert.Equal(t, own, info.ImportedMemoryVol)
	assert.Equal(t, "snap-8", info.MintedMemorySource)
	assert.Equal(t, "tpl-tpl-a-memory", info.MintedMemoryOrigin)
}

func TestRunSandboxMemoryMintCleansUpOnCloneFailure(t *testing.T) {
	sid := "sb-mint-4"
	own := SandboxMemoryName(sid)
	engine := &fakeCowEngine{createVolumeFromSnapshotErr: errors.New("ficlone failed")}
	useTestMintStorage(t, engine)
	seedMintRecord(t, sid, "", "", "")

	runSandboxMemoryMint(context.Background(), cow.BackendXFS, sid, "tpl-snap-8-memory", "snap-8", "tpl-tpl-a-memory")

	// The stale-drop before the clone and the failure cleanup after it
	// each delete the own volume once.
	assert.Equal(t, []string{own, own}, engine.deletedVolumes)
	info := readMintRecord(t, sid)
	assert.Empty(t, info.ImportedMemoryVol)
	assert.Empty(t, info.MintedMemorySource)
	assert.Empty(t, info.MintedMemoryOrigin)
}
