package templatecenter

import (
	"context"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/migrate"
	"gorm.io/gorm"
)

func TestRootfsArtifactDowngradePreservesLegacyMetadata(t *testing.T) {
	for _, dialect := range []string{"mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var db *gorm.DB
			if dialect == "mysql" {
				env := newMySQLDockerEnv(t)
				t.Cleanup(env.teardown)
				db = openMigratedMySQLGORM(t, env)
			} else {
				env := newPGDockerEnv(t)
				t.Cleanup(env.teardown)
				db = openMigratedPostgresGORM(t, env)
			}
			raw, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			previous := store.db
			store.db = db
			t.Cleanup(func() { store.db = previous })
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			artifacts := []models.RootfsArtifact{
				{ArtifactID: "downgrade-created", TemplateSpecFingerprint: "downgrade-created", RootfsPath: "/created.ext4", RootfsSHA256: "created-sha", RootfsSizeBytes: 4096},
				{ArtifactID: "downgrade-updated", TemplateSpecFingerprint: "downgrade-updated", RootfsPath: "/before.ext4", RootfsSHA256: "before-sha", RootfsSizeBytes: 4096, Status: ArtifactStatusBuilding},
				{ArtifactID: "downgrade-deleted", TemplateSpecFingerprint: "downgrade-deleted", RootfsPath: "/deleted.ext4", RootfsSHA256: "deleted-sha", RootfsSizeBytes: 8192},
			}
			// Leave an externally supplied test database at the current schema.
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cleanupCancel()
				if err := migrate.Run(cleanupCtx, raw, dialect, nil); err != nil {
					t.Errorf("restore current schema: %v", err)
				}
				for _, artifact := range artifacts {
					if err := db.WithContext(cleanupCtx).Unscoped().Where("artifact_id = ?", artifact.ArtifactID).Delete(&models.RootfsArtifact{}).Error; err != nil {
						t.Errorf("clean up artifact: %v", err)
					}
				}
				if err := db.WithContext(cleanupCtx).Unscoped().Where("job_id = ?", "downgrade-job").Delete(&models.TemplateImageJob{}).Error; err != nil {
					t.Errorf("clean up job: %v", err)
				}
			})
			for i := range artifacts {
				if err := db.Create(&artifacts[i]).Error; err != nil {
					t.Fatal(err)
				}
			}
			updated := &artifacts[1]
			updated.RootfsPath, updated.RootfsSHA256, updated.RootfsSizeBytes = "/updated.ext4", "updated-sha", 16384
			ok, err := updateRootfsArtifactIfStatus(ctx, updated.ArtifactID, ArtifactStatusBuilding, map[string]any{
				"rootfs_format": updated.RootfsFormat,
				"rootfs_path":   updated.RootfsPath, "rootfs_sha256": updated.RootfsSHA256, "rootfs_size_bytes": updated.RootfsSizeBytes, "status": ArtifactStatusReady,
			})
			if err != nil || !ok {
				t.Fatalf("update artifact: matched=%t err=%v", ok, err)
			}
			if err := db.Delete(&artifacts[2]).Error; err != nil {
				t.Fatal(err)
			}
			job := models.TemplateImageJob{JobID: "downgrade-job", TemplateID: "downgrade-template", AttemptNo: 1, Phase: JobPhaseBuildingRootfs, ResumePhase: JobPhaseBuildingRootfs}
			if err := db.Create(&job).Error; err != nil {
				t.Fatal(err)
			}

			if err := migrate.DownTo(ctx, raw, dialect, nil, 20260916120000); err != nil {
				t.Fatal(err)
			}
			for _, column := range []string{"rootfs_format", "rootfs_path", "rootfs_sha256", "rootfs_size_bytes"} {
				if db.Migrator().HasColumn(&models.RootfsArtifact{}, column) {
					t.Fatalf("downgrade retained column %s", column)
				}
			}
			for i, artifact := range artifacts {
				// Read only the old columns, without the new model's read hook.
				var legacy struct {
					Path      string
					SHA256    string
					SizeBytes int64
					DeletedAt *time.Time
				}
				result := db.Table(constants.RootfsArtifactTableName).
					Select("ext4_path AS path, ext4_sha256 AS sha256, ext4_size_bytes AS size_bytes, deleted_at").
					Where("artifact_id = ?", artifact.ArtifactID).Scan(&legacy)
				if result.Error != nil || result.RowsAffected != 1 {
					t.Fatalf("read legacy artifact %s: rows=%d err=%v", artifact.ArtifactID, result.RowsAffected, result.Error)
				}
				if legacy.Path != artifact.RootfsPath || legacy.SHA256 != artifact.RootfsSHA256 || legacy.SizeBytes != artifact.RootfsSizeBytes {
					t.Fatalf("legacy artifact %s: %+v", artifact.ArtifactID, legacy)
				}
				if (legacy.DeletedAt != nil) != (i == 2) {
					t.Fatalf("legacy artifact %s lost its deletion state", artifact.ArtifactID)
				}
			}
			var legacyJob struct{ Phase, ResumePhase string }
			if err := db.Table(constants.TemplateImageJobTableName).Select("phase, resume_phase").Where("job_id = ?", job.JobID).Scan(&legacyJob).Error; err != nil {
				t.Fatal(err)
			}
			if legacyJob.Phase != "BUILDING_EXT4" || legacyJob.ResumePhase != "BUILDING_EXT4" {
				t.Fatalf("legacy job phases: %+v", legacyJob)
			}
		})
	}
}
