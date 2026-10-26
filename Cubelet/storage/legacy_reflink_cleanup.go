// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

func isXFSCleanupBackend(backend string) bool {
	normalized, err := cow.NormalizeBackend(backend)
	if err != nil {
		return false
	}
	return normalized == cow.BackendXFS
}

// removeLegacyXfsCowObjectIfGone deletes a cubecow volume directory or
// snapshot file from the pre-refactor <work>/cubecow-reflink pool when the
// same name is already gone from <work>/xfs/objects. Template / snapshot
// cleanup used to miss those leftovers because cubecow's reflink root_dir
// now points at xfs/objects and treats old objects as NotFound.
func removeLegacyXfsCowObjectIfGone(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if err := pathutil.ValidateSafeID(name); err != nil {
		return nil
	}
	currentRoot, legacyRoot, ok := reflinkCleanupRoots()
	if !ok {
		return nil
	}
	if _, found, err := locateReflinkObject(currentRoot, name); err != nil {
		return err
	} else if found {
		return nil
	}
	path, found, err := locateReflinkObject(legacyRoot, name)
	if err != nil || !found {
		return err
	}
	if err := removeReflinkPath(legacyRoot, path); err != nil {
		return err
	}
	log.G(ctx).Infof("removed leftover xfs cubecow object %s from legacy reflink path %s", name, path)
	return nil
}

func reflinkCleanupRoots() (currentRoot, legacyRoot string, ok bool) {
	if localStorage == nil || localStorage.config == nil {
		return "", "", false
	}
	dataPath := strings.TrimSpace(localStorage.config.DataPath)
	if dataPath == "" {
		return "", "", false
	}
	work := stripStoragePluginDataDir(filepath.Clean(dataPath))
	if work == "" || work == "." {
		return "", "", false
	}
	return filepath.Join(work, cow.BackendXFS, SnapshotObjectsDir),
		filepath.Join(work, legacyReflinkDirName),
		true
}

func locateReflinkObject(root, name string) (string, bool, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", false, nil
	}
	volumes := filepath.Join(root, reflinkVolumesDirName)
	st, err := os.Lstat(volumes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", false, nil
	}

	volDir := filepath.Join(volumes, name)
	resolved, err := pathutil.ValidatePathUnderBase(volumes, volDir)
	if err != nil {
		return "", false, nil
	}
	if info, err := os.Lstat(resolved); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return resolved, true, nil
	}

	entries, err := os.ReadDir(volumes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == name {
			continue
		}
		if err := pathutil.ValidateSafeID(entry.Name()); err != nil {
			continue
		}
		candidate := filepath.Join(volumes, entry.Name(), name)
		resolved, err := pathutil.ValidatePathUnderBase(volumes, candidate)
		if err != nil {
			continue
		}
		info, err := os.Lstat(resolved)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		return resolved, true, nil
	}
	return "", false, nil
}

func removeReflinkPath(root, target string) error {
	volumes := filepath.Join(root, reflinkVolumesDirName)
	resolved, err := pathutil.ValidatePathUnderBase(volumes, target)
	if err != nil {
		return fmt.Errorf("refusing unsafe legacy reflink path %q: %w", target, err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove symlink leftover %q", resolved)
	}
	if info.IsDir() {
		return os.RemoveAll(resolved)
	}
	return os.Remove(resolved)
}
