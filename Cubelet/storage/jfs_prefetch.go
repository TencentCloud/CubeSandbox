// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/jfsdiff"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

const (
	// Skip warming when this much of the image is already cached (a
	// same-node resume of a just-paused sandbox, a template in use here).
	jfsMemoryWarmSkipFraction = 0.95
	jfsMemoryWarmWorkers      = 8
	jfsMemoryWarmTimeout      = 2 * time.Minute

	// defaultJFSFuseReadAheadKB is the JuiceFS mount read_ahead_kb used when
	// [cow.jfs] fuse_read_ahead_kb is unset.
	defaultJFSFuseReadAheadKB = 16384
)

// jfsMemoryPrefetchEnabled reports whether jfs restores warm guest memory
// ([cow.jfs] memory_prefetch, default false).
func (c *Config) jfsMemoryPrefetchEnabled() bool {
	if !c.jfsConfigured() {
		return false
	}
	return c.Cow.JFS.MemoryPrefetch != nil && *c.Cow.JFS.MemoryPrefetch
}

// jfsFuseReadAheadKB is the read_ahead_kb to enforce on the JuiceFS mount;
// 0 means leave the kernel value alone.
func (c *Config) jfsFuseReadAheadKB() int {
	if !c.jfsConfigured() {
		return 0
	}
	if c.Cow.JFS.FuseReadAheadKB == nil {
		return defaultJFSFuseReadAheadKB
	}
	if v := *c.Cow.JFS.FuseReadAheadKB; v > 0 {
		return v
	}
	return 0
}

// warmJFSRestoreMemory pulls the memory image the VM is about to map
// MAP_PRIVATE from the JuiceFS mount into the kernel page cache. It runs on
// the memory branch of Create, concurrently with rootfs preparation, and
// Create waits for it before the VM is started.
//
// Failure only costs speed: the VM still faults pages in on demand, so
// errors are logged and not returned.
func (l *local) warmJFSRestoreMemory(ctx context.Context, sandboxID, devPath string) {
	if l == nil || !l.config.jfsMemoryPrefetchEnabled() {
		return
	}
	path := strings.TrimPrefix(strings.TrimSpace(devPath), "file://")
	if path == "" || !strings.HasPrefix(filepath.Clean(path), l.config.jfsMountPath()+string(os.PathSeparator)) {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, jfsMemoryWarmTimeout)
	defer cancel()
	st, err := jfsdiff.WarmFile(wctx, path, jfsMemoryWarmWorkers, jfsMemoryWarmSkipFraction)
	if err != nil {
		CubeLog.Warnf("jfs memory prefetch for sandbox %s (%s): %v after %s (read %d of %d bytes); falling back to on-demand faults",
			sandboxID, path, err, st.Duration.Round(time.Millisecond), st.ReadBytes, st.SizeBytes)
		return
	}
	if st.Skipped {
		return
	}
	mbps := 0.0
	if s := st.Duration.Seconds(); s > 0 {
		mbps = float64(st.ReadBytes) / s / (1 << 20)
	}
	CubeLog.Warnf("jfs memory prefetch for sandbox %s: read %d bytes (%d of %d already cached) in %s (%.0f MiB/s)",
		sandboxID, st.ReadBytes, st.ResidentBefore, st.SizeBytes, st.Duration.Round(time.Millisecond), mbps)
}

// applyJFSFuseReadAhead sets read_ahead_kb on the JuiceFS mount's BDI (see
// jfsFuseReadAheadKB). It runs at init and again before each jfs restore
// opens a memory image: the value belongs to the current mount, so a
// JuiceFS remount resets it to the kernel default, and the kernel copies it
// into a file's read-ahead state at open, so it must be in place before the
// VM opens the image.
func (c *Config) applyJFSFuseReadAhead() error {
	kb := c.jfsFuseReadAheadKB()
	if kb <= 0 {
		return nil
	}
	bdi, err := mountBDI(c.jfsMountPath())
	if err != nil {
		return err
	}
	knob := filepath.Join("/sys/class/bdi", bdi, "read_ahead_kb")
	want := fmt.Sprintf("%d", kb)
	if cur, err := os.ReadFile(knob); err == nil && strings.TrimSpace(string(cur)) == want {
		return nil
	}
	if err := os.WriteFile(knob, []byte(want), 0o644); err != nil {
		return fmt.Errorf("set %s=%s: %w", knob, want, err)
	}
	CubeLog.Warnf("jfs: set %s read_ahead_kb=%s", c.jfsMountPath(), want)
	return nil
}
