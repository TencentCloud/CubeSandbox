// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/moby/sys/mountinfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

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
