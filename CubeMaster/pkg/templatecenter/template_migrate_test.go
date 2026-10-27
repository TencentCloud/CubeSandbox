package templatecenter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore/configenv"
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
	uploadArtifactFileToS3 = func(ctx context.Context, artifactID, filePath, rootfsFormat string) error {
		return errS3PresignNotConfigured
	}
	t.Cleanup(func() { uploadArtifactFileToS3 = oldUpload })
}

func stubMigrateTCUpload(t *testing.T, fn func(ctx context.Context, artifactID, filePath string) (*templateCenterUploadResult, error)) {
	t.Helper()
	oldUpload := uploadArtifactFileToTC
	uploadArtifactFileToTC = fn
	t.Cleanup(func() { uploadArtifactFileToTC = oldUpload })
}

func seedMigratingTemplate(t *testing.T, db *gorm.DB, templateID, artifactID, ext4Path string) {
	seedMigratingTemplateWithFormat(t, db, templateID, artifactID, ext4Path, "")
}

// seedMigratingTemplateWithFormat is the format-aware variant: it sets the
// artifact row's RootfsFormat so the S3 write path derives the object key from
// it (the same column TC's Stat/Delete read) rather than from the local file
// extension.
func seedMigratingTemplateWithFormat(t *testing.T, db *gorm.DB, templateID, artifactID, ext4Path, rootfsFormat string) {
	t.Helper()
	require.NoError(t, db.Create(&models.TemplateDefinition{
		TemplateID:       templateID,
		Status:           StatusReady,
		RootfsArtifactID: artifactID,
	}).Error)
	require.NoError(t, db.Create(&models.RootfsArtifact{
		ArtifactID:   artifactID,
		Status:       ArtifactStatusReady,
		Ext4Path:     ext4Path,
		RootfsFormat: rootfsFormat,
	}).Error)
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
	stubMigrateS3Unavailable(t)
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
	if _, statErr := os.Stat(srcPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected local source removed, stat err=%v", statErr)
	}
}

