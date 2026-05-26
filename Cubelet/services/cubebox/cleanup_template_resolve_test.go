// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

// seedCatalogForTest writes a catalog entry on disk under t.TempDir() and
// removes it from the in-memory cache via t.Cleanup so concurrent tests do
// not interfere.
func seedCatalogForTest(t *testing.T, entry *storage.SnapshotCatalogEntry) {
	t.Helper()
	if entry.SnapshotPath == "" {
		entry.SnapshotPath = filepath.Join(t.TempDir(), "snap-"+entry.SnapshotID)
	}
	require.NoError(t, storage.WriteSnapshotCatalog(entry))
	t.Cleanup(func() { storage.DeleteSnapshotCatalog(entry.SnapshotID) })
}

func TestResolveCleanupRefsPrefersCallerObjects(t *testing.T) {
	templateID := "tpl-resolve-caller"
	objs := []*cubebox.CowObjectRef{
		{Name: "explicit-rootfs", Kind: "snapshot", Role: "rootfs"},
		{Name: "explicit-mem", Kind: "volume", Role: "memory"},
	}
	refs, snapPath, err := resolveCleanupRefs(context.Background(), templateID, objs, "/legacy/path")
	require.NoError(t, err)
	require.Len(t, refs, 2)
	assert.Equal(t, "explicit-rootfs", refs[0].Name)
	assert.Equal(t, "explicit-mem", refs[1].Name)
	// Caller-supplied path is retained when caller also supplied objects
	// (full legacy path - we trust everything they sent).
	assert.Equal(t, "/legacy/path", snapPath)
}

func TestResolveCleanupRefsUsesCatalogWhenObjectsEmpty(t *testing.T) {
	templateID := "tpl-resolve-catalog"
	snapDir := filepath.Join(t.TempDir(), "snap-"+templateID)
	seedCatalogForTest(t, &storage.SnapshotCatalogEntry{
		SnapshotID:      templateID,
		SnapshotPath:    snapDir,
		RootfsVol:       "cat-rootfs",
		RootfsKind:      storage.CowKindSnapshot,
		MemoryVol:       "cat-mem",
		MemoryKind:      storage.CowKindVolume,
		BuildRootfsVol:  "cat-build",
		BuildRootfsKind: storage.CowKindVolume,
		Kind:            storage.CatalogKindTemplate,
	})

	refs, snapPath, err := resolveCleanupRefs(context.Background(), templateID, nil, "")
	require.NoError(t, err)
	require.Len(t, refs, 3)
	assert.Equal(t, "cat-rootfs", refs[0].Name)
	assert.Equal(t, "rootfs", refs[0].Role)
	assert.Equal(t, "cat-mem", refs[1].Name)
	assert.Equal(t, "memory", refs[1].Role)
	assert.Equal(t, "cat-build", refs[2].Name)
	assert.Equal(t, "build_rootfs", refs[2].Role)
	// catalog entry's SnapshotPath wins over (empty) caller-supplied path.
	assert.Equal(t, snapDir, snapPath)
}

func TestResolveCleanupRefsCatalogPathOverridesCallerWhenBothPresent(t *testing.T) {
	templateID := "tpl-resolve-prefer-catalog-path"
	snapDir := filepath.Join(t.TempDir(), "snap-"+templateID)
	seedCatalogForTest(t, &storage.SnapshotCatalogEntry{
		SnapshotID:   templateID,
		SnapshotPath: snapDir,
		RootfsVol:    "r",
		MemoryVol:    "m",
	})

	_, snapPath, err := resolveCleanupRefs(context.Background(), templateID, nil, "/legacy/should/lose")
	require.NoError(t, err)
	assert.Equal(t, snapDir, snapPath)
}

func TestResolveCleanupRefsFallsBackToDeterministicOnCatalogMiss(t *testing.T) {
	templateID := "tpl-resolve-miss"
	refs, snapPath, err := resolveCleanupRefs(context.Background(), templateID, nil, "/legacy/fallback")
	require.NoError(t, err)
	require.Len(t, refs, 3)
	assert.Equal(t, "tpl-"+templateID+"-rootfs", refs[0].Name)
	assert.Equal(t, "tpl-"+templateID+"-memory", refs[1].Name)
	assert.Equal(t, "tpl-"+templateID+"-build-rootfs", refs[2].Name)
	// catalog miss: snapshot path comes from caller (best-effort) since we
	// can't derive it locally.
	assert.Equal(t, "/legacy/fallback", snapPath)
}

func TestResolveCleanupRefsCatalogWithoutBuildRootfsStillCleansFromCatalog(t *testing.T) {
	templateID := "tpl-resolve-nobuild"
	snapDir := filepath.Join(t.TempDir(), "snap-"+templateID)
	// Runtime snapshot entry: build_rootfs is intentionally empty.
	seedCatalogForTest(t, &storage.SnapshotCatalogEntry{
		SnapshotID:   templateID,
		SnapshotPath: snapDir,
		RootfsVol:    "rt-rootfs",
		RootfsKind:   storage.CowKindSnapshot,
		MemoryVol:    "rt-mem",
		MemoryKind:   storage.CowKindVolume,
		Kind:         storage.CatalogKindRuntimeSnapshot,
	})

	refs, _, err := resolveCleanupRefs(context.Background(), templateID, nil, "")
	require.NoError(t, err)
	require.Len(t, refs, 2)
	assert.Equal(t, "rt-rootfs", refs[0].Name)
	assert.Equal(t, "rt-mem", refs[1].Name)
}
