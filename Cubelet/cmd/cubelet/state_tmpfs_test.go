// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/sys/mountinfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
	"golang.org/x/sys/unix"
)

func TestStateTmpfsSymlinkRestartPreservesState(t *testing.T) {
	if runMountNamespace(t) {
		return
	}
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real", "state")
	require.NoError(t, os.MkdirAll(realPath, 0o700))
	alias := filepath.Join(dir, "alias")
	require.NoError(t, os.Symlink(filepath.Join(dir, "real"), alias))
	statePath := filepath.Join(alias, "state")
	flags := flag.NewFlagSet("state", flag.ContinueOnError)
	flags.Int("state-tmpfs-size", 16, "")
	ctx := cli.NewContext(cli.NewApp(), flags, nil)
	require.NoError(t, mountTmpfsDir(statePath, ctx))
	defer unix.Unmount(realPath, 0)
	marker := filepath.Join(realPath, "live-shim-state")
	require.NoError(t, os.WriteFile(marker, []byte("must survive restart"), 0o600))
	before := kernelMountID(t, realPath)
	for _, size := range []string{"16", "32", "16"} {
		require.NoError(t, flags.Set("state-tmpfs-size", size))
		require.NoError(t, mountTmpfsDir(statePath, ctx))
		after, err := lookupExactMount(statePath)
		require.NoError(t, err)
		require.NotNil(t, after)
		require.Equal(t, before, uint64(after.ID))
		require.Equal(t, before, kernelMountID(t, statePath))
		actual, ok := parseTmpfsSizeBytes(after.VFSOptions)
		require.True(t, ok)
		require.Equal(t, tmpfsSizeFromMB(ctx.Int("state-tmpfs-size")), actual)
		data, err := os.ReadFile(marker)
		require.NoError(t, err)
		require.Equal(t, "must survive restart", string(data))
	}
	// Historical restarts can leave stacked mounts. Inspect the visible layer.
	require.NoError(t, unix.Mount("none", statePath, "tmpfs", 0, "size=32m"))
	defer unix.Unmount(realPath, 0)
	top, err := lookupExactMount(statePath)
	require.NoError(t, err)
	require.NotNil(t, top)
	require.NotEqual(t, before, uint64(top.ID))
	require.Equal(t, kernelMountID(t, statePath), uint64(top.ID))
	size, ok := parseTmpfsSizeBytes(top.VFSOptions)
	require.True(t, ok)
	require.Equal(t, uint64(32*1024*1024), size)
}

func TestFreshStateTmpfsVerificationFailures(t *testing.T) {
	if runMountNamespace(t) {
		return
	}
	for _, tc := range []struct {
		name string
		info *mountinfo.Info
		err  error
		want string
	}{
		{name: "wrong filesystem", info: &mountinfo.Info{FSType: "ext4"}, want: "expected tmpfs, got ext4"},
		{name: "no visible mount", want: "no visible mount"},
		{name: "lookup failure", err: unix.EIO, want: "input/output error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir()
			t.Cleanup(func() { require.NoError(t, unix.Unmount(path, 0)) })
			called := false
			err := mountFreshStateTmpfsWithLookup(path, 16, func(got string) (*mountinfo.Info, error) {
				called = true
				require.Equal(t, path, got)
				// Mount a real tmpfs, then inject only the verification result.
				actual, err := lookupExactMount(got)
				require.NoError(t, err)
				require.NotNil(t, actual)
				require.Equal(t, "tmpfs", actual.FSType)
				return tc.info, tc.err
			})
			require.True(t, called)
			require.ErrorContains(t, err, "verify tmpfs mount "+path+": "+tc.want)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
			}
		})
	}
}

func TestLookupExactMountResolvesSymlinkParent(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(dir, "root-alias")
	require.NoError(t, os.Symlink("/", alias))
	// /proc is an existing Linux mount, so this needs no mount privileges.
	want, err := lookupExactMount("/proc")
	require.NoError(t, err)
	require.NotNil(t, want)
	got, err := lookupExactMount(filepath.Join(alias, "proc"))
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, want.ID, got.ID)
	require.Equal(t, "/proc", got.Mountpoint)
	t.Chdir(dir)
	relative, err := lookupExactMount("root-alias/proc")
	require.NoError(t, err)
	require.Equal(t, want.ID, relative.ID)
}

