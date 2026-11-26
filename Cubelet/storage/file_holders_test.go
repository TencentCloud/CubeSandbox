// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileHolders(t *testing.T) {
	if _, err := os.Stat(filepath.Join(procRoot, "self", "fd")); err != nil {
		t.Skip("no /proc on this host")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "disk")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))

	pids, err := fileHolders(procRoot, path)
	require.NoError(t, err)
	require.NotContains(t, pids, os.Getpid())

	f, err := os.Open(path)
	require.NoError(t, err)
	// Reached through another name, the open file is still found.
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(path, link))
	pids, err = fileHolders(procRoot, link)
	require.NoError(t, err)
	require.Contains(t, pids, os.Getpid())

	require.NoError(t, f.Close())
	pids, err = fileHolders(procRoot, path)
	require.NoError(t, err)
	require.NotContains(t, pids, os.Getpid())

	// A mapping keeps the file in use after its descriptor is closed.
	f, err = os.Open(path)
	require.NoError(t, err)
	mem, err := syscall.Mmap(int(f.Fd()), 0, 1, syscall.PROT_READ, syscall.MAP_SHARED)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	pids, err = fileHolders(procRoot, path)
	require.NoError(t, err)
	require.Contains(t, pids, os.Getpid())
	require.NoError(t, syscall.Munmap(mem))
	pids, err = fileHolders(procRoot, path)
	require.NoError(t, err)
	require.NotContains(t, pids, os.Getpid())

	_, err = fileHolders(procRoot, filepath.Join(dir, "missing"))
	require.Error(t, err)
}
