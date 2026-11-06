// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore/configenv"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newStableLocatorTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "artifact.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.RootfsArtifact{}); err != nil {
		t.Fatal(err)
	}
	oldDB := store.db
	store.db = db
	t.Cleanup(func() { store.db = oldDB })
	return db
}

func TestEnsureArtifactStableLocatorRejectsStaleSnapshot(t *testing.T) {
	db := newStableLocatorTestDB(t)
	t.Setenv(constants.TemplateCenterDownloadAddrEnv, "http://legacy-tc.example:8090")
	snapshot := &models.RootfsArtifact{
		ArtifactID: "artifact-stale-locator",
		Ext4Path:   "/var/lib/cube/legacy.ext4",
		Status:     ArtifactStatusReady,
	}
	if err := db.Create(snapshot).Error; err != nil {
		t.Fatal(err)
	}
	original := *snapshot
	if err := db.Model(&models.RootfsArtifact{}).Where("artifact_id = ?", snapshot.ArtifactID).Updates(map[string]any{
		"generation":             9,
		"storage_backend":        "s3",
		"object_key":             "new-prefix/artifact.ext4",
		"storage_ref":            "s3://new-bucket/new-prefix?endpoint=new-s3.example",
		"owner_tc_download_base": "http://new-tc.example:8090",
		"artifact_url":           "https://new-s3.example/new-presigned-url",
		"ext4_path":              "/var/lib/cube/new.ext4",
	}).Error; err != nil {
		t.Fatal(err)
	}
	var before models.RootfsArtifact
	if err := db.Where("artifact_id = ?", snapshot.ArtifactID).First(&before).Error; err != nil {
		t.Fatal(err)
	}
	if err := ensureArtifactStableLocator(context.Background(), snapshot); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("stale snapshot error = %v; want conflict", err)
	}
	if !reflect.DeepEqual(*snapshot, original) {
		t.Fatalf("conflicting backfill changed input: got %+v, want %+v", snapshot, original)
	}
	var after models.RootfsArtifact
	if err := db.Where("artifact_id = ?", snapshot.ArtifactID).First(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("stale snapshot overwrote current row: before %+v, after %+v", before, after)
	}
}

func TestEnsureArtifactStableLocatorCASGuardsOriginalFields(t *testing.T) {
	for _, change := range []struct {
		name  string
		value any
	}{
		{"generation", int64(9)},
		{"storage_backend", "fs"},
		{"object_key", "new/artifact.ext4"},
		{"storage_ref", "file:///new/root"},
		{"owner_tc_download_base", "http://new-tc.example:8090"},
		{"artifact_url", "https://s3.example/artifact.ext4"},
		{"ext4_path", "/var/lib/cube/new.ext4"},
		{"status", ArtifactStatusBuilding},
		{"status", nil},
	} {
		t.Run(change.name, func(t *testing.T) {
			db := newStableLocatorTestDB(t)
			t.Setenv(constants.TemplateCenterDownloadAddrEnv, "http://legacy-tc.example:8090")
			snapshot := &models.RootfsArtifact{
				ArtifactID: "artifact-cas-field",
				Ext4Path:   "/var/lib/cube/legacy.ext4",
				Status:     ArtifactStatusReady,
			}
			if err := db.Create(snapshot).Error; err != nil {
				t.Fatal(err)
			}
			original := *snapshot
			if err := db.Model(&models.RootfsArtifact{}).Where("artifact_id = ?", snapshot.ArtifactID).Update(change.name, change.value).Error; err != nil {
				t.Fatal(err)
			}
			if err := ensureArtifactStableLocator(context.Background(), snapshot); err == nil || !strings.Contains(err.Error(), "conflict") {
				t.Fatalf("changed %s error = %v; want conflict", change.name, err)
			}
			if !reflect.DeepEqual(*snapshot, original) {
				t.Fatalf("conflicting %s backfill changed input", change.name)
			}
		})
	}
}

func TestEnsureArtifactStableLocatorNoOpStillRequiresCurrentReadyRow(t *testing.T) {
	db := newStableLocatorTestDB(t)
	snapshot := &models.RootfsArtifact{
		ArtifactID:          "artifact-complete-locator",
		Generation:          3,
		StorageBackend:      "fs",
		StorageRef:          "file:///artifact-store",
		OwnerTCDownloadBase: "http://tc.example:8090",
		Status:              ArtifactStatusReady,
	}
	if err := db.Create(snapshot).Error; err != nil {
		t.Fatal(err)
	}
	if err := ensureArtifactStableLocator(context.Background(), snapshot); err != nil {
		t.Fatalf("current READY row: %v", err)
	}
	if err := db.Model(&models.RootfsArtifact{}).Where("artifact_id = ?", snapshot.ArtifactID).Update("status", ArtifactStatusBuilding).Error; err != nil {
		t.Fatal(err)
	}
	if err := ensureArtifactStableLocator(context.Background(), snapshot); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("no-op stale READY error = %v; want conflict", err)
	}
}