func TestLookupExactMountRejectsBrokenSymlink(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(dir, "broken")
	require.NoError(t, os.Symlink(filepath.Join(dir, "missing"), alias))
	_, err := lookupExactMount(alias)
	require.Error(t, err)
}

func TestParseTmpfsSizeBytes(t *testing.T) {
	tests := []struct {
		name   string
		opts   string
		want   uint64
		wantOK bool
	}{
		{
			name:   "kernel kilobytes for 500m",
			opts:   "rw,size=512000k,nr_inodes=1048576",
			want:   500 * 1024 * 1024,
			wantOK: true,
		},
		{
			name:   "kernel kilobytes for 1024m",
			opts:   "rw,size=1048576k",
			want:   1024 * 1024 * 1024,
			wantOK: true,
		},
		{
			name:   "explicit megabytes",
			opts:   "rw,size=500m",
			want:   500 * 1024 * 1024,
			wantOK: true,
		},
		{
			name:   "explicit gigabytes",
			opts:   "size=1g",
			want:   1024 * 1024 * 1024,
			wantOK: true,
		},
		{
			name:   "bare bytes",
			opts:   "size=524288000",
			want:   524288000,
			wantOK: true,
		},
		{
			name:   "percent of ram unsupported",
			opts:   "rw,size=50%",
			wantOK: false,
		},
		{
			name:   "missing size",
			opts:   "rw,nr_inodes=1024",
			wantOK: false,
		},
		{
			name:   "empty",
			opts:   "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseTmpfsSizeBytes(tt.opts)
			require.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestTmpfsSizeEqual(t *testing.T) {
	page := uint64(unix.Getpagesize())
	base := uint64(1024 * 1024 * 1024)

	assert.True(t, tmpfsSizeEqual(base, base))
	assert.True(t, tmpfsSizeEqual(base, base+page-1))
	assert.False(t, tmpfsSizeEqual(base, base+page))
	assert.False(t, tmpfsSizeEqual(500*1024*1024, 1024*1024*1024))
}

func TestMaybeRemountStateTmpfsSkipsNonTmpfs(t *testing.T) {
	err := maybeRemountStateTmpfs("/data/cubelet/state", &mountinfo.Info{
		FSType:     "ext4",
		VFSOptions: "rw",
	}, 1024)
	require.NoError(t, err)
}

func TestMaybeRemountStateTmpfsNoopWhenSizeMatches(t *testing.T) {
	err := maybeRemountStateTmpfs("/data/cubelet/state", &mountinfo.Info{
		FSType:     "tmpfs",
		VFSOptions: "rw,size=1048576k",
	}, 1024)
	require.NoError(t, err)
}

func TestMaybeRemountStateTmpfsRefuseShrinkWhenUsedExceeds(t *testing.T) {
	// Statfs on a normal temp dir reports the backing filesystem's used bytes,
	// which is >> 1. With a claimed current size of 1Gi, the shrink guard must
	// refuse and return without attempting remount.
	dir := t.TempDir()
	err := maybeRemountStateTmpfs(dir, &mountinfo.Info{
		FSType:     "tmpfs",
		VFSOptions: "rw,size=1048576k",
	}, 1)
	require.NoError(t, err)
}

func TestPathFDMountIDMatchesVisibleRoot(t *testing.T) {
	id, err := mountIDFromPathFD("/")
	require.NoError(t, err)
	info, err := lookupExactMount("/")
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, uint64(info.ID), id)
}

func TestVisibleMountIDReportsBothLookupFailures(t *testing.T) {
	_, err := visibleMountID(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "statx")
	require.Contains(t, err.Error(), "fdinfo")
	require.ErrorIs(t, err, unix.ENOENT)
}

// Keep all mount changes in a child namespace with one-way propagation.
func runMountNamespace(t *testing.T) bool {
	t.Helper()
	if os.Getenv("CUBELET_TEST_MOUNT_CHILD") == "1" {
		return false
	}
	if os.Getenv("CUBELET_REQUIRE_MOUNT_TESTS") != "1" {
		t.Skip("set CUBELET_REQUIRE_MOUNT_TESTS=1 for isolated Linux mount regression")
	}
	cmd := exec.Command("unshare", "--mount", "--propagation", "slave", os.Args[0], "-test.run=^"+strings.Split(t.Name(), "/")[0]+"$", "-test.v")
	cmd.Env = append(os.Environ(), "CUBELET_TEST_MOUNT_CHILD=1")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Logf("%s", output)
	return true
}

func kernelMountID(t *testing.T, path string) uint64 {
	t.Helper()
	var st unix.Statx_t
	require.NoError(t, unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_MNT_ID, &st))
	require.NotZero(t, st.Mask&unix.STATX_MNT_ID)
	return st.Mnt_id
}

// An overmounted ancestor leaves the old child mounts in mountinfo but hides
// them from pathname lookup, even when only one old child mount exists.
func coveredMountPath(t *testing.T, layers int, shared bool) string {
	t.Helper()
	parent := t.TempDir()
	require.NoError(t, unix.Mount("none", parent, "tmpfs", 0, "size=64m"))
	t.Cleanup(func() { require.NoError(t, unix.Unmount(parent, unix.MNT_DETACH)) })
	child := filepath.Join(parent, "state")
	require.NoError(t, os.Mkdir(child, 0700))
	for i := 0; i < layers; i++ {
		require.NoError(t, unix.Mount("none", child, "tmpfs", 0, "size=16m"))
	}
	if shared {
		require.NoError(t, unix.Mount("", child, "", unix.MS_SHARED, ""))
	}
	require.NoError(t, unix.Mount("none", parent, "tmpfs", 0, "size=64m"))
	t.Cleanup(func() { require.NoError(t, unix.Unmount(parent, unix.MNT_DETACH)) })
	require.NoError(t, os.Mkdir(child, 0700))
	entries, err := mountinfo.GetMounts(func(info *mountinfo.Info) (bool, bool) { return info.Mountpoint != child, false })
	require.NoError(t, err)
	require.Len(t, entries, layers)
	for _, entry := range entries {
		require.NotEqual(t, uint64(entry.ID), kernelMountID(t, child))
	}
	return child
}

func TestStateTmpfsCoveredAncestor(t *testing.T) {
	if runMountNamespace(t) {
		return
	}
	for _, layers := range []int{1, 2} {
		t.Run(fmt.Sprint(layers), func(t *testing.T) {
			path := coveredMountPath(t, layers, false)
			info, err := lookupExactMount(path)
			require.NoError(t, err)
			require.Nil(t, info)
			flags := flag.NewFlagSet("state", flag.ContinueOnError)
			flags.Int("state-tmpfs-size", 16, "")
			ctx := cli.NewContext(cli.NewApp(), flags, nil)
			require.NoError(t, mountTmpfsDir(path, ctx))
			before := kernelMountID(t, path)
			marker := filepath.Join(path, "live-state")
			require.NoError(t, os.WriteFile(marker, []byte("retained"), 0600))
			require.NoError(t, flags.Set("state-tmpfs-size", "32"))
			require.NoError(t, mountTmpfsDir(path, ctx))
			require.Equal(t, before, kernelMountID(t, path))
			data, err := os.ReadFile(marker)
			require.NoError(t, err)
			require.Equal(t, "retained", string(data))
			info, err = lookupExactMount(path)
			require.NoError(t, err)
			require.NotNil(t, info)
			require.Equal(t, before, uint64(info.ID))
			size, ok := parseTmpfsSizeBytes(info.VFSOptions)
			require.True(t, ok)
			require.Equal(t, uint64(32*1024*1024), size)
		})
	}
}
