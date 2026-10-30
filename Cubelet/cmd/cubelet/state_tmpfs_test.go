// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/moby/sys/mountinfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
	"golang.org/x/sys/unix"
)

func TestStateTmpfsSymlinkRestartPreservesState(t *testing.T) {
	if os.Getenv("CUBELET_TEST_MOUNT_CHILD") != "1" {
		if os.Getenv("CUBELET_REQUIRE_MOUNT_TESTS") != "1" {
			t.Skip("set CUBELET_REQUIRE_MOUNT_TESTS=1 for isolated Linux mount regression")
		}
		cmd := exec.Command("unshare", "--mount", "--propagation", "private", os.Args[0],
			"-test.run=^TestStateTmpfsSymlinkRestartPreservesState$", "-test.v")
		cmd.Env = append(os.Environ(), "CUBELET_TEST_MOUNT_CHILD=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		t.Logf("%s", output)
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
	before, err := lookupExactMount(realPath)
	require.NoError(t, err)
	require.NotNil(t, before)
	for i := 0; i < 3; i++ {
		require.NoError(t, mountTmpfsDir(statePath, ctx))
		after, err := lookupExactMount(statePath)
		require.NoError(t, err)
		require.NotNil(t, after)
		require.Equal(t, before.ID, after.ID)
		data, err := os.ReadFile(marker)
		require.NoError(t, err)
		require.Equal(t, "must survive restart", string(data))
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
