// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubecow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

type memSandboxImportKV struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (m *memSandboxImportKV) ReadAll() (map[string][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]byte{}
	for k, v := range m.data {
		out[k] = append([]byte(nil), v...)
	}
	return out, nil
}

func (m *memSandboxImportKV) Set(key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = append([]byte(nil), value...)
	return nil
}

func (m *memSandboxImportKV) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func (m *memSandboxImportKV) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.data {
		out = append(out, k)
	}
	return out
}

func stubSandboxImportKV(t *testing.T) *memSandboxImportKV {
	t.Helper()
	kv := &memSandboxImportKV{data: map[string][]byte{}}
	prev := testSandboxImportKV
	testSandboxImportKV = kv
	t.Cleanup(func() {
		testSandboxImportKV = prev
		sandboxImportMu.Lock()
		sandboxImportsInFlight = map[string]int{}
		sandboxImportMu.Unlock()
	})
	return kv
}

func stubReleaseSandboxImport(t *testing.T, err error) *[]string {
	t.Helper()
	var released []string
	prev := releaseSandboxImportFn
	releaseSandboxImportFn = func(_ context.Context, backend, id string) error {
		released = append(released, backend+":"+id)
		return err
	}
	t.Cleanup(func() { releaseSandboxImportFn = prev })
	return &released
}

func notOwned(string) (bool, error) { return false, nil }

// A Create rejected after the import (for example a package that lost its
// sandbox_spec.json) leaves no sandbox record, so the claim itself has to
// release what was imported.
func TestSandboxImportClaimReleasesWhenNoSandboxTookOver(t *testing.T) {
	kv := stubSandboxImportKV(t)
	released := stubReleaseSandboxImport(t, nil)

	claim, err := ClaimSandboxImport("sb-1", "s3")
	require.NoError(t, err)
	require.Equal(t, []string{"sb-1"}, kv.keys(), "claim must be durable before importing")

	require.NoError(t, claim.Finish(context.Background(), false))
	require.Equal(t, []string{"s3:sb-1"}, *released)
	require.Empty(t, kv.keys())
	// Finish is once-only.
	require.NoError(t, claim.Finish(context.Background(), false))
	require.Len(t, *released, 1)
}

func TestSandboxImportClaimLeavesOwnedDisksToDestroy(t *testing.T) {
	kv := stubSandboxImportKV(t)
	released := stubReleaseSandboxImport(t, nil)

	claim, err := ClaimSandboxImport("sb-1", "s3")
	require.NoError(t, err)
	require.NoError(t, claim.Finish(context.Background(), true))
	require.Empty(t, *released)
	require.Empty(t, kv.keys())
}

// A release that fails keeps the durable claim, so the sweep retries it.
func TestSandboxImportClaimKeptWhenReleaseFails(t *testing.T) {
	kv := stubSandboxImportKV(t)
	stubReleaseSandboxImport(t, errors.New("umount busy"))

	claim, err := ClaimSandboxImport("sb-1", "s3")
	require.NoError(t, err)
	require.Error(t, claim.Finish(context.Background(), false))
	require.Equal(t, []string{"sb-1"}, kv.keys())
}

// Two Creates for the same sandbox: the one that loses must not release disks
// the other may be about to own.
func TestSandboxImportClaimDefersToConcurrentCreate(t *testing.T) {
	kv := stubSandboxImportKV(t)
	released := stubReleaseSandboxImport(t, nil)

	first, err := ClaimSandboxImport("sb-1", "s3")
	require.NoError(t, err)
	second, err := ClaimSandboxImport("sb-1", "s3")
	require.NoError(t, err)

	require.NoError(t, first.Finish(context.Background(), false))
	require.Empty(t, *released)
	require.Equal(t, []string{"sb-1"}, kv.keys())

	require.NoError(t, second.Finish(context.Background(), false))
	require.Equal(t, []string{"s3:sb-1"}, *released)
}

func TestClaimSandboxImportRejectsUnsafeInput(t *testing.T) {
	stubSandboxImportKV(t)
	for _, tc := range [][2]string{{"../x", "s3"}, {"", "s3"}, {"sb-1", "xfs"}} {
		_, err := ClaimSandboxImport(tc[0], tc[1])
		require.Error(t, err, tc)
	}
}

func TestReclaimReleasesClaimsLeftByEarlierProcess(t *testing.T) {
	kv := stubSandboxImportKV(t)
	released := stubReleaseSandboxImport(t, nil)

	// A claim from a process that died: persisted, not in flight here.
	_, err := ClaimSandboxImport("dead", "s3")
	require.NoError(t, err)
	_, err = ClaimSandboxImport("owned", "s3")
	require.NoError(t, err)
	live, err := ClaimSandboxImport("live", "s3")
	require.NoError(t, err)
	sandboxImportMu.Lock()
	delete(sandboxImportsInFlight, "dead")
	delete(sandboxImportsInFlight, "owned")
	sandboxImportMu.Unlock()

	reclaimed, err := ReclaimAbandonedSandboxImports(context.Background(), func(id string) (bool, error) {
		return id == "owned", nil
	}, time.Hour)
	require.NoError(t, err)
	require.Equal(t, []string{"dead"}, reclaimed)
	require.Equal(t, []string{"s3:dead"}, *released)
	// The owned claim is dropped without touching its disks; the live
	// Create keeps its claim.
	require.ElementsMatch(t, []string{"live"}, kv.keys())
	require.NoError(t, live.Finish(context.Background(), true))
}

