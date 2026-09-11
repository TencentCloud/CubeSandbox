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
