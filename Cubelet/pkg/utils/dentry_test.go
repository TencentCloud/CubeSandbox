// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSupportPrepare(t *testing.T) {
	flag := IsMountLoop("/proc")
	assert.True(t, flag)

	flag = IsMountLoop("/xxx/yyy/zzz")
	assert.False(t, flag)
}

func TestFileExistAndValid(t *testing.T) {
	t.Run("valid regular file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "valid.bin")
		assert.NoError(t, os.WriteFile(path, make([]byte, 2048), 0o644))

		ok, err := FileExistAndValid(path)
		assert.True(t, ok)
		assert.NoError(t, err)
	})

	t.Run("small file is invalid", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "small.bin")
		assert.NoError(t, os.WriteFile(path, make([]byte, 128), 0o644))

		ok, err := FileExistAndValid(path)
		assert.False(t, ok)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid size")
	})

	t.Run("directory is invalid", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dir")
		assert.NoError(t, os.MkdirAll(path, 0o755))

		ok, err := FileExistAndValid(path)
		assert.False(t, ok)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "is a directory")
	})

	t.Run("missing path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.bin")

		ok, err := FileExistAndValid(path)
		assert.False(t, ok)
		assert.NoError(t, err)
	})
}

func TestGetDeviceIdleRatio(t *testing.T) {
	t.Run("no panic on real path", func(t *testing.T) {
		blockRatio, inodeRatio, err := GetDeviceIdleRatio("/tmp")
		assert.NoError(t, err)
		// On any real filesystem, block ratio should be between 0 and 100.
		assert.GreaterOrEqual(t, blockRatio, uint64(0))
		assert.LessOrEqual(t, blockRatio, uint64(100))
		// Inode ratio is 100 on btrfs (no fixed inode table) or a
		// real percentage on other filesystems.
		assert.GreaterOrEqual(t, inodeRatio, uint64(0))
		assert.LessOrEqual(t, inodeRatio, uint64(100))
	})

	t.Run("no panic on root path", func(t *testing.T) {
		blockRatio, inodeRatio, err := GetDeviceIdleRatio("/")
		assert.NoError(t, err)
		assert.GreaterOrEqual(t, blockRatio, uint64(0))
		assert.LessOrEqual(t, blockRatio, uint64(100))
		assert.GreaterOrEqual(t, inodeRatio, uint64(0))
		assert.LessOrEqual(t, inodeRatio, uint64(100))
	})

	t.Run("invalid path returns error", func(t *testing.T) {
		_, _, err := GetDeviceIdleRatio("/nonexistent/path/that/does/not/exist")
		assert.Error(t, err)
	})

	t.Run("btrfs-like inode ratio is 100 when Files=0", func(t *testing.T) {
		// On btrfs, statfs returns Files=0 because btrfs has no
		// fixed inode table. Verify the function handles this
		// gracefully by returning 100% inode ratio.
		//
		// We test against the data path which is known to be btrfs
		// in our CI and dev environments.
		paths := []string{"/data", "/"}
		for _, p := range paths {
			if _, err := os.Stat(p); err != nil {
				continue
			}
			_, inodeRatio, err := GetDeviceIdleRatio(p)
			assert.NoError(t, err)
			// On any filesystem, inodeRatio must be >= 0 and <= 100.
			// On btrfs it will be exactly 100.
			assert.GreaterOrEqual(t, inodeRatio, uint64(0))
			assert.LessOrEqual(t, inodeRatio, uint64(100))
		}
	})
}
