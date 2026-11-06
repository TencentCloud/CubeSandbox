// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	shimbinary "github.com/containerd/containerd/v2/pkg/shim"
)

// delayedCleanupShimPath creates a stable alias for a particular shim binary.
// containerd records it in the bundle for crash cleanup and restart recovery.
func delayedCleanupShimPath(root, runtimePath, runtimeType string) (string, error) {
	if runtimePath == "" {
		name := shimbinary.BinaryName(runtimeType)
		if name == "" {
			return "", fmt.Errorf("invalid runtime name %q", runtimeType)
		}
		runtimePath = shimbinary.BinaryPath(runtimeType)
		if _, err := os.Stat(runtimePath); err != nil {
			var lookupErr error
			runtimePath, lookupErr = exec.LookPath(name)
			if lookupErr != nil {
				if !errors.Is(lookupErr, exec.ErrNotFound) {
					return "", lookupErr
				}
				self, err := os.Executable()
				if err != nil {
					return "", err
				}
				runtimePath = filepath.Join(filepath.Dir(self), name)
			}
		}
		var err error
		runtimePath, err = filepath.Abs(runtimePath)
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(runtimePath) {
		return "", fmt.Errorf("shim path must be absolute: %q", runtimePath)
	}
	if _, err := os.Stat(runtimePath); err != nil {
		return "", fmt.Errorf("shim path %q: %w", runtimePath, err)
	}
	hash := sha256.Sum256([]byte(runtimePath))
	dir := filepath.Join(root, "delayed-log-shims")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("invalid shim launcher directory %q", dir)
	}
	launcher := filepath.Join(dir, fmt.Sprintf("containerd-shim-cube-rs-retained-%x", hash))
	if err := os.Symlink(runtimePath, launcher); err != nil && !os.IsExist(err) {
		return "", err
	}
	// Symlink creation is atomic; verify an existing alias after a race.
	target, err := os.Readlink(launcher)
	if err != nil || target != runtimePath {
		return "", fmt.Errorf("invalid shim launcher %q", launcher)
	}
	return launcher, nil
}
