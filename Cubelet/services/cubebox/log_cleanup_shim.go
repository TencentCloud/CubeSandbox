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
	"strings"

	shimbinary "github.com/containerd/containerd/v2/pkg/shim"
)

// delayedCleanupShimPath creates a stable launcher for a particular shim
// binary. containerd invokes that path for both start and crash cleanup, and
// records it in the bundle for recovery after a Cubelet restart.
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
	// Single-quote the complete path, including embedded single quotes.
	quoted := "'" + strings.ReplaceAll(runtimePath, "'", "'\\''") + "'"
	content := "#!/bin/sh\nexport " + delayedShimLogCleanupEnv + "=1\nexport " + managedShimLogCleanupEnv + "=1\nexec " + quoted + " \"$@\"\n"
	hash := sha256.Sum256([]byte(runtimePath))
	dir := filepath.Join(root, "delayed-log-shims")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("invalid shim launcher directory %q", dir)
	}
	launcher := filepath.Join(dir, fmt.Sprintf("%x", hash))
	if info, err := os.Lstat(launcher); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0700 {
			return "", fmt.Errorf("invalid shim launcher %q", launcher)
		}
		data, err := os.ReadFile(launcher)
		if err != nil || string(data) != content {
			return "", fmt.Errorf("invalid shim launcher %q", launcher)
		}
		return launcher, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".shim-")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0700); err != nil {
		f.Close()
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Link(f.Name(), launcher); err != nil && !os.IsExist(err) {
		return "", err
	}
	// Validate an existing file as well when concurrent creates race.
	info, statErr := os.Lstat(launcher)
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0700 {
		return "", fmt.Errorf("invalid shim launcher %q", launcher)
	}
	data, err := os.ReadFile(launcher)
	if err != nil || string(data) != content {
		return "", fmt.Errorf("invalid shim launcher %q", launcher)
	}
	return launcher, nil
}
