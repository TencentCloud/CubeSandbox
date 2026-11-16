// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"testing"
)

func TestJFSMemoryPrefetchConfig(t *testing.T) {
	off, on := false, true
	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{"nil config", nil, false},
		{"jfs disabled", &Config{}, false},
		{"default off", func() *Config { c := &Config{}; c.Cow.JFS.Enable = true; return c }(), false},
		{"explicit on", func() *Config { c := &Config{}; c.Cow.JFS.Enable = true; c.Cow.JFS.MemoryPrefetch = &on; return c }(), true},
		{"explicit off", func() *Config { c := &Config{}; c.Cow.JFS.Enable = true; c.Cow.JFS.MemoryPrefetch = &off; return c }(), false},
	}
	for _, tc := range cases {
		if got := tc.cfg.jfsMemoryPrefetchEnabled(); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestWarmJFSRestoreMemoryIgnoresPathsOffTheMount(t *testing.T) {
	c := &Config{}
	c.Cow.JFS.Enable = true
	c.Cow.JFS.MountPath = "/nonexistent/jfs"
	l := &local{config: c}
	// Neither call may touch the filesystem or panic: the first is off the
	// JuiceFS mount, the second is an empty path.
	l.warmJFSRestoreMemory(context.Background(), "sb", "file:///data/cubelet/storage/xfs/objects/x")
	l.warmJFSRestoreMemory(context.Background(), "sb", "")
}

func TestJFSFuseReadAheadConfig(t *testing.T) {
	zero, custom, negative := 0, 4096, -1
	jfs := func(v *int) *Config { c := &Config{}; c.Cow.JFS.Enable = true; c.Cow.JFS.FuseReadAheadKB = v; return c }
	cases := []struct {
		name string
		cfg  *Config
		want int
	}{
		{"nil config", nil, 0},
		{"jfs disabled", &Config{}, 0},
		{"default", jfs(nil), defaultJFSFuseReadAheadKB},
		{"explicit value", jfs(&custom), 4096},
		{"zero leaves kernel default", jfs(&zero), 0},
		{"negative leaves kernel default", jfs(&negative), 0},
	}
	for _, tc := range cases {
		if got := tc.cfg.jfsFuseReadAheadKB(); got != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
	if defaultJFSFuseReadAheadKB != 16384 {
		t.Errorf("default read-ahead changed to %d KiB", defaultJFSFuseReadAheadKB)
	}
}

func TestApplyJFSFuseReadAheadNoopWhenDisabled(t *testing.T) {
	zero := 0
	c := &Config{}
	c.Cow.JFS.Enable = true
	c.Cow.JFS.MountPath = "/nonexistent/jfs"
	c.Cow.JFS.FuseReadAheadKB = &zero
	if err := c.applyJFSFuseReadAhead(); err != nil {
		t.Fatalf("read-ahead must be a no-op when set to 0: %v", err)
	}
	if err := (&Config{}).applyJFSFuseReadAhead(); err != nil {
		t.Fatalf("read-ahead must be a no-op without jfs: %v", err)
	}
}

func TestApplyJFSFuseReadAheadDefaultNeedsMount(t *testing.T) {
	c := &Config{}
	c.Cow.JFS.Enable = true
	c.Cow.JFS.MountPath = "/nonexistent/jfs"
	// The default (16 MiB) is enforced, so a missing mount is reported
	// rather than silently skipped.
	if err := c.applyJFSFuseReadAhead(); err == nil {
		t.Fatal("expected an error for a missing JuiceFS mount")
	}
}