func TestReclaimStopsOnUnknownOwnership(t *testing.T) {
	kv := stubSandboxImportKV(t)
	released := stubReleaseSandboxImport(t, nil)
	_, err := ClaimSandboxImport("sb-1", "s3")
	require.NoError(t, err)
	sandboxImportMu.Lock()
	sandboxImportsInFlight = map[string]int{}
	sandboxImportMu.Unlock()

	_, err = ReclaimAbandonedSandboxImports(context.Background(), func(string) (bool, error) {
		return false, errors.New("storage db unreadable")
	}, time.Hour)
	require.Error(t, err)
	require.Empty(t, *released)
	require.Equal(t, []string{"sb-1"}, kv.keys())
}

// Imports made before claims existed: the metadata disk the Create entry
// imported for a sandbox that never got a record. Only that exact shape, old
// enough, unowned and without a sealed package snapshot is released.
func TestReclaimReleasesUnclaimedLegacyS3Import(t *testing.T) {
	stubS3MetadataMounts(t)
	stubSandboxImportKV(t)
	engine := &fakeCowEngine{}
	useTestCowStorageAt(t, engine)
	ctx := context.Background()

	imp := CrossNodeSandboxImport(crossNodeAnnotations())
	dir, err := imp.EnsureMetadata(ctx)
	require.NoError(t, err)
	home := SnapshotHome(cow.BackendS3, SnapshotKindNormal, "sb-1")

	// Too young: a Create may still be filling it.
	reclaimed, err := ReclaimAbandonedSandboxImports(ctx, notOwned, time.Hour)
	require.NoError(t, err)
	require.Empty(t, reclaimed)

	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(home, old, old))

	// Owned by a sandbox on this node: destroy's business.
	reclaimed, err = ReclaimAbandonedSandboxImports(ctx, func(string) (bool, error) { return true, nil }, time.Hour)
	require.NoError(t, err)
	require.Empty(t, reclaimed)
	require.True(t, s3MetadataIsMounted(dir))

	// A sealed package snapshot means this is a package, not an import.
	engine.volumeInfos[S3MetadataSnapshotName("sb-1")] = &cubecow.Volume{}
	reclaimed, err = ReclaimAbandonedSandboxImports(ctx, notOwned, time.Hour)
	require.NoError(t, err)
	require.Empty(t, reclaimed)
	delete(engine.volumeInfos, S3MetadataSnapshotName("sb-1"))

	reclaimed, err = ReclaimAbandonedSandboxImports(ctx, notOwned, time.Hour)
	require.NoError(t, err)
	require.Equal(t, []string{"sb-1"}, reclaimed)
	require.Contains(t, engine.deletedVolumes, S3MetadataVolumeName("sb-1"))
	require.False(t, s3MetadataIsMounted(dir))
	_, statErr := os.Stat(home)
	require.True(t, os.IsNotExist(statErr), "import home left behind")

	// Released state is gone, so nothing is found again.
	reclaimed, err = ReclaimAbandonedSandboxImports(ctx, notOwned, time.Hour)
	require.NoError(t, err)
	require.Empty(t, reclaimed)
}

// Template packages share the layout but are never imports.
func TestReclaimIgnoresTemplateHomes(t *testing.T) {
	stubS3MetadataMounts(t)
	stubSandboxImportKV(t)
	engine := &fakeCowEngine{}
	useTestCowStorageAt(t, engine)
	ctx := context.Background()

	imp := &SandboxImport{SandboxID: "tpl-1", UUIDs: CrossNodeSandboxImport(crossNodeAnnotations()).UUIDs}
	_, err := imp.EnsureMetadata(ctx)
	require.NoError(t, err)
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(SnapshotHome(cow.BackendS3, SnapshotKindNormal, "tpl-1"), old, old))

	reclaimed, err := ReclaimAbandonedSandboxImports(ctx, notOwned, time.Hour)
	require.NoError(t, err)
	require.Empty(t, reclaimed)
	require.Empty(t, engine.deletedVolumes)
}

// The real release keeps files under a disk it could not unmount.
func TestReleaseSandboxImportKeepsHomeWhenUnmountFails(t *testing.T) {
	stubS3MetadataMounts(t)
	engine := &fakeCowEngine{}
	useTestCowStorageAt(t, engine)
	ctx := context.Background()

	imp := CrossNodeSandboxImport(crossNodeAnnotations())
	_, err := imp.EnsureMetadata(ctx)
	require.NoError(t, err)
	unmountS3MetadataPath = func(string) error { return errors.New("busy") }

	require.Error(t, releaseSandboxImportDisks(ctx, cow.BackendS3, "sb-1"))
	_, statErr := os.Stat(SnapshotHome(cow.BackendS3, SnapshotKindNormal, "sb-1"))
	require.NoError(t, statErr)
}
