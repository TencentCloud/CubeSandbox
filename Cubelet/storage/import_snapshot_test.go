// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

func useTestImportStaging(t *testing.T) string {
	t.Helper()
	dataPath := t.TempDir()
	staging := defaultImportStagingDir(dataPath)
	require.NoError(t, os.MkdirAll(staging, 0o755))
	previousLocalStorage := localStorage
	localStorage = &local{config: &Config{DataPath: dataPath}}
	t.Cleanup(func() {
		localStorage = previousLocalStorage
	})
	return staging
}

func TestOpenImportArtifactAcceptsStagedFile(t *testing.T) {
	staging := useTestImportStaging(t)
	src := filepath.Join(staging, "rootfs.vol")
	require.NoError(t, os.WriteFile(src, []byte("artifact-body"), 0o644))

	f, size, err := openImportArtifact("  " + src + "  ")
	require.NoError(t, err)
	defer f.Close()
	require.Equal(t, uint64(len("artifact-body")), size)
	require.Equal(t, src, f.Name())
}

func TestOpenImportArtifactRejectsInvalidSources(t *testing.T) {
	staging := useTestImportStaging(t)

	outside := filepath.Join(t.TempDir(), "outside.vol")
	require.NoError(t, os.WriteFile(outside, []byte("x"), 0o644))

	symlink := filepath.Join(staging, "escape.vol")
	require.NoError(t, os.Symlink(outside, symlink))

	dirlink := filepath.Join(staging, "dirlink")
	require.NoError(t, os.Symlink(filepath.Dir(outside), dirlink))

	subdir := filepath.Join(staging, "dir")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	cases := []struct {
		name    string
		src     string
		wantErr string
	}{
		{"empty", "  ", "empty source path"},
		{"outside staging", outside, "outside staging dir"},
		{"traversal escapes staging", filepath.Join(staging, "..", "escape.vol"), "outside staging dir"},
		{"staging dir itself", staging, "outside staging dir"},
		{"symlink", symlink, "open source"},
		{"dir symlink escapes staging", filepath.Join(dirlink, "outside.vol"), "outside staging dir"},
		{"directory", subdir, "not a regular file"},
		{"missing", filepath.Join(staging, "absent.vol"), "open source"},
	}
	for _, tc := range cases {
		f, _, err := openImportArtifact(tc.src)
		if f != nil {
			f.Close()
		}
		require.Errorf(t, err, "%s: expected error", tc.name)
		require.Containsf(t, err.Error(), tc.wantErr, "%s: error %v", tc.name, err)
	}
}

func TestFindWlayerSubdir(t *testing.T) {
	t.Parallel()

	mkWlayer := func(t *testing.T, root, rel string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Join(root, rel, "upper"), 0o755))
		require.NoError(t, os.MkdirAll(filepath.Join(root, rel, "work"), 0o755))
	}

	t.Run("depth-two layout", func(t *testing.T) {
		root := t.TempDir()
		mkWlayer(t, root, "disk/ctr-1")
		require.NoError(t, os.MkdirAll(filepath.Join(root, "lost+found"), 0o755))
		got, err := findWlayerSubdir(root)
		require.NoError(t, err)
		require.Equal(t, filepath.Join("disk", "ctr-1"), got)
	})

	t.Run("depth-one layout", func(t *testing.T) {
		root := t.TempDir()
		mkWlayer(t, root, "wlayer")
		got, err := findWlayerSubdir(root)
		require.NoError(t, err)
		require.Equal(t, "wlayer", got)
	})

	t.Run("upper without work is no candidate", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, "disk/ctr-1/upper"), 0o755))
		_, err := findWlayerSubdir(root)
		require.ErrorContains(t, err, "expected exactly one")
	})

	t.Run("multiple candidates rejected", func(t *testing.T) {
		root := t.TempDir()
		mkWlayer(t, root, "disk/ctr-1")
		mkWlayer(t, root, "disk/ctr-2")
		_, err := findWlayerSubdir(root)
		require.ErrorContains(t, err, "expected exactly one")
	})

	t.Run("empty root rejected", func(t *testing.T) {
		_, err := findWlayerSubdir(t.TempDir())
		require.ErrorContains(t, err, "expected exactly one")
	})
}

func TestResolveSnapshotMemoryVolDiskOnlyNeedsCatalogMarker(t *testing.T) {
	l := &local{}
	annotations := map[string]string{constants.MasterAnnotationAppSnapshotTemplateID: "snap-marker"}
	seed := func(t *testing.T, diskOnly bool) {
		t.Helper()
		dir := t.TempDir()
		require.NoError(t, WriteSnapshotCatalog(&SnapshotCatalogEntry{
			SnapshotID:   "snap-marker",
			InstanceType: "cubebox",
			SpecDir:      "1C1M",
			SnapshotPath: dir,
			MetaDir:      dir,
			RootfsVol:    "tpl-snap-marker-rootfs",
			RootfsKind:   CowKindSnapshot,
			DiskOnly:     diskOnly,
			Kind:         CatalogKindTemplate,
		}))
		t.Cleanup(func() { DeleteSnapshotCatalog("snap-marker") })
	}

	seed(t, false)
	if _, _, diskOnly, err := l.resolveSnapshotMemoryVolFromCatalog(context.Background(), annotations); err != nil || diskOnly {
		t.Fatalf("unmarked entry with blank memory_vol must not read as disk-only: diskOnly=%v err=%v", diskOnly, err)
	}
	DeleteSnapshotCatalog("snap-marker")

	seed(t, true)
	if _, _, diskOnly, err := l.resolveSnapshotMemoryVolFromCatalog(context.Background(), annotations); err != nil || !diskOnly {
		t.Fatalf("marked entry must read as disk-only: diskOnly=%v err=%v", diskOnly, err)
	}
}

func TestDiskOnlyRestore(t *testing.T) {
	t.Parallel()

	if diskOnly, url := DiskOnlyRestore(&StorageInfo{RestoreMemoryVolURL: "file:///dev/mapper/tpl-memory"}); diskOnly || url != "file:///dev/mapper/tpl-memory" {
		t.Fatalf("memory restore: diskOnly=%v url=%q", diskOnly, url)
	}
	if diskOnly, url := DiskOnlyRestore(&StorageInfo{RestoreDiskOnly: true}); !diskOnly || url != "" {
		t.Fatalf("disk-only restore: diskOnly=%v url=%q", diskOnly, url)
	}
	// An empty URL alone is "not resolved", not an intentional disk-only restore.
	if diskOnly, _ := DiskOnlyRestore(&StorageInfo{}); diskOnly {
		t.Fatal("unmarked storage info must not be disk-only")
	}
	// Unresolved storage info must not read as disk-only.
	if diskOnly, _ := DiskOnlyRestore(nil); diskOnly {
		t.Fatal("nil storage info must not be disk-only")
	}
	if diskOnly, _ := DiskOnlyRestore((*StorageInfo)(nil)); diskOnly {
		t.Fatal("typed-nil storage info must not be disk-only")
	}
}
