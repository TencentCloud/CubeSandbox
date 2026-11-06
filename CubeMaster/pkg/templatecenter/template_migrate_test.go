package templatecenter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newMigrateSharedStoreTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.TemplateDefinition{}, &models.RootfsArtifact{}))
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })
	return db
}

// stubMigrateS3Unavailable forces the TC-local upload branch: S3 is not
// configured, so migration falls back to uploading into TC's own store.
func stubMigrateS3Unavailable(t *testing.T) {
	t.Helper()
	oldUpload := uploadArtifactFileToS3
	uploadArtifactFileToS3 = func(ctx context.Context, artifactID, filePath string) error {
		return errS3PresignNotConfigured
	}
	t.Cleanup(func() { uploadArtifactFileToS3 = oldUpload })
}

func stubMigrateTCUpload(t *testing.T, fn func(ctx context.Context, artifactID, filePath string) (*templateCenterUploadResult, error)) {
	t.Helper()
	oldUpload := uploadArtifactFileToTC
	uploadArtifactFileToTC = func(ctx context.Context, artifactID, filePath string) (*templateCenterUploadResult, error) {
		res, err := fn(ctx, artifactID, filePath)
		if res != nil && res.OwnerTCDownloadBase == "" {
			res.OwnerTCDownloadBase = "http://target-tc.example:8090"
		}
		return res, err
	}
	t.Cleanup(func() { uploadArtifactFileToTC = oldUpload })
}

func stubMigrateS3Metadata(t *testing.T, fn func(string) (string, string, string, error)) {
	t.Helper()
	oldMetadata := migrationArtifactStoreMetadata
	migrationArtifactStoreMetadata = fn
	t.Cleanup(func() { migrationArtifactStoreMetadata = oldMetadata })
}

func stubMigratePresign(t *testing.T, fn func(context.Context, *models.RootfsArtifact) (string, error)) {
	t.Helper()
	oldPresign := presignArtifactGetURL
	presignArtifactGetURL = fn
	t.Cleanup(func() { presignArtifactGetURL = oldPresign })
}

func seedMigratingTemplate(t *testing.T, db *gorm.DB, templateID, artifactID, ext4Path string) {
	t.Helper()
	require.NoError(t, db.Create(&models.TemplateDefinition{
		TemplateID:       templateID,
		Status:           StatusReady,
		RootfsArtifactID: artifactID,
	}).Error)
	require.NoError(t, db.Create(&models.RootfsArtifact{
		ArtifactID: artifactID,
		Status:     ArtifactStatusReady,
		Ext4Path:   ext4Path,
	}).Error)
}

func TestMigrateTemplateRejectsConcurrentGeneration(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	path := filepath.Join(t.TempDir(), "source.ext4")
	require.NoError(t, os.WriteFile(path, []byte("old-generation"), 0o600))
	seedMigratingTemplate(t, db, "tpl-race", "rfs-race", path)
	oldUpload := uploadArtifactFileToS3
	t.Cleanup(func() { uploadArtifactFileToS3 = oldUpload })
	uploadArtifactFileToS3 = func(_ context.Context, uploadID, _ string) error {
		require.True(t, strings.HasPrefix(uploadID, "rfs-race-"))
		require.NotEqual(t, "rfs-race", uploadID)
		return db.Model(&models.RootfsArtifact{}).Where("artifact_id = ?", "rfs-race").Updates(map[string]any{"generation": 2, "object_key": "new-generation.ext4", "storage_ref": "new-store"}).Error
	}
	stubMigrateS3Metadata(t, func(id string) (string, string, string, error) { return "s3", id + ".ext4", "s3://bucket/prefix", nil })
	stubMigratePresign(t, func(context.Context, *models.RootfsArtifact) (string, error) { return "https://signed.example", nil })
	_, err := MigrateTemplateArtifactToTC(t.Context(), "tpl-race")
	require.Error(t, err)
	var current models.RootfsArtifact
	require.NoError(t, db.Where("artifact_id = ?", "rfs-race").First(&current).Error)
	require.EqualValues(t, 2, current.Generation)
	require.Equal(t, "new-generation.ext4", current.ObjectKey)
	require.FileExists(t, path)
}

