// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cosfs

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func requireMountpoint(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mountpoint"); err != nil {
		t.Skip("mountpoint not installed")
	}
}

func TestIsMountPoint_missingPath(t *testing.T) {
	requireMountpoint(t)

	mounted, err := isMountPoint(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("isMountPoint: %v", err)
	}
	if mounted {
		t.Fatal("expected not mounted for missing path")
	}
}

func TestIsMountPoint_existingDirNotMount(t *testing.T) {
	requireMountpoint(t)

	dir := t.TempDir()
	child := filepath.Join(dir, "cos-test-vol")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}

	mounted, err := isMountPoint(child)
	if err != nil {
		t.Fatalf("isMountPoint: %v", err)
	}
	if mounted {
		t.Fatal("expected not mounted for plain directory")
	}
}

func TestRemoveMountDir(t *testing.T) {
	dir := t.TempDir()
	mnt := filepath.Join(dir, "cos-test-vol")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeMountDir(mnt); err != nil {
		t.Fatalf("removeMountDir: %v", err)
	}
	if _, err := os.Stat(mnt); !os.IsNotExist(err) {
		t.Fatalf("expected mount dir removed, stat: %v", err)
	}
}
