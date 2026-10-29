// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package logcleanup manages logs whose sandbox runtime has been confirmed gone.
package logcleanup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
)

// Root is the directory used by CubeShim for regular sandbox init output.
const Root = "/data/cubelet/log"

const markerName = ".delayed-cleanup"

// Mark starts the retention period after Cubelet has confirmed runtime exit.
// Failure to mark is safe: an unmarked directory is never removed by Scan.
func Mark(root, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	dir := filepath.Join(root, id)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
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
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, markerName))
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