func TestEnsureArtifactStableLocatorRejectsUnmigratedS3(t *testing.T) {
	for _, backend := range []string{"", "s3", " S3 "} {
		t.Run("backend="+backend, func(t *testing.T) {
			db := newStableLocatorTestDB(t)
			t.Setenv(constants.TemplateCenterDownloadAddrEnv, "http://tc.example:8090")
			t.Setenv(configenv.EnvS3Endpoint, "current-s3.example")
			t.Setenv(configenv.EnvS3Bucket, "current-bucket")
			t.Setenv(configenv.EnvS3AccessKeyID, "test-access-key")
			t.Setenv(configenv.EnvS3SecretAccessKey, "test-secret-key")
			t.Setenv(configenv.EnvS3ArtifactPrefix, "current-prefix")
			artifact := &models.RootfsArtifact{
				ArtifactID:     "artifact-unmigrated-s3",
				StorageBackend: backend,
				Ext4Path:       "/var/lib/cube/legacy.ext4",
				Status:         ArtifactStatusReady,
			}
			if backend == "" {
				artifact.ArtifactURL = "https://legacy-s3.example/old-bucket/legacy.ext4?expired-signature=1"
			}
			if err := db.Create(artifact).Error; err != nil {
				t.Fatal(err)
			}
			original := *artifact
			var before models.RootfsArtifact
			if err := db.Where("artifact_id = ?", artifact.ArtifactID).First(&before).Error; err != nil {
				t.Fatal(err)
			}
			if ref := inferredArtifactStorageRef(artifact); ref != "" {
				t.Fatalf("unmigrated S3 inferred ref = %q; want no locator", ref)
			}
			if err := ensureArtifactStableLocator(context.Background(), artifact); err == nil || !strings.Contains(err.Error(), "explicit migration") {
				t.Fatalf("unmigrated S3 error = %v; want explicit migration", err)
			}
			if !reflect.DeepEqual(*artifact, original) {
				t.Fatalf("rejected S3 migration changed input: %+v", artifact)
			}
			var persisted models.RootfsArtifact
			if err := db.Where("artifact_id = ?", artifact.ArtifactID).First(&persisted).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(persisted, before) {
				t.Fatalf("rejected S3 migration changed DB: got %+v, want %+v", persisted, before)
			}
		})
	}
}

func TestEnsureArtifactStableLocatorAcceptsNullableLegacyColumns(t *testing.T) {
	db := newStableLocatorTestDB(t)
	t.Setenv(constants.TemplateCenterDownloadAddrEnv, "http://tc.example:8090")
	artifact := &models.RootfsArtifact{
		ArtifactID: "artifact-nullable-locator",
		Ext4Path:   "/var/lib/cube/legacy.ext4",
		Status:     ArtifactStatusReady,
	}
	if err := db.Create(artifact).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.RootfsArtifact{}).Where("artifact_id = ?", artifact.ArtifactID).Updates(map[string]any{
		"generation": nil, "storage_backend": nil, "object_key": nil,
		"storage_ref": nil, "owner_tc_download_base": nil, "artifact_url": nil,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("artifact_id = ?", artifact.ArtifactID).First(artifact).Error; err != nil {
		t.Fatal(err)
	}
	if err := ensureArtifactStableLocator(context.Background(), artifact); err != nil {
		t.Fatalf("nullable legacy backfill: %v", err)
	}
	var persisted models.RootfsArtifact
	if err := db.Where("artifact_id = ?", artifact.ArtifactID).First(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if artifact.Generation != 1 || persisted.Generation != 1 ||
		artifact.StorageRef != "file:///var/lib/cube/legacy.ext4" || persisted.StorageRef != artifact.StorageRef ||
		persisted.OwnerTCDownloadBase != "http://tc.example:8090" {
		t.Fatalf("nullable backfill: input %+v, persisted %+v", artifact, persisted)
	}
}

func TestEnsureArtifactStableLocatorFailedUpdatePreservesInput(t *testing.T) {
	db := newStableLocatorTestDB(t)
	t.Setenv(constants.TemplateCenterDownloadAddrEnv, "http://tc.example:8090")
	artifact := &models.RootfsArtifact{
		ArtifactID: "artifact-failed-locator",
		Ext4Path:   "/var/lib/cube/legacy.ext4",
		Status:     ArtifactStatusReady,
	}
	if err := db.Create(artifact).Error; err != nil {
		t.Fatal(err)
	}
	original := *artifact
	var before models.RootfsArtifact
	if err := db.Where("artifact_id = ?", artifact.ArtifactID).First(&before).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_stable_locator BEFORE UPDATE ON " + constants.RootfsArtifactTableName +
		" BEGIN SELECT RAISE(ABORT, 'forced stable locator update failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := ensureArtifactStableLocator(context.Background(), artifact); err == nil || !strings.Contains(err.Error(), "forced stable locator update failure") {
		t.Fatalf("failed update error = %v; want injected DB failure", err)
	}
	if !reflect.DeepEqual(*artifact, original) {
		t.Fatalf("failed update changed input: got %+v, want %+v", artifact, original)
	}
	var persisted models.RootfsArtifact
	if err := db.Where("artifact_id = ?", artifact.ArtifactID).First(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted, before) {
		t.Fatalf("failed update changed DB: got %+v, want %+v", persisted, before)
	}
}

func TestArtifactStableLocatorRequiresInitializedStore(t *testing.T) {
	oldStore := store
	t.Cleanup(func() { store = oldStore })
	for _, emptyStore := range []*localStore{nil, {}} {
		store = emptyStore
		artifact := &models.RootfsArtifact{
			ArtifactID: "artifact-no-store",
			Generation: 1,
			StorageRef: "file:///artifact-store",
			Status:     ArtifactStatusReady,
		}
		if err := ensureArtifactStableLocator(context.Background(), artifact); !errors.Is(err, ErrTemplateStoreNotInitialized) {
			t.Fatalf("uninitialized stable locator error = %v", err)
		}
		if _, err := IssueArtifactDownloadTicket(context.Background(), artifact.ArtifactID, "node-1", ""); !errors.Is(err, ErrTemplateStoreNotInitialized) {
			t.Fatalf("uninitialized ticket error = %v", err)
		}
	}
}
