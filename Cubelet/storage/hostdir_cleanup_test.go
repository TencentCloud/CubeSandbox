// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/sys/mountinfo"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type fakeHostDirMount struct {
	path   string
	layers int // mounts stacked on path; each successful unmount removes one
}

// withFakeHostDirMounts replaces the mountinfo listing and umount2 used by
// detachHostDirMounts. Mountpoints exist only in the fake table, so lstat on
// them is never what drives the unmount, as with a deleted FUSE bind source.
func withFakeHostDirMounts(t *testing.T, mounts []*fakeHostDirMount, unmountErr map[string]error) *[]string {
	t.Helper()
	oldList, oldUnmount := listHostDirMounts, unmountHostDir
	t.Cleanup(func() { listHostDirMounts, unmountHostDir = oldList, oldUnmount })

	var calls []string
	listHostDirMounts = func(filter mountinfo.FilterFunc) ([]*mountinfo.Info, error) {
		var out []*mountinfo.Info
		for _, m := range mounts {
			for i := 0; i < m.layers; i++ {
				info := &mountinfo.Info{Mountpoint: m.path}
				if skip, _ := filter(info); !skip {
					out = append(out, info)
				}
			}
		}
		return out, nil
	}
	unmountHostDir = func(target string, flags int) error {
		require.Equal(t, unix.MNT_DETACH, flags)
		calls = append(calls, target)
		if err, ok := unmountErr[target]; ok {
			return err
		}
		for _, m := range mounts {
			if m.path == target && m.layers > 0 {
				m.layers--
				return nil
			}
		}
		return unix.EINVAL
	}
	return &calls
}

func newHostDirCleanupStorage(t *testing.T) *local {
	t.Helper()
	oldHostDirBasePath := hostDirBasePath
	hostDirBasePath = t.TempDir()
	t.Cleanup(func() { hostDirBasePath = oldHostDirBasePath })
	return &local{}
}

func hostDirCleanupInfo(sandboxID string) *StorageInfo {
	return &StorageInfo{
		SandboxID: sandboxID,
		HostDirBackendInfos: map[string]*HostDirBackendInfo{
			"volume/hostdir-0": {VolumeName: "volume"},
		},
	}
}

func TestCleanupHostDirVolumesDetachesMountWhoseMountpointCannotBeStated(t *testing.T) {
	s := newHostDirCleanupStorage(t)
	sandboxDir := filepath.Join(hostDirBasePath, "stale-sb")
	require.NoError(t, os.MkdirAll(filepath.Join(sandboxDir, "rw"), 0o755))
	// The bind target is listed in mountinfo but absent on disk: lstat fails
	// with ENOENT, as it does on a bind whose FUSE source was deleted.
	stale := filepath.Join(sandboxDir, "rw", "hostdir-0")
	calls := withFakeHostDirMounts(t, []*fakeHostDirMount{{path: stale, layers: 1}}, nil)

	require.NoError(t, s.cleanupHostDirVolumes(context.Background(), hostDirCleanupInfo("stale-sb")))
	require.Equal(t, []string{stale, stale}, *calls, "unmount until EINVAL")
	require.NoDirExists(t, sandboxDir)
}

func TestCleanupHostDirVolumesDetachesStackedMountsDeepestFirst(t *testing.T) {
	s := newHostDirCleanupStorage(t)
	sandboxDir := filepath.Join(hostDirBasePath, "stacked-sb")
	rw := filepath.Join(sandboxDir, "rw")
	nested := filepath.Join(rw, "hostdir-0", "sub")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	calls := withFakeHostDirMounts(t, []*fakeHostDirMount{
		{path: rw, layers: 1},
		{path: nested, layers: 2},
		{path: hostDirBasePath + "/stacked-sb-sibling", layers: 1},
	}, nil)

	require.NoError(t, s.cleanupHostDirVolumes(context.Background(), hostDirCleanupInfo("stacked-sb")))
	require.Equal(t, []string{nested, nested, nested, rw, rw}, *calls)
	require.NoDirExists(t, sandboxDir)
}

func TestCleanupHostDirVolumesKeepsSandboxDirWhenDetachFails(t *testing.T) {
	s := newHostDirCleanupStorage(t)
	sandboxDir := filepath.Join(hostDirBasePath, "busy-sb")
	target := filepath.Join(sandboxDir, "rw", "hostdir-0")
	require.NoError(t, os.MkdirAll(target, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(target, "data"), []byte("keep"), 0o644))
	withFakeHostDirMounts(t, []*fakeHostDirMount{{path: target, layers: 1}},
		map[string]error{target: unix.EPERM})

	err := s.cleanupHostDirVolumes(context.Background(), hostDirCleanupInfo("busy-sb"))
	require.ErrorIs(t, err, unix.EPERM)
	require.FileExists(t, filepath.Join(target, "data"), "cleanup must not remove anything after a failed unmount")
}

func TestCleanupHostDirVolumesUnmountsRealBindAndKeepsSource(t *testing.T) {
	requireRoot(t)
	s := newHostDirCleanupStorage(t)
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "data"), []byte("keep"), 0o644))
	target := filepath.Join(hostDirBasePath, "bind-sb", "rw", "hostdir-0")
	require.NoError(t, os.MkdirAll(target, 0o755))
	require.NoError(t, unix.Mount(source, target, "", unix.MS_BIND, ""))
	t.Cleanup(func() { _ = unix.Unmount(target, unix.MNT_DETACH) })

	require.NoError(t, s.cleanupHostDirVolumes(context.Background(), hostDirCleanupInfo("bind-sb")))
	require.NoDirExists(t, filepath.Join(hostDirBasePath, "bind-sb"))
	require.FileExists(t, filepath.Join(source, "data"))
}
