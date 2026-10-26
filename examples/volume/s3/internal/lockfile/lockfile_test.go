// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package lockfile

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Environment variables used to drive the helper subprocess below.
const (
	envHelperDir   = "CUBE_S3_LOCKTEST_DIR"
	envHelperStamp = "CUBE_S3_LOCKTEST_STAMP"
)

func TestAcquireCreatesLockFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")

	lock, err := Acquire(dir, "vol-1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lock.Release()

	if _, err := os.Stat(filepath.Join(dir, "vol-1.lock")); err != nil {
		t.Errorf("lock file missing: %v", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	lock, err := Acquire(t.TempDir(), "vol-1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	lock.Release()
	lock.Release()

	var nilLock *Lock
	nilLock.Release()
}

func TestDifferentVolumesDoNotBlock(t *testing.T) {
	dir := t.TempDir()

	first, err := Acquire(dir, "vol-1")
	if err != nil {
		t.Fatalf("Acquire vol-1: %v", err)
	}
	defer first.Release()

	second, err := Acquire(dir, "vol-2")
	if err != nil {
		t.Fatalf("Acquire vol-2: %v", err)
	}
	second.Release()
}

// The plugin runs one process per operation, so the lock has to serialise
// separate processes. An in-process mutex would pass a same-process test but
// would not prevent two sandboxes from double-mounting the same volume.
func TestAcquireBlocksAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	stamp := filepath.Join(dir, "acquired")

	held, err := Acquire(dir, "vol-1")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	helper := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
	helper.Env = append(os.Environ(), envHelperDir+"="+dir, envHelperStamp+"="+stamp)
	helper.Stdout = os.Stderr
	helper.Stderr = os.Stderr
	if err := helper.Start(); err != nil {
		held.Release()
		t.Fatalf("start helper: %v", err)
	}
	defer func() {
		_ = helper.Process.Kill()
		_, _ = helper.Process.Wait()
	}()

	// While the parent holds the lock the helper must not get past Acquire.
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(stamp); err == nil {
		held.Release()
		t.Fatal("helper acquired the lock while it was held by this process")
	}

	held.Release()

	if err := helper.Wait(); err != nil {
		t.Fatalf("helper failed after lock release: %v", err)
	}
	if _, err := os.Stat(stamp); err != nil {
		t.Errorf("helper did not acquire the lock after release: %v", err)
	}
}

// TestLockHelperProcess is the subprocess entry point for
// TestAcquireBlocksAcrossProcesses. It is inert during a normal test run.
func TestLockHelperProcess(t *testing.T) {
	dir := os.Getenv(envHelperDir)
	if dir == "" {
		t.Skip("helper process entry point")
	}

	lock, err := Acquire(dir, "vol-1")
	if err != nil {
		t.Fatalf("helper Acquire: %v", err)
	}
	defer lock.Release()

	if err := os.WriteFile(os.Getenv(envHelperStamp), nil, 0o600); err != nil {
		t.Fatalf("helper write stamp: %v", err)
	}
}
