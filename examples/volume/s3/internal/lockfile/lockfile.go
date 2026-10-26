// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package lockfile serialises concurrent attach/detach for the same volume.
//
// The plugin runs as one short-lived process per operation, so the lock MUST be
// an advisory file lock visible across processes. An in-process sync.Mutex would
// not serialise anything here.
package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock is a held flock on a per-volume lock file.
type Lock struct {
	file *os.File
}

// Acquire blocks until the exclusive lock on <dir>/<volumeID>.lock is held.
func Acquire(dir, volumeID string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create lock dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, volumeID+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %q: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock %q: %w", path, err)
	}
	return &Lock{file: f}, nil
}

// Release unlocks and closes the lock file. Safe to call more than once.
func (l *Lock) Release() {
	if l == nil || l.file == nil {
		return
	}
	// Closing the descriptor drops the lock too; unlock explicitly so the
	// release point is obvious and does not depend on close ordering.
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}