func TestMigrateTemplateArtifactToS3PersistsStableLocator(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	ext4Path := filepath.Join(t.TempDir(), "artifact.ext4")
	require.NoError(t, os.WriteFile(ext4Path, []byte("ext4-bytes"), 0o644))
	seedMigratingTemplate(t, db, "tpl-s3-locator", "rfs-s3-locator", ext4Path)

	oldUpload := uploadArtifactFileToS3
	uploadArtifactFileToS3 = func(context.Context, string, string) error { return nil }
	t.Cleanup(func() { uploadArtifactFileToS3 = oldUpload })
	stubMigrateS3Metadata(t, func(string) (string, string, string, error) {
		return "s3", "artifacts/rfs-s3-locator.ext4", "s3://bucket/artifacts?endpoint=http%3A%2F%2Fminio%3A9000", nil
	})
	stubMigratePresign(t, func(_ context.Context, artifact *models.RootfsArtifact) (string, error) {
		require.Equal(t, "s3", artifact.StorageBackend)
		require.Equal(t, "artifacts/rfs-s3-locator.ext4", artifact.ObjectKey)
		require.Equal(t, "s3://bucket/artifacts?endpoint=http%3A%2F%2Fminio%3A9000", artifact.StorageRef)
		return "http://signed.invalid/artifact", nil
	})

	result, err := MigrateTemplateArtifactToTC(t.Context(), "tpl-s3-locator")
	require.NoError(t, err)
	require.True(t, result.Migrated)

	var artifact models.RootfsArtifact
	require.NoError(t, db.Where("artifact_id = ?", "rfs-s3-locator").First(&artifact).Error)
	require.Equal(t, "s3", artifact.StorageBackend)
	require.Equal(t, "artifacts/rfs-s3-locator.ext4", artifact.ObjectKey)
	require.Equal(t, "s3://bucket/artifacts?endpoint=http%3A%2F%2Fminio%3A9000", artifact.StorageRef)
}

func TestMigrateTemplateArtifactToS3RejectsMismatchedOwnership(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	seedMigratingTemplate(t, db, "tpl-s3-mismatch", "rfs-s3-mismatch", "/missing/rfs-s3-mismatch.ext4")
	require.NoError(t, db.Model(&models.RootfsArtifact{}).
		Where("artifact_id = ?", "rfs-s3-mismatch").
		Updates(map[string]any{
			"artifact_url":    "http://legacy-signed.invalid/artifact",
			"storage_backend": "s3",
			"object_key":      "artifacts/rfs-s3-mismatch.ext4",
			"storage_ref":     "s3://foreign-bucket/artifacts?endpoint=http%3A%2F%2Fforeign%3A9000",
		}).Error)
	t.Setenv("CUBE_S3_ENDPOINT", "http://current:9000")
	t.Setenv("CUBE_S3_BUCKET", "current-bucket")
	t.Setenv("CUBE_S3_ACCESS_KEY_ID", "access")
	t.Setenv("CUBE_S3_SECRET_ACCESS_KEY", "secret")
	t.Setenv("CUBE_S3_ARTIFACT_PREFIX", "artifacts")

	oldStat := statArtifactObjectInS3
	statCalled := false
	statArtifactObjectInS3 = func(context.Context, *models.RootfsArtifact) (bool, error) {
		statCalled = true
		return true, nil
	}
	t.Cleanup(func() { statArtifactObjectInS3 = oldStat })

	_, err := MigrateTemplateArtifactToTC(t.Context(), "tpl-s3-mismatch")
	require.ErrorContains(t, err, "foreign-bucket")
	require.False(t, statCalled, "mismatched ownership must fail before probing the current bucket")
}

func TestMigrateTemplateArtifactToS3RepairsLegacyLocator(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	seedMigratingTemplate(t, db, "tpl-s3-repair", "rfs-s3-repair", "/missing/rfs-s3-repair.ext4")
	require.NoError(t, db.Model(&models.RootfsArtifact{}).
		Where("artifact_id = ?", "rfs-s3-repair").
		Updates(map[string]any{
			"artifact_url":    "http://legacy-signed.invalid/artifact",
			"storage_backend": "s3",
			"object_key":      "artifacts/rfs-s3-repair.ext4",
			"storage_ref":     "",
		}).Error)

	oldStat := statArtifactObjectInS3
	statArtifactObjectInS3 = func(context.Context, *models.RootfsArtifact) (bool, error) { return true, nil }
	t.Cleanup(func() { statArtifactObjectInS3 = oldStat })
	stubMigrateS3Metadata(t, func(string) (string, string, string, error) {
		return "s3", "artifacts/rfs-s3-repair.ext4", "s3://bucket/artifacts?endpoint=http%3A%2F%2Fminio%3A9000", nil
	})

	result, err := MigrateTemplateArtifactToTC(t.Context(), "tpl-s3-repair")
	require.NoError(t, err)
	require.False(t, result.Migrated)

	var artifact models.RootfsArtifact
	require.NoError(t, db.Where("artifact_id = ?", "rfs-s3-repair").First(&artifact).Error)
	require.Equal(t, "s3", artifact.StorageBackend)
	require.Equal(t, "artifacts/rfs-s3-repair.ext4", artifact.ObjectKey)
	require.Equal(t, "s3://bucket/artifacts?endpoint=http%3A%2F%2Fminio%3A9000", artifact.StorageRef)
}

