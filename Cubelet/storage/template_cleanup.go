// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
)

func CleanupTemplateLocalData(ctx context.Context, templateID, snapshotPath string) error {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return errors.New("templateID is required")
	}
	if err := pathutil.ValidateSafeID(templateID); err != nil {
		return fmt.Errorf("invalid templateID: %w", err)
	}
	if snapshotPath != "" {
		if err := pathutil.ValidateNoTraversal(snapshotPath); err != nil {
			return fmt.Errorf("invalid snapshotPath: %w", err)
		}
	}
	if localStorage == nil || localStorage.config == nil {
		return nil
	}

	var cleanupErr error
	remove := func(target string) {
		target = strings.TrimSpace(target)
		if target == "" {
			return
		}
		if err := pathutil.ValidateNoTraversal(target); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("refusing to remove unsafe path %q: %w", target, err))
			return
		}
		// NOCC:Path Traversal()
		if err := os.RemoveAll(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}

	if snapshotPath != "" {
		remove(snapshotPath)
		// Catalog SnapshotPath is .../cubebox/<id>/<specDir>. Removing only the
		// leaf leaves an empty <id> parent behind (observed as empty dirs under
		// DefaultSnapshotDir/cubebox). When the leaf's parent is the snap/template
		// id directory, remove that too. Skip when snapshotPath already points at
		// the id directory (catalog-miss / ensureSnapshotCleanupPath path).
		cleanSnap := filepath.Clean(snapshotPath)
		if filepath.Base(cleanSnap) != templateID {
			parent := filepath.Dir(cleanSnap)
			if filepath.Base(parent) == templateID {
				remove(parent)
			}
		}
	}

	localStorage.tmpPoolFormat.Delete(templateID)
	localStorage.poolFormat.Range(func(key, value any) bool {
		keyStr, ok := key.(string)
		if !ok {
			return true
		}
		if keyStr != templateID && !strings.Contains(keyStr, "/"+templateID+"/") {
			return true
		}
		if pool, ok := value.(Pool); ok {
			pool.Close()
		}
		localStorage.poolFormat.Delete(key)
		return true
	})

	remove(filepath.Join(localStorage.cubeboxTemplateFormatPath, templateID))

	templatesRoot := filepath.Join(localStorage.config.RootPath, "base-block-storage", "templates")
	entries, err := os.ReadDir(templatesRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		remove(filepath.Join(templatesRoot, entry.Name(), templateID))
	}

	if cleanupErr != nil {
		return cleanupErr
	}
	log.G(ctx).Infof("cleanup template local data success, templateID=%s snapshotPath=%s", templateID, snapshotPath)
	return nil
}
