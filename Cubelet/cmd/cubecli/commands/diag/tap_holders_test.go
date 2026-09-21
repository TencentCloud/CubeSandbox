// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package diag

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProcEntry writes one /proc/<pid>/fd/<fd> symlink plus its fdinfo.
func fakeProcEntry(t *testing.T, procRoot, pid, fd, target, fdinfo string) {
	t.Helper()
	fdDir := filepath.Join(procRoot, pid, "fd")
	fdinfoDir := filepath.Join(procRoot, pid, "fdinfo")
	require.NoError(t, os.MkdirAll(fdDir, 0o755))
	require.NoError(t, os.MkdirAll(fdinfoDir, 0o755))
	require.NoError(t, os.Symlink(target, filepath.Join(fdDir, fd)))
	require.NoError(t, os.WriteFile(filepath.Join(fdinfoDir, fd), []byte(fdinfo), 0o644))
}

func TestScanTunHolders(t *testing.T) {
	procRoot := t.TempDir()
	fakeProcEntry(t, procRoot, "100", "7", tunDevice, "pos:\t0\nflags:\t02\niff:\ttap0\n")
	// A second tun fd on the same process, not yet attached to a device.
	fakeProcEntry(t, procRoot, "100", "9", tunDevice, "pos:\t0\nflags:\t02\n")
	// A different process holding a different tap.
	fakeProcEntry(t, procRoot, "200", "3", tunDevice, "iff:\ttap1\n")
	// Unrelated fds and non-process directories must be ignored.
	fakeProcEntry(t, procRoot, "300", "4", "/dev/null", "pos:\t0\n")
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, "self", "fd"), 0o755))

	holders, err := scanTunHolders(procRoot)
	require.NoError(t, err)
	require.Len(t, holders, 3)

	assert.Equal(t, 100, holders[0].Pid)
	assert.Equal(t, "7", holders[0].Fd)
	assert.Equal(t, "tap0", holders[0].Iface)

	assert.Equal(t, 100, holders[1].Pid)
	assert.Equal(t, "9", holders[1].Fd)
	assert.Empty(t, holders[1].Iface, "an unattached fd holds no device")

	assert.Equal(t, 200, holders[2].Pid)
	assert.Equal(t, "tap1", holders[2].Iface)
}

func TestScanTunHoldersSkipsUnreadableProcesses(t *testing.T) {
	procRoot := t.TempDir()
	fakeProcEntry(t, procRoot, "100", "7", tunDevice, "iff:\ttap0\n")
	// A process directory with no fd dir, as happens when a process exits
	// mid-scan. A partial answer beats failing the whole command.
	require.NoError(t, os.MkdirAll(filepath.Join(procRoot, "101"), 0o755))

	holders, err := scanTunHolders(procRoot)
	require.NoError(t, err)
	require.Len(t, holders, 1)
	assert.Equal(t, 100, holders[0].Pid)
}

func TestReadTunIface(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fdinfo")

	require.NoError(t, os.WriteFile(path, []byte("pos:\t0\nflags:\t02\nmnt_id:\t15\niff:\ttap42\n"), 0o644))
	assert.Equal(t, "tap42", readTunIface(path))

	require.NoError(t, os.WriteFile(path, []byte("pos:\t0\nflags:\t02\n"), 0o644))
	assert.Empty(t, readTunIface(path))

	assert.Empty(t, readTunIface(filepath.Join(dir, "missing")))
}
