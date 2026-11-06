// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
)

const publicationMarkerName = ".cube-artifact-publication-in-progress"
const localArtifactGrace = 2 * time.Hour

// The publication marker outlives BuildExt4's marker and covers upload/report.
func holdArtifactPublicationMarker(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	marker := filepath.Join(dir, publicationMarkerName)
	if err := os.WriteFile(marker, []byte(fmt.Sprintf("pid=%d\n", os.Getpid())), 0o600); err != nil {
		return nil, err
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				_ = os.Chtimes(marker, now, now)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop); <-done; _ = os.Remove(marker) })
	}, nil
}

// SweepLocal removes only unreferenced aged directories. It never deletes
// shared rows, object-store data, or Cubelet placements.
func (d *ArtifactDeleter) SweepLocal(ctx context.Context, executor *Executor) error {
	if d.db == nil || executor == nil {
		return nil
	}
	roots := []string{image.ArtifactWorkRootDir(), image.ArtifactStoreRootDir()}
	if strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR")) == "" {
		roots = append(roots, image.ArtifactFallbackStoreRootDir())
	}
	return sweepLocalArtifactDirs(ctx, roots, executor.WithIdle, d.localArtifactUnreferenced)
}

func (d *ArtifactDeleter) localArtifactUnreferenced(ctx context.Context, id string) (bool, error) {
	checks := []struct {
		table, predicate string
		args             []any
	}{
		{constants.RootfsArtifactTableName, "artifact_id = ?", []any{id}},
		{constants.TemplateDefinitionTableName, "rootfs_artifact_id = ?", []any{id}},
		{constants.SnapshotTableName, "rootfs_artifact_id = ?", []any{id}},
		{constants.TemplateReplicaTableName, "artifact_id = ?", []any{id}},
		{constants.ArtifactNodePlacementTableName, "artifact_id = ?", []any{id}},
		{constants.TemplateImageJobTableName, "(artifact_id = ? AND status IN ?) OR (artifact_id = '' AND status IN ?)", []any{id,
			[]string{templatecenter.JobStatusPending, templatecenter.JobStatusRunning, templatecenter.JobStatusBuilt},
			[]string{templatecenter.JobStatusPending, templatecenter.JobStatusRunning}}},
	}
	for _, check := range checks {
		var count int64
		if err := d.db.WithContext(ctx).Unscoped().Table(check.table).Where(check.predicate, check.args...).Count(&count).Error; err != nil {
			return false, err
		}
		if count > 0 {
			return false, nil
		}
	}
	return true, nil
}

func sweepLocalArtifactDirs(ctx context.Context, roots []string, withIdle func(func() error) error, unreferenced func(context.Context, string) (bool, error)) error {
	var errs []error
	for _, root := range roots {
		root, err := filepath.Abs(root)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		resolved, err := filepath.EvalSymlinks(root)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || resolved != root {
			errs = append(errs, fmt.Errorf("local artifact root %s is not a regular resolved directory: %v", root, err))
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, entry := range entries {
			id := entry.Name()
			if !entry.IsDir() || !strings.HasPrefix(id, "rfs-") || !image.ValidArtifactID(id) {
				continue
			}
			dir := filepath.Join(root, id)
			info, err := entry.Info()
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if time.Since(info.ModTime()) < localArtifactGrace {
				continue
			}
			if err := withIdle(func() error {
				checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				ok, err := unreferenced(checkCtx, id)
				if err != nil || !ok {
					return err
				}
				resolvedRoot, err := filepath.EvalSymlinks(root)
				if err != nil || resolvedRoot != root {
					return fmt.Errorf("local artifact root changed: %s", root)
				}
				info, err := os.Lstat(dir)
				if os.IsNotExist(err) {
					return nil
				}
				if err != nil {
					return err
				}
				if !info.IsDir() || time.Since(info.ModTime()) < localArtifactGrace {
					return nil
				}
				for _, name := range []string{".cube-artifact-build-in-progress", publicationMarkerName} {
					marker, err := os.Lstat(filepath.Join(dir, name))
					if os.IsNotExist(err) {
						continue
					}
					if err != nil {
						return err
					}
					if !marker.Mode().IsRegular() || time.Since(marker.ModTime()) < localArtifactGrace {
						return nil
					}
				}
				if err := checkCtx.Err(); err != nil {
					return err
				}
				return os.RemoveAll(dir)
			}); err != nil {
				errs = append(errs, fmt.Errorf("local artifact %s: %w", id, err))
			}
		}
	}
	return errors.Join(errs...)
}