// Regression guard for the shared-store data loss: when Master and TC share
// one artifact volume, TC "ingests" the very file it was handed and reports
// the same path back. Cleanup must NOT remove it, or the migrated row points
// at a deleted file.
func TestMigrateTemplateArtifactToTCSamePathKeepsLocalFile(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	sharedRoot := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", sharedRoot)

	artifactID := "rfs-shared-same-path"
	storeDir := filepath.Join(sharedRoot, artifactID)
	require.NoError(t, os.MkdirAll(storeDir, 0o755))
	ext4Path := filepath.Join(storeDir, artifactID+".ext4")
	require.NoError(t, os.WriteFile(ext4Path, []byte("ext4-bytes"), 0o644))

	templateID := "tpl-shared-same-path"
	seedMigratingTemplate(t, db, templateID, artifactID, ext4Path)
	stubMigrateS3Metadata(t, func(string) (string, string, string, error) {
		return "fs", "rfs-shared-same-path.ext4", "", nil
	})
	oldS3Upload := uploadArtifactFileToS3
	uploadArtifactFileToS3 = func(context.Context, string, string) error {
		t.Fatal("fs migration must not write through the S3 upload path")
		return nil
	}
	t.Cleanup(func() { uploadArtifactFileToS3 = oldS3Upload })
	stubMigrateTCUpload(t, func(ctx context.Context, id, filePath string) (*templateCenterUploadResult, error) {
		return &templateCenterUploadResult{Ext4Path: filePath}, nil
	})

	result, err := MigrateTemplateArtifactToTC(context.Background(), templateID)
	require.NoError(t, err)
	require.True(t, result.Migrated)
	require.False(t, result.Cleaned, "same-path migration must not delete the only copy")
	if _, statErr := os.Stat(ext4Path); statErr != nil {
		t.Fatalf("migrated artifact file must survive same-path migration: %v", statErr)
	}
	var artifact models.RootfsArtifact
	require.NoError(t, db.Where("artifact_id = ?", artifactID).First(&artifact).Error)
	require.Empty(t, artifact.StorageBackend)
	require.Empty(t, artifact.ObjectKey)
	require.Equal(t, "file://"+ext4Path, artifact.StorageRef)
	require.Equal(t, "http://target-tc.example:8090", artifact.OwnerTCDownloadBase)
}

// Even in the shared-store model the source path can differ from the target
// path string (for example, a hard-linked staging file copied into the shared
// artifact layout). SameFile must still prevent deleting the only bytes.
func TestMigrateTemplateArtifactToTCSameInodeDifferentPathKeepsLocalFile(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	sharedRoot := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", sharedRoot)

	artifactID := "rfs-shared-inode"
	srcDir := filepath.Join(t.TempDir(), artifactID)
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	srcPath := filepath.Join(srcDir, artifactID+".ext4")
	require.NoError(t, os.WriteFile(srcPath, []byte("ext4-bytes"), 0o644))

	dstDir := filepath.Join(sharedRoot, artifactID)
	require.NoError(t, os.MkdirAll(dstDir, 0o755))
	dstPath := filepath.Join(dstDir, artifactID+".ext4")
	require.NoError(t, os.Link(srcPath, dstPath))

	templateID := "tpl-shared-inode"
	seedMigratingTemplate(t, db, templateID, artifactID, srcPath)
	stubMigrateS3Unavailable(t)
	stubMigrateTCUpload(t, func(ctx context.Context, id, filePath string) (*templateCenterUploadResult, error) {
		return &templateCenterUploadResult{Ext4Path: dstPath}, nil
	})

	result, err := MigrateTemplateArtifactToTC(context.Background(), templateID)
	require.NoError(t, err)
	require.True(t, result.Migrated)
	require.False(t, result.Cleaned, "same-inode migration must not delete the source")
	if _, statErr := os.Stat(srcPath); statErr != nil {
		t.Fatalf("source must survive when TC destination is the same inode: %v", statErr)
	}
}

