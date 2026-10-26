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
