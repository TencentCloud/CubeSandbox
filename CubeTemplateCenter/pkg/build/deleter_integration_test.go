package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/migrate"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestArtifactDeleterUnreportedDirectories(t *testing.T) {
	for _, dialect := range []string{"mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			if dialect == "mysql" {
				dsn := os.Getenv("CUBEMASTER_DAO_TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("requires an isolated MySQL test database")
				}
				driver = mysql.Open(dsn)
			} else {
				dsn := os.Getenv("CUBEMASTER_DAO_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("requires an isolated PostgreSQL test database")
				}
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = raw.Close() })
			if err := migrate.Run(context.Background(), raw, dialect, nil); err != nil {
				t.Fatal(err)
			}
			for _, marked := range []bool{false, true} {
				name := "unreported"
				if marked {
					name = "build-marked"
				}
				t.Run(name, func(t *testing.T) {
					storeRoot, workRoot := t.TempDir(), t.TempDir()
					t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)
					t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_DIR", workRoot)
					artifact := models.RootfsArtifact{ArtifactID: "audit-" + name, TemplateSpecFingerprint: "audit-" + name, Status: "CLEANUP_PENDING"}
					if err := db.Create(&artifact).Error; err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { db.Unscoped().Where("artifact_id = ?", artifact.ArtifactID).Delete(&models.RootfsArtifact{}) })
					for _, root := range []string{storeRoot, workRoot} {
						dir := filepath.Join(root, artifact.ArtifactID)
						if err := os.MkdirAll(dir, 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(dir, "partial"), []byte("data"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					deleter := NewArtifactDeleter(db, nil)
					if marked {
						release, err := image.MarkArtifactBuildInProgress(filepath.Join(storeRoot, artifact.ArtifactID))
						if err != nil {
							t.Fatal(err)
						}
						defer release()
						if err := deleter.Delete(t.Context(), artifact.ArtifactID); err == nil {
							t.Fatal("live build cleanup did not remain retryable")
						}
						var pending models.RootfsArtifact
						if err := db.First(&pending, "artifact_id = ?", artifact.ArtifactID).Error; err != nil || pending.Status != "CLEANUP_PENDING" || !strings.Contains(pending.LastError, "build in progress") {
							t.Fatalf("cleanup retry marker lost: %+v, %v", pending, err)
						}
						for _, root := range []string{storeRoot, workRoot} {
							if _, err := os.Stat(filepath.Join(root, artifact.ArtifactID, "partial")); err != nil {
								t.Fatalf("active build residue removed: %v", err)
							}
						}
						release()
					}
					if err := deleter.Delete(t.Context(), artifact.ArtifactID); err != nil {
						t.Fatal(err)
					}
					var gone models.RootfsArtifact
					if err := db.First(&gone, "artifact_id = ?", artifact.ArtifactID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
						t.Fatalf("artifact row remains: %v", err)
					}
					for _, root := range []string{storeRoot, workRoot} {
						if _, err := os.Stat(filepath.Join(root, artifact.ArtifactID)); !os.IsNotExist(err) {
							t.Fatalf("unreported directory remains: %s: %v", root, err)
						}
					}
					if err := deleter.Delete(t.Context(), artifact.ArtifactID); err != nil {
						t.Fatalf("repeated delete: %v", err)
					}
				})
			}
		})
	}
}