// A genuinely different destination must still clean up the local source after
// a successful migration.
func TestMigrateTemplateArtifactToTCDifferentPathCleansUp(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	sharedRoot := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", sharedRoot)

	artifactID := "rfs-split-store"
	srcDir := filepath.Join(t.TempDir(), artifactID)
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	srcPath := filepath.Join(srcDir, artifactID+".ext4")
	require.NoError(t, os.WriteFile(srcPath, []byte("ext4-bytes"), 0o644))

	dstDir := filepath.Join(sharedRoot, artifactID)
	require.NoError(t, os.MkdirAll(dstDir, 0o755))
	dstPath := filepath.Join(dstDir, artifactID+".ext4")

	templateID := "tpl-split-store"
	seedMigratingTemplate(t, db, templateID, artifactID, srcPath)
	require.NoError(t, db.Model(&models.RootfsArtifact{}).Where("artifact_id = ?", artifactID).Update("owner_tc_download_base", "http://old-tc.example").Error)
	stubMigrateS3Unavailable(t)
	stubMigrateTCUpload(t, func(ctx context.Context, id, filePath string) (*templateCenterUploadResult, error) {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(dstPath, data, 0o644); err != nil {
			return nil, err
		}
		return &templateCenterUploadResult{Ext4Path: dstPath}, nil
	})

	result, err := MigrateTemplateArtifactToTC(context.Background(), templateID)
	require.NoError(t, err)
	require.True(t, result.Migrated)
	require.True(t, result.Cleaned, "different-path migration should remove the local source")
	var migrated models.RootfsArtifact
	require.NoError(t, db.Where("artifact_id = ?", artifactID).First(&migrated).Error)
	require.Equal(t, "http://target-tc.example:8090", migrated.OwnerTCDownloadBase)
	require.Equal(t, "file://"+dstPath, migrated.StorageRef)
	if _, statErr := os.Stat(srcPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected local source removed, stat err=%v", statErr)
	}
}

func TestValidateTCLocalArtifactPath(t *testing.T) {
	primary := t.TempDir()
	fallback := filepath.Join(t.TempDir(), "fallback")
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", primary)

	valid := filepath.Join(primary, "rfs-1", "rfs-1.ext4")
	if err := validateTCLocalArtifactPath("rfs-1", valid); err != nil {
		t.Fatalf("expected path under shared store root to pass, got %v", err)
	}

	badRoots := []string{
		filepath.Join(primary, "rfs-1", "nested", "rfs-1.ext4"),
		filepath.Join(primary, "rfs-1", "other.ext4"),
		filepath.Join(filepath.Dir(primary), "rfs-1", "rfs-1.ext4"),
		filepath.Join(fallback, "rfs-1", "rfs-1.ext4"),
	}
	for _, path := range badRoots {
		if err := validateTCLocalArtifactPath("rfs-1", path); err == nil {
			t.Fatalf("expected path %q to be rejected", path)
		}
	}
}

func TestValidateTCLocalArtifactPathAllowsFallbackStoreWhenEnvUnset(t *testing.T) {
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", "")
	fallbackRoot := ArtifactFallbackStoreRootDir()
	valid := filepath.Join(fallbackRoot, "rfs-2", "rfs-2.ext4")
	if err := validateTCLocalArtifactPath("rfs-2", valid); err != nil {
		t.Fatalf("expected fallback store path to pass when env unset, got %v", err)
	}
}

func TestShouldSkipLocalCleanup(t *testing.T) {
	tests := []struct {
		name       string
		sourcePath string
		targetPath string
		want       bool
	}{
		{
			name:       "same path skips cleanup",
			sourcePath: "/data/CubeMaster/storage/rfs-1/rfs-1.ext4",
			targetPath: "/data/CubeMaster/storage/rfs-1/rfs-1.ext4",
			want:       true,
		},
		{
			name:       "same cleaned path skips cleanup",
			sourcePath: filepath.Clean("/data/CubeMaster/storage/rfs-1//rfs-1.ext4"),
			targetPath: "/data/CubeMaster/storage/rfs-1/rfs-1.ext4",
			want:       true,
		},
		{
			name:       "different paths do not skip",
			sourcePath: "/data/CubeMaster/storage/rfs-1/rfs-1.ext4",
			targetPath: "/data/CubeMaster/storage/rfs-2/rfs-2.ext4",
			want:       false,
		},
		{
			name:       "empty source does not skip",
			sourcePath: "",
			targetPath: "/data/CubeMaster/storage/rfs-1/rfs-1.ext4",
			want:       false,
		},
		{
			name:       "empty target does not skip",
			sourcePath: "/data/CubeMaster/storage/rfs-1/rfs-1.ext4",
			targetPath: "",
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSkipLocalCleanup(tt.sourcePath, tt.targetPath); got != tt.want {
				t.Fatalf("shouldSkipLocalCleanup(%q, %q)=%v, want %v", tt.sourcePath, tt.targetPath, got, tt.want)
			}
		})
	}
}
