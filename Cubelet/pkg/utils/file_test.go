// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyFileFast(t *testing.T) {

	tempDir := t.TempDir()

	srcFile := filepath.Join(tempDir, "source.txt")
	testContent := []byte("test content for copy")
	err := os.WriteFile(srcFile, testContent, 0644)
	require.NoError(t, err)

	t.Run("successful copy", func(t *testing.T) {
		dstFile := filepath.Join(tempDir, "dest.txt")
		err := copyFileFast(srcFile, dstFile)
		require.NoError(t, err)

		content, err := os.ReadFile(dstFile)
		require.NoError(t, err)
		assert.Equal(t, testContent, content)

		srcInfo, err := os.Stat(srcFile)
		require.NoError(t, err)
		dstInfo, err := os.Stat(dstFile)
		require.NoError(t, err)
		assert.Equal(t, srcInfo.Mode(), dstInfo.Mode())
	})

	t.Run("preserve file permissions", func(t *testing.T) {
		execFile := filepath.Join(tempDir, "exec.sh")
		err := os.WriteFile(execFile, []byte("#!/bin/bash\necho test"), 0755)
		require.NoError(t, err)

		dstFile := filepath.Join(tempDir, "exec_copy.sh")
		err = copyFileFast(execFile, dstFile)
		require.NoError(t, err)

		srcInfo, err := os.Stat(execFile)
		require.NoError(t, err)
		dstInfo, err := os.Stat(dstFile)
		require.NoError(t, err)
		assert.Equal(t, srcInfo.Mode(), dstInfo.Mode())
	})

	t.Run("source file not exist", func(t *testing.T) {
		err := copyFileFast("/nonexistent/file", filepath.Join(tempDir, "dest.txt"))
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to open source file")
	})

	t.Run("invalid destination path", func(t *testing.T) {
		err := copyFileFast(srcFile, "/invalid/path/dest.txt")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create dest file")
	})
}

func TestCopyfile(t *testing.T) {
	tempDir := t.TempDir()
	ctx := namespaces.WithNamespace(context.Background(), "test-ns")

	t.Run("copy single file", func(t *testing.T) {
		srcFile := filepath.Join(tempDir, "single_source.txt")
		testContent := []byte("single file content")
		err := os.WriteFile(srcFile, testContent, 0644)
		require.NoError(t, err)

		dstFile := filepath.Join(tempDir, "subdir", "single_dest.txt")
		err = CopyFile(ctx, srcFile, dstFile)
		require.NoError(t, err)

		content, err := os.ReadFile(dstFile)
		require.NoError(t, err)
		assert.Equal(t, testContent, content)

		_, err = os.Stat(filepath.Dir(dstFile))
		assert.NoError(t, err)
	})

	t.Run("copy directory", func(t *testing.T) {

		srcDir := filepath.Join(tempDir, "src_dir")
		err := os.MkdirAll(filepath.Join(srcDir, "subdir"), 0755)
		require.NoError(t, err)

		err = os.WriteFile(filepath.Join(srcDir, "file1.txt"), []byte("content1"), 0644)
		require.NoError(t, err)
		err = os.WriteFile(filepath.Join(srcDir, "subdir", "file2.txt"), []byte("content2"), 0644)
		require.NoError(t, err)

		dstDir := filepath.Join(tempDir, "dst_dir")
		err = CopyFile(ctx, srcDir, dstDir)
		require.NoError(t, err)

		_, err = os.Stat(dstDir)
		assert.NoError(t, err)

		content1, err := os.ReadFile(filepath.Join(dstDir, "file1.txt"))
		require.NoError(t, err)
		assert.Equal(t, []byte("content1"), content1)

		content2, err := os.ReadFile(filepath.Join(dstDir, "subdir", "file2.txt"))
		require.NoError(t, err)
		assert.Equal(t, []byte("content2"), content2)
	})

	t.Run("source not exist", func(t *testing.T) {
		err := CopyFile(ctx, "/nonexistent/path", filepath.Join(tempDir, "dest"))
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to stat source path")
	})
}

func BenchmarkCopyFileFast(b *testing.B) {
	tempDir := b.TempDir()

	srcFile := filepath.Join(tempDir, "source.bin")
	content := make([]byte, 1024*1024)
	for i := range content {
		content[i] = byte(i % 256)
	}
	err := os.WriteFile(srcFile, content, 0644)
	require.NoError(b, err)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dstFile := filepath.Join(tempDir, fmt.Sprintf("dest_%d.bin", i))
		err := copyFileFast(srcFile, dstFile)
		require.NoError(b, err)
	}
}

func BenchmarkCopyfile(b *testing.B) {
	tempDir := b.TempDir()
	ctx := namespaces.WithNamespace(context.Background(), "bench-ns")

	srcDir := filepath.Join(tempDir, "src")
	err := os.MkdirAll(srcDir, 0755)
	require.NoError(b, err)

	for i := 0; i < 10; i++ {
		filename := filepath.Join(srcDir, fmt.Sprintf("file_%d.txt", i))
		err := os.WriteFile(filename, []byte("test content"), 0644)
		require.NoError(b, err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dstDir := filepath.Join(tempDir, fmt.Sprintf("dst_%d", i))
		err := CopyFile(ctx, srcDir, dstDir)
		require.NoError(b, err)
	}
}
