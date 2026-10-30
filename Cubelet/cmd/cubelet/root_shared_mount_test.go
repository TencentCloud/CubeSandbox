// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"os/exec"
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
	if os.Getenv("CUBELET_TEST_ROOT_MOUNT_CHILD") != "1" {
		if os.Getenv("CUBELET_REQUIRE_MOUNT_TESTS") != "1" {
			t.Skip("set CUBELET_REQUIRE_MOUNT_TESTS=1 for isolated Linux mount regression")
		}
		cmd := exec.Command("unshare", "--mount", "--propagation", "private", os.Args[0],
			"-test.run=^TestRootSharedMountSymlinkRestartDoesNotStack$", "-test.v")
		cmd.Env = append(os.Environ(), "CUBELET_TEST_ROOT_MOUNT_CHILD=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		t.Logf("%s", output)
		return
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real", "root")
	require.NoError(t, os.MkdirAll(real, 0o700))
	alias := filepath.Join(dir, "alias")
	require.NoError(t, os.Symlink(filepath.Join(dir, "real"), alias))
	root := filepath.Join(alias, "root")
	require.NoError(t, ensureRootSharedMount(root))
	defer unix.Unmount(real, 0)
	before, err := lookupExactMount(root)
	require.NoError(t, err)
	require.NotNil(t, before)
	marker := filepath.Join(real, "preserved-data")
	require.NoError(t, os.WriteFile(marker, []byte("unchanged"), 0o600))
	for i := 0; i < 16; i++ {
		require.NoError(t, ensureRootSharedMount(root))
		after, err := lookupExactMount(root)
		require.NoError(t, err)
		require.Equal(t, before.ID, after.ID)
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
