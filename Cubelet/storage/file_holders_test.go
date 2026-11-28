// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// ownProcRoot returns a /proc look-alike that lists only this process, plus a
// pid that does not exist, so the result does not depend on other processes
// on the host (which a non-root test run may not be allowed to read). Its
// self/ns/pid claims the host PID namespace, so tests also run in containers.
func ownProcRoot(t *testing.T) string {
	t.Helper()
	self := filepath.Join("/proc", strconv.Itoa(os.Getpid()))
	if _, err := os.Stat(filepath.Join(self, "fd")); err != nil {
		t.Skip("no /proc on this host")
	}
	root := t.TempDir()
	require.NoError(t, os.Symlink(self, filepath.Join(root, strconv.Itoa(os.Getpid()))))
	require.NoError(t, os.Symlink("/proc/2147483646", filepath.Join(root, "2147483646")))
	writeSelfPIDNamespace(t, root, initPIDNamespace)
	return root
}

func writeSelfPIDNamespace(t *testing.T, root, ns string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "self", "ns"), 0o700))
	require.NoError(t, os.Symlink(ns, filepath.Join(root, "self", "ns", "pid")))
}

func TestFileHolders(t *testing.T) {
	root := ownProcRoot(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "disk")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))

	pids, err := fileHolders(root, path)
	require.NoError(t, err)
	require.NotContains(t, pids, os.Getpid())

	f, err := os.Open(path)
	require.NoError(t, err)
	// Reached through another name, the open file is still found.
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(path, link))
	pids, err = fileHolders(root, link)
	require.NoError(t, err)
	require.Contains(t, pids, os.Getpid())

	require.NoError(t, f.Close())
	pids, err = fileHolders(root, path)
	require.NoError(t, err)
	require.NotContains(t, pids, os.Getpid())

	// A mapping keeps the file in use after its descriptor is closed.
	f, err = os.Open(path)
	require.NoError(t, err)
	mem, err := syscall.Mmap(int(f.Fd()), 0, 1, syscall.PROT_READ, syscall.MAP_SHARED)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	pids, err = fileHolders(root, path)
	require.NoError(t, err)
	require.Contains(t, pids, os.Getpid())
	require.NoError(t, syscall.Munmap(mem))
	pids, err = fileHolders(root, path)
	require.NoError(t, err)
	require.NotContains(t, pids, os.Getpid())

	_, err = fileHolders(root, filepath.Join(dir, "missing"))
	require.Error(t, err)
}

// When the scan cannot tell, it reports an error, so the caller keeps the
// file instead of assuming nobody holds it.
func TestFileHoldersFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	cases := []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{
			// fd is a file, not a directory: reading it fails with ENOTDIR.
			name: "unreadable process",
			setup: func(t *testing.T, root string) {
				writeSelfPIDNamespace(t, root, initPIDNamespace)
				require.NoError(t, os.Mkdir(filepath.Join(root, "1"), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(root, "1", "fd"), nil, 0o600))
			},
		},
		{
			name: "nested PID namespace",
			setup: func(t *testing.T, root string) {
				writeSelfPIDNamespace(t, root, "pid:[4026533584]")
			},
		},
		{
			name:  "no self/ns/pid",
			setup: func(*testing.T, string) {},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			_, err := fileHolders(root, path)
			require.Error(t, err)
		})
	}
}
