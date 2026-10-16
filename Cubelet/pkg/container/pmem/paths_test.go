// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package pmem

import (
	"bytes"
	"context"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestIndependentArtifactPaths(t *testing.T) {
	root := t.TempDir()
	p, err := ResolvePaths(filepath.Join(root, "tools"), filepath.Join(root, "data"), filepath.Join(root, "kernel", "vmlinux"))
	require.NoError(t, err)
	_, err = os.Stat(p.ImageBasePath)
	require.True(t, os.IsNotExist(err), "resolving paths must not create directories")
	require.Equal(t, filepath.Join(root, "tools", "other_os_image"), p.ImageDir("other"))
	previous := CurrentPaths()
	InitPaths(p)
	t.Cleanup(func() { InitPaths(previous) })
	require.NoError(t, os.MkdirAll(filepath.Dir(p.SharedKernelPath), 0755))
	require.NoError(t, os.WriteFile(p.SharedKernelPath, bytes.Repeat([]byte("k"), 2048), 0644))
	target := GetRawKernelFilePath("cubebox", "rfs-test")
	require.NoError(t, RefreshKernelFile(context.Background(), GetSharedKernelFilePath(), target))
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, bytes.Repeat([]byte("k"), 2048), data)
	require.Equal(t, filepath.Join(root, "data", "rfs-test", "rfs-test.ext4"), GetRawImageFilePath("cubebox", "rfs-test"))
}

func TestArtifactSuffixAndFsType(t *testing.T) {
	cases := []struct {
		media          string
		wantSuffix     string
		wantFsType     string
		wantPathSuffix string
	}{
		{"ext4", ".ext4", "ext4", ".ext4"},
		{"erofs", ".erofs", "erofs", ".erofs"},
		{"", ".ext4", "ext4", ".ext4"},   // empty defaults to ext4
		{"docker", ".ext4", "ext4", ".ext4"}, // docker is not a rootfs format; defaults to ext4 suffix
		{"unknown", ".ext4", "ext4", ".ext4"},
	}
	for _, c := range cases {
		if got := ArtifactSuffix(c.media); got != c.wantSuffix {
			t.Fatalf("ArtifactSuffix(%q) = %q, want %q", c.media, got, c.wantSuffix)
		}
		if got := ArtifactFsType(c.media); got != c.wantFsType {
			t.Fatalf("ArtifactFsType(%q) = %q, want %q", c.media, got, c.wantFsType)
		}
	}
}

func TestImageFileByMedia(t *testing.T) {
	root := t.TempDir()
	p, err := ResolvePaths(filepath.Join(root, "tools"), filepath.Join(root, "data"), filepath.Join(root, "kernel", "vmlinux"))
	require.NoError(t, err)
	previous := CurrentPaths()
	InitPaths(p)
	t.Cleanup(func() { InitPaths(previous) })

	// ext4 → .ext4 (same as the legacy ImageFile), erofs → .erofs.
	require.Equal(t, filepath.Join(root, "data", "rfs-1", "rfs-1.ext4"),
		p.ImageFileByMedia("cubebox", "rfs-1", "ext4"))
	require.Equal(t, filepath.Join(root, "data", "rfs-1", "rfs-1.erofs"),
		p.ImageFileByMedia("cubebox", "rfs-1", "erofs"))
	// GetRawImageFilePathByMedia mirrors ImageFileByMedia.
	require.Equal(t, filepath.Join(root, "data", "rfs-1", "rfs-1.erofs"),
		GetRawImageFilePathByMedia("cubebox", "rfs-1", "erofs"))
	// Unknown/empty media falls back to .ext4.
	require.Equal(t, filepath.Join(root, "data", "rfs-1", "rfs-1.ext4"),
		GetRawImageFilePathByMedia("cubebox", "rfs-1", ""))
}
