// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package logcleanup manages logs whose sandbox runtime has been confirmed gone.
package logcleanup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/identifiers"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/sandboxlog"
	"golang.org/x/sys/unix"
)

// Root is the directory used by CubeShim for regular sandbox init output.
const Root = sandboxlog.Dir

const markerName = ".delayed-cleanup"

// ErrLogDirNotFound means there was no sandbox log directory to retain.
var ErrLogDirNotFound = errors.New("sandbox log directory not found")

// Mark starts the retention period after Cubelet has confirmed runtime exit.
// Failure to mark is safe: an unmarked directory is never removed by Scan.
func Mark(root, id, namespace string) error {
	if err := validateID(id); err != nil {
		return err
	}
	if err := identifiers.Validate(namespace); err != nil {
		return fmt.Errorf("invalid sandbox namespace: %w", err)
	}
	dir := filepath.Join(root, id)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return ErrLogDirNotFound
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("sandbox log path is not a directory: %s", dir)
	}

	// Rename publishes a new timestamp atomically, including when a sandbox ID
	// was reused. Never follow an existing marker symlink.
	f, err := os.CreateTemp(dir, ".delayed-cleanup-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := io.WriteString(f, namespace); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, markerName))
}

// Namespace reads a marker without following a symlink. Old, empty or invalid
// markers cannot safely identify the shim namespace and must be retained.
func Namespace(root, id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	fd, err := unix.Open(filepath.Join(root, id, markerName), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), markerName)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("cleanup marker is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil {
		return "", err
	}
	ns := string(data)
	if len(data) > 256 || strings.TrimSpace(ns) != ns {
		return "", errors.New("invalid cleanup marker namespace")
	}
	if err := identifiers.Validate(ns); err != nil {
		return "", fmt.Errorf("invalid cleanup marker namespace: %w", err)
	}
	return ns, nil
}

// Guard serializes a final safety check with sandbox lifecycle operations.
// It calls remove only when it can prove the sandbox cannot use these logs.
type Guard func(ctx context.Context, id string, remove func() error) error

// Scan only removes marked directories older than delay. The safety check is
// repeated under the caller's lifecycle lock immediately before removal.
func Scan(ctx context.Context, root string, delay time.Duration, now time.Time, guard Guard) error {
	if delay <= 0 || guard == nil {
		return errors.New("invalid log retention configuration")
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		id := entry.Name()
		if !entry.IsDir() || validateID(id) != nil {
			continue
		}
		dir := filepath.Join(root, id)
		if !expired(dir, delay, now) {
			continue
		}
		if err := guard(ctx, id, func() error {
			// A concurrent teardown can refresh the marker while we wait.
			if !expired(dir, delay, now) {
				return nil
			}
			return os.RemoveAll(dir)
		}); err != nil {
			result = errors.Join(result, fmt.Errorf("clean sandbox logs %s: %w", id, err))
		}
	}
	return result
}

func validateID(id string) error {
	if id == "." {
		return errors.New("invalid sandbox id")
	}
	return pathutil.ValidateSafeID(id)
}

func expired(dir string, delay time.Duration, now time.Time) bool {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	marker, err := os.Lstat(filepath.Join(dir, markerName))
	return err == nil && marker.Mode().IsRegular() && !marker.ModTime().After(now.Add(-delay))
}