// TestMigrateTemplateArtifactToTCS3ErofsRecordsErofsObjectKey pins down the
// RootfsFormat-driven S3 write path: an erofs artifact migrated to S3 must be
// uploaded and recorded under <prefix>/<id>.erofs (the key TC's Stat/Delete
// derive from artifact.RootfsFormat), NOT the legacy <id>.ext4. The key source
// is the artifact row's RootfsFormat column, independent of the local file's
// own name, so a key written here is always read back on the TC side.
func TestMigrateTemplateArtifactToTCS3ErofsRecordsErofsObjectKey(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	sharedRoot := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", sharedRoot)
	t.Setenv(configenv.EnvS3ArtifactPrefix, "template-artifacts")

	artifactID := "rfs-s3-erofs"
	storeDir := filepath.Join(sharedRoot, artifactID)
	require.NoError(t, os.MkdirAll(storeDir, 0o755))
	// The on-disk file carries the .erofs suffix as TC's BuildErofs writes it;
	// the object key must come from RootfsFormat, not this name, but the two
	// agree here so the test also doubles as an end-to-end happy-path check.
	ext4Path := filepath.Join(storeDir, artifactID+".erofs")
	require.NoError(t, os.WriteFile(ext4Path, []byte("erofs-bytes"), 0o644))

	templateID := "tpl-s3-erofs"
	seedMigratingTemplateWithFormat(t, db, templateID, artifactID, ext4Path, config.RootfsFormatErofs)

	// artifactStoreColumns reads sharedArtifactStore(); inject a non-nil store
	// (backend only, no real blobstore.Store needed) so it returns the S3
	// object_key. uploadArtifactFileToS3 is stubbed below so the store is never
	// used for the actual Put.
	_ = sharedArtifactStore()
	oldStore := artifactStoreInst
	artifactStoreInst = &artifactStore{backend: "s3"}
	t.Cleanup(func() { artifactStoreInst = oldStore })

	// Capture the rootfsFormat the write path received; it must be "erofs".
	var uploadedRootfsFormat string
	oldUpload := uploadArtifactFileToS3
	uploadArtifactFileToS3 = func(ctx context.Context, id, filePath, rootfsFormat string) error {
		uploadedRootfsFormat = rootfsFormat
		return nil
	}
	t.Cleanup(func() { uploadArtifactFileToS3 = oldUpload })

	// presign must succeed so the S3 branch is taken (not the TC-local fallback).
	// It must also receive the format-aware object key as ObjectKey (NOT the
	// pre-migration snapshot.objectKey, which is empty here and would make
	// artifactStoreKeyWithPrefix fall back to <id>.ext4 while the upload wrote
	// <id>.erofs -- the stored artifact_url would then point at a never-created
	// object). Capturing the presigned URL's key pins this down.
	var presignedObjectKey string
	stubPresign(t, func(ctx context.Context, artifact *models.RootfsArtifact) (string, error) {
		presignedObjectKey = strings.TrimSpace(artifact.ObjectKey)
		return "https://s3.example/" + artifact.ObjectKey + "?token=x", nil
	})

	result, err := MigrateTemplateArtifactToTC(context.Background(), templateID)
	require.NoError(t, err)
	require.True(t, result.Migrated, "S3 upload branch should mark the artifact migrated")

	require.Equal(t, config.RootfsFormatErofs, uploadedRootfsFormat,
		"uploadArtifactFileToS3 must receive the artifact's RootfsFormat so the object key is format-derived")

	// presign must have been handed the format-aware key, the same value written
	// to object_key below -- so artifact_url and object_key come from one source.
	require.Equal(t, "template-artifacts/"+artifactID+".erofs", presignedObjectKey,
		"presign must receive the format-aware <prefix>/<id>.erofs object key, not the empty pre-migration objectKey that falls back to .ext4")

	// The persisted row must record the erofs object key TC will Stat/Delete.
	var row models.RootfsArtifact
	require.NoError(t, db.Where("artifact_id = ?", artifactID).First(&row).Error)
	require.Equal(t, "template-artifacts/"+artifactID+".erofs", row.ObjectKey,
		"object_key must be <prefix>/<id>.erofs so TC's format-aware Stat/Delete find it")
	require.Equal(t, "s3", row.StorageBackend)
	require.Equal(t, ArtifactStatusReady, row.Status)
}

func TestValidateTCLocalArtifactPath(t *testing.T) {
	primary := t.TempDir()
	fallback := filepath.Join(t.TempDir(), "fallback")
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", primary)

	valid := filepath.Join(primary, "rfs-1", "rfs-1.ext4")
	if err := validateTCLocalArtifactPath("rfs-1", valid); err != nil {
		t.Fatalf("expected path under shared store root to pass, got %v", err)
	}

	// An erofs artifact is stored as <id>.erofs (TC preserves the uploaded
	// suffix), so that layout must validate too -- otherwise the migration of
	// an erofs template would reject the very path TC returned.
	validErofs := filepath.Join(primary, "rfs-1", "rfs-1.erofs")
	if err := validateTCLocalArtifactPath("rfs-1", validErofs); err != nil {
		t.Fatalf("expected erofs path under shared store root to pass, got %v", err)
	}

	badRoots := []string{
		filepath.Join(primary, "rfs-1", "nested", "rfs-1.ext4"),
		filepath.Join(primary, "rfs-1", "other.ext4"),
		filepath.Join(filepath.Dir(primary), "rfs-1", "rfs-1.ext4"),
		filepath.Join(fallback, "rfs-1", "rfs-1.ext4"),
		// An erofs path misplaced (wrong parent dir) must still be rejected.
		filepath.Join(primary, "rfs-1", "nested", "rfs-1.erofs"),
		// A non-ext4/non-erofs suffix is not a valid artifact layout even under
		// the right root, so a stray file cannot masquerade as an artifact.
		filepath.Join(primary, "rfs-1", "rfs-1.iso"),
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
