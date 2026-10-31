// SPDX-License-Identifier: Apache-2.0

package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
)

const localOrphanGrace = 10 * time.Minute

// CleanupLocalOrphans must run on every TC replica: deleting the shared row and
// S3 object on one replica cannot remove another replica's private build cache.
// Artifact IDs are unique per build, never reused. Retain all rows (including
// deletion tombstones), live owners, recent directories and active build markers;
// database uncertainty must never turn into permission to remove local bytes.
func (d *ArtifactDeleter) CleanupLocalOrphans(ctx context.Context) error {
	if d.db == nil {
		return errors.New("local artifact cleanup has no db handle")
	}
	roots := localArtifactRoots()
	if d.s3Client == nil {
		// Without object storage, artifact-store paths may be a shared RWX
		// volume. Sweep only replica-local scratch directories in this mode.
		roots = []string{image.ArtifactWorkRootDir()}
	}
	return cleanupLocalOrphans(ctx, roots, time.Now(), d.localArtifactReferenced)
}

func artifactBuildInAnyRoot(roots []string, id string) bool {
	// BuildExt4 publishes its marker in the store, before writing the work tree.
	// A work directory without its own marker is therefore not necessarily idle.
	markerRoots := append([]string{image.ArtifactStoreRootDir(), image.ArtifactFallbackStoreRootDir()}, roots...)
	for _, root := range markerRoots {
		if active, _ := image.ArtifactBuildInProgress(filepath.Join(root, id)); active {
			return true
		}
	}
	return false
}

func localArtifactRoots() []string {
	roots := []string{image.ArtifactWorkRootDir(), image.ArtifactStoreRootDir()}
	if strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR")) == "" {
		roots = append(roots, image.ArtifactFallbackStoreRootDir())
	}
	return roots
}

func (d *ArtifactDeleter) localArtifactReferenced(ctx context.Context, id string) (bool, error) {
	checks := []struct{ table, predicate string }{
		{artifactTableName, "artifact_id = ?"},
		{constants.TemplateImageJobTableName, "artifact_id = ? AND deleted_at IS NULL AND status IN ('PENDING','RUNNING','BUILT')"},
		{constants.TemplateDefinitionTableName, "rootfs_artifact_id = ? AND deleted_at IS NULL"},
		{constants.TemplateReplicaTableName, "artifact_id = ? AND deleted_at IS NULL"},
		{constants.SnapshotTableName, "rootfs_artifact_id = ? AND deleted_at IS NULL"},
	}
	for _, check := range checks {
		var rows []struct{ Present int }
		if err := d.db.WithContext(ctx).Unscoped().Table(check.table).Select("1 AS present").Where(check.predicate, id).Limit(1).Scan(&rows).Error; err != nil {
			return false, err
		}
		if len(rows) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func cleanupLocalOrphans(ctx context.Context, roots []string, now time.Time, referenced func(context.Context, string) (bool, error)) error {
	seen := map[string]bool{}
	var errs []error
	removed := 0
	for _, root := range roots {
		root, err := filepath.Abs(root)
		if err != nil {
			return err
		}
		if seen[root] {
			continue
		}
		seen[root] = true
		entries, err := os.ReadDir(root)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(errs, err)...)
			}
			// Only direct, non-symlink artifact directories. Never sweep the
			// store itself, layer caches, or a path supplied by a database row.
			id := entry.Name()
			if !entry.IsDir() || !strings.HasPrefix(id, "rfs-") || !image.ValidArtifactID(id) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				errs = append(errs, err)
				continue
			}
			dir := filepath.Join(root, id)
			if now.Sub(info.ModTime()) < localOrphanGrace {
				continue
			}
			if artifactBuildInAnyRoot(roots, id) {
				continue
			}
			live, err := referenced(ctx, id)
			if err != nil {
				return errors.Join(append(errs, fmt.Errorf("check local artifact %s: %w", id, err))...)
			}
			if live {
				continue
			}
			// Recheck the identity/marker after the DB round trip. An active
			// build without a persisted artifact row still owns its marker/job.
			after, err := os.Lstat(dir)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if !after.IsDir() || !os.SameFile(info, after) || now.Sub(after.ModTime()) < localOrphanGrace {
				continue
			}
			if artifactBuildInAnyRoot(roots, id) {
				continue
			}
			if err := os.RemoveAll(dir); err != nil {
				errs = append(errs, err)
				continue
			}
			log.G(ctx).WithFields(map[string]any{"artifact_id": id, "path": dir}).Info("removed unreferenced local artifact cache")
			removed++
			if removed >= 200 {
				return errors.Join(errs...)
			}
		}
	}
	return errors.Join(errs...)
}
