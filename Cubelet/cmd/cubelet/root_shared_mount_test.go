// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/sys/mountinfo"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestRootSharedMountDoesNotRebindExistingMount(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "private", true: "shared"}[shared], func(t *testing.T) {
			dir := t.TempDir()
			var flags []uintptr
			lookup := func(string) (*mountinfo.Info, error) {
				optional := "master:1"
				if shared {
					optional += " shared:42"
				}
				return &mountinfo.Info{Mountpoint: dir, Optional: optional}, nil
			}
			err := ensureRootSharedMountWithLookup(dir, lookup, func(_, _, _ string, flag uintptr, _ string) error {
				flags = append(flags, flag)
				return nil
			})
			require.NoError(t, err)
			if shared {
				require.Empty(t, flags)
			} else {
				require.Equal(t, []uintptr{unix.MS_SHARED}, flags)
			}
		})
	}
}

func TestRootSharedMountLookupFailureDoesNotMutateMounts(t *testing.T) {
	want := errors.New("mount inventory unavailable")
	err := ensureRootSharedMountWithLookup(t.TempDir(), func(string) (*mountinfo.Info, error) {
		return nil, want
	}, func(_, _, _ string, _ uintptr, _ string) error {
		t.Fatal("mount must not run after lookup failure")
		return nil
	})
	require.ErrorIs(t, err, want)
}

func TestRootSharedMountSymlinkRestartDoesNotStack(t *testing.T) {
	if runMountNamespace(t) {
		return
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real", "root")
	require.NoError(t, os.MkdirAll(real, 0o700))
	real, err := filepath.EvalSymlinks(real)
	require.NoError(t, err)
	alias := filepath.Join(dir, "alias")
	require.NoError(t, os.Symlink(filepath.Join(dir, "real"), alias))
	root := filepath.Join(alias, "root")
	require.NoError(t, ensureRootSharedMount(root))
	defer unix.Unmount(real, 0)
	before := kernelMountID(t, root)
	mounts, err := mountinfo.GetMounts(mountinfo.SingleEntryFilter(real))
	require.NoError(t, err)
	require.Len(t, mounts, 1)
	require.Contains(t, mounts[0].Optional, "shared:")
	marker := filepath.Join(real, "preserved-data")
	require.NoError(t, os.WriteFile(marker, []byte("unchanged"), 0o600))
	for i := 0; i < 16; i++ {
		require.NoError(t, ensureRootSharedMount(root))
		after, err := lookupExactMount(root)
		require.NoError(t, err)
		require.Equal(t, before, uint64(after.ID))
		require.Equal(t, before, kernelMountID(t, root))
		require.Contains(t, after.Optional, "shared:")
		mounts, err := mountinfo.GetMounts(nil)
		require.NoError(t, err)
		count := 0
		for _, info := range mounts {
			if info.Mountpoint == real {
				count++
			}
		}
		require.Equal(t, 1, count)
	}
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, "unchanged", string(data))
}

func TestRootSharedMountCoveredAncestor(t *testing.T) {
	if runMountNamespace(t) {
		return
	}
	for _, layers := range []int{1, 2} {
		t.Run(fmt.Sprint(layers), func(t *testing.T) {
			path := coveredMountPath(t, layers, true)
			before := kernelMountID(t, path)
			require.NoError(t, ensureRootSharedMount(path))
			visible := kernelMountID(t, path)
			require.NotEqual(t, before, visible, "the visible directory needs its own bind mount")
			entries, err := mountinfo.GetMounts(nil)
			require.NoError(t, err)
			found := false
			for _, entry := range entries {
				if uint64(entry.ID) == visible {
					require.Equal(t, path, entry.Mountpoint)
					require.Contains(t, entry.Optional, "shared:")
					found = true
				}
			}
			require.True(t, found)
			require.NoError(t, ensureRootSharedMount(path))
			require.Equal(t, visible, kernelMountID(t, path))
		})
	}
}
