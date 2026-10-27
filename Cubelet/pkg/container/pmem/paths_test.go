// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package pmem

import (
	"bytes"
	"context"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cubeimages "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
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
		{"", ".ext4", "ext4", ".ext4"},       // empty defaults to ext4
		{"docker", ".ext4", "ext4", ".ext4"}, // docker is not a rootfs format; defaults to ext4 suffix
		{"unknown", ".ext4", "ext4", ".ext4"},
	}
	// A fixed instance/image so wantPathSuffix can assert the full path the
	// media maps to via GetRawImageFilePathByMedia, not just the bare suffix —
	// this is the path the pmem device/shim actually consumes. Only the suffix
	// is asserted (HasSuffix) so the test does not depend on the configured
	// ImageBasePath, which is irrelevant to the media→suffix mapping.
	const instanceType, imageID = "cubebox", "rfs-suffix"
	for _, c := range cases {
		if got := ArtifactSuffix(c.media); got != c.wantSuffix {
			t.Fatalf("ArtifactSuffix(%q) = %q, want %q", c.media, got, c.wantSuffix)
		}
		if got := ArtifactFsType(c.media); got != c.wantFsType {
			t.Fatalf("ArtifactFsType(%q) = %q, want %q", c.media, got, c.wantFsType)
		}
		// The path-level suffix must agree with wantPathSuffix, proving the
		// suffix helper and the path builder route the same media the same way.
		wantTail := imageID + c.wantPathSuffix
		if got := GetRawImageFilePathByMedia(instanceType, imageID, c.media); !strings.HasSuffix(got, wantTail) {
			t.Fatalf("GetRawImageFilePathByMedia(%q, %q, %q) = %q, want suffix %q",
				instanceType, imageID, c.media, got, wantTail)
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

// TestArtifactSuffixMatchesProtoEnum asserts the intended 1:1 mapping between
// pmem's local storageMediaErofs constant and the proto ImageStorageMediaType
// enum. pmem production code stays decoupled from the proto package, so this
// cross-check lives in the test (which may import proto): if a proto rename
// changes erofs's string form, this fails loudly here instead of silently
// breaking the media->suffix/fstype dispatch. Together with proto's own
// TestImageStorageMediaTypeStrings, this makes the mapping explicit and guarded
// on both sides.
func TestArtifactSuffixMatchesProtoEnum(t *testing.T) {
	require.Equal(t, storageMediaErofs, cubeimages.ImageStorageMediaType_erofs.String(),
		"pmem storageMediaErofs must equal proto ImageStorageMediaType_erofs.String()")
	// The dispatch must route the proto enum's string form to the erofs suffix
	// and fstype, not fall through to the ext4 default.
	if got := ArtifactSuffix(cubeimages.ImageStorageMediaType_erofs.String()); got != ".erofs" {
		t.Fatalf("ArtifactSuffix(proto erofs) = %q, want .erofs", got)
	}
	if got := ArtifactFsType(cubeimages.ImageStorageMediaType_erofs.String()); got != "erofs" {
		t.Fatalf("ArtifactFsType(proto erofs) = %q, want erofs", got)
	}
	if got := ArtifactSuffix(cubeimages.ImageStorageMediaType_ext4.String()); got != ".ext4" {
		t.Fatalf("ArtifactSuffix(proto ext4) = %q, want .ext4", got)
	}
}
