// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/s3store"
	"gorm.io/gorm"
)

func TestDeleterPreservesEveryDurableReference(t *testing.T) {
	for _, owner := range []string{"definition", "replica", "job", "snapshot", "placement", "none"} {
		t.Run(owner, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "references.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			if err := db.AutoMigrate(&models.RootfsArtifact{}, &models.TemplateDefinition{}, &models.TemplateReplica{}, &models.TemplateImageJob{}, &models.SnapshotRecord{}, &models.ArtifactNodePlacement{}); err != nil {
				t.Fatal(err)
			}
			const id = "rfs-referenced"
			dir := filepath.Join(root, id)
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, id+".ext4")
			if err := os.WriteFile(file, []byte("protected"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&models.RootfsArtifact{ArtifactID: id, Status: templatecenter.ArtifactStatusCleanupPending, Ext4Path: file}).Error; err != nil {
				t.Fatal(err)
			}
			var ref any
			switch owner {
			case "definition":
				ref = &models.TemplateDefinition{TemplateID: "tpl-old", RootfsArtifactID: id, Status: templatecenter.StatusFailed}
			case "replica":
				ref = &models.TemplateReplica{TemplateID: "tpl-old", ArtifactID: id, CleanupRequired: true}
			case "job":
				ref = &models.TemplateImageJob{JobID: "job-resume", ArtifactID: id, Status: templatecenter.JobStatusBuilt}
			case "snapshot":
				ref = &models.SnapshotRecord{SnapshotID: "snap-old", RootfsArtifactID: id}
			case "placement":
				ref = &models.ArtifactNodePlacement{ArtifactID: id, NodeID: "node-old"}
			}
			if ref != nil {
				if err := db.Create(ref).Error; err != nil {
					t.Fatal(err)
				}
			}
			deleter := NewArtifactDeleter(db, nil)
			if err := deleter.Delete(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := db.Model(&models.RootfsArtifact{}).Where("artifact_id = ?", id).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(file)
			if owner == "none" {
				if count != 0 || !os.IsNotExist(statErr) {
					t.Fatal("unreferenced source was not removed")
				}
			} else if count != 1 || statErr != nil {
				t.Fatal("referenced source or recovery record was removed")
			}
		})
	}
}

func TestDeleteLocalExt4RemovesManagedDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)

	artifactID := "rfs-del-001"
	artifactDir := filepath.Join(root, artifactID)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ext4Path := filepath.Join(artifactDir, artifactID+".ext4")
	if err := os.WriteFile(ext4Path, []byte("fake ext4 data"), 0o644); err != nil {
		t.Fatalf("write ext4: %v", err)
	}

	if err := deleteLocalExt4(ext4Path); err != nil {
		t.Fatalf("deleteLocalExt4: %v", err)
	}
	if _, err := os.Stat(artifactDir); !os.IsNotExist(err) {
		t.Fatalf("expected artifact dir removed, stat err=%v", err)
	}
}

func TestDeleteLocalExt4RefusesPathTraversal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)

	// A path OUTSIDE the managed root must be refused and NOT deleted.
	outsideDir := t.TempDir()
	evilPath := filepath.Join(outsideDir, "evil.ext4")
	if err := os.WriteFile(evilPath, []byte("evil"), 0o644); err != nil {
		t.Fatalf("write evil file: %v", err)
	}

	err := deleteLocalExt4(evilPath)
	if err == nil {
		t.Fatalf("expected path-traversal refusal, got nil")
	}
	// The outside file must still exist.
	if _, statErr := os.Stat(evilPath); statErr != nil {
		t.Fatalf("expected outside file preserved, stat err=%v", statErr)
	}
}

func TestManagedArtifactDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", root)

	inside := filepath.Join(root, "rfs-abc", "sub")
	outside := t.TempDir()

	if !managedArtifactDir(inside) {
		t.Fatalf("expected %q recognized as managed", inside)
	}
	if managedArtifactDir(outside) {
		t.Fatalf("expected %q recognized as NOT managed", outside)
	}
	// The root itself is deliberately NOT managed: deleteLocalExt4 runs
	// RemoveAll(dir), so treating the store root as managed would wipe every
	// artifact on the node (and the callback-side path validator accepts
	// exactly such top-level paths).
	if managedArtifactDir(root) {
		t.Fatalf("expected root %q recognized as NOT managed (RemoveAll would wipe the whole store)", root)
	}
}

func TestDeleterEmptyArtifactID(t *testing.T) {
	deleter := NewArtifactDeleter(nil, nil)
	if err := deleter.Delete(t.Context(), ""); err == nil {
		t.Fatalf("expected error for empty artifact_id")
	}
	if err := deleter.Delete(t.Context(), "   "); err == nil {
		t.Fatalf("expected error for whitespace-only artifact_id")
	}
}

func TestDeleterValidatesStoreBeforeDeletingAnyData(t *testing.T) {
	for _, scenario := range []string{"foreign-root", "matching-root", "missing-client", "missing-ref", "legacy-missing-backend", "wrong-backend", "wrong-prefix", "missing-prefix", "missing-key"} {
		t.Run(scenario, func(t *testing.T) {
			localRoot := t.TempDir()
			t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", localRoot)
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "ownership.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			if err := db.AutoMigrate(&models.RootfsArtifact{}, &models.TemplateDefinition{}, &models.TemplateReplica{}, &models.TemplateImageJob{}, &models.SnapshotRecord{}, &models.ArtifactNodePlacement{}); err != nil {
				t.Fatal(err)
			}
			owner, err := s3store.NewClient(s3store.Config{Driver: "fs", FSRoot: t.TempDir(), ArtifactPrefix: "templates"})
			if err != nil {
				t.Fatal(err)
			}
			peer, err := s3store.NewClient(s3store.Config{Driver: "fs", FSRoot: t.TempDir(), ArtifactPrefix: "templates"})
			if err != nil {
				t.Fatal(err)
			}
			const id = "rfs-store-owned"
			dir := filepath.Join(localRoot, id)
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, id+".ext4")
			content := []byte("protected rootfs")
			if err := os.WriteFile(file, content, 0o600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(content)
			sum := hex.EncodeToString(digest[:])
			key, err := owner.UploadVersioned(t.Context(), id, file, sum)
			if err != nil {
				t.Fatal(err)
			}
			peerKey, err := peer.UploadVersioned(t.Context(), id, file, sum)
			if err != nil {
				t.Fatal(err)
			}
			if peerKey != key {
				t.Fatalf("same-key setup differs: owner = %q, peer = %q", key, peerKey)
			}
			row := models.RootfsArtifact{
				ArtifactID: id, Status: templatecenter.ArtifactStatusCleanupPending,
				Ext4Path: file, StorageBackend: "fs", StorageRef: owner.StorageRef(), ObjectKey: key,
			}
			client := owner
			switch scenario {
			case "foreign-root":
				client = peer
			case "missing-client":
				client = nil
			case "missing-ref":
				row.StorageRef = ""
			case "legacy-missing-backend":
				row.StorageBackend = ""
				row.StorageRef = ""
				row.ArtifactURL = "http://legacy.example.invalid/artifact.ext4?stale=1"
			case "wrong-backend":
				row.StorageBackend = "s3"
			case "wrong-prefix":
				row.ObjectKey = "other/" + filepath.Base(key)
			case "missing-prefix":
				row.ObjectKey = filepath.Base(key)
			case "missing-key":
				row.ObjectKey = ""
			}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			deleteErr := NewArtifactDeleter(db, client).Delete(t.Context(), id)
			matching := scenario == "matching-root"
			if (deleteErr == nil) != matching {
				t.Fatalf("delete error = %v, matching = %v", deleteErr, matching)
			}
			assertObject := func(store *s3store.Client, wantExists bool) {
				t.Helper()
				exists, err := store.StatStored(t.Context(), key, id)
				if err != nil || exists != wantExists {
					t.Fatalf("object in %q exists = %v, want %v, error = %v", store.StorageRef(), exists, wantExists, err)
				}
			}
			assertObject(owner, !matching)
			assertObject(peer, true)
			var count int64
			if err := db.Model(&models.RootfsArtifact{}).Where("artifact_id = ?", id).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(file)
			if matching {
				if count != 0 || !os.IsNotExist(statErr) {
					t.Fatalf("matching store not deleted: rows = %d, local stat = %v", count, statErr)
				}
				return
			}
			if count != 1 || statErr != nil {
				t.Fatalf("wrong owner deleted data or row: rows = %d, local stat = %v", count, statErr)
			}
			var kept models.RootfsArtifact
			if err := db.Where("artifact_id = ?", id).First(&kept).Error; err != nil {
				t.Fatal(err)
			}
			if kept.Status != templatecenter.ArtifactStatusCleanupPending || kept.LastError == "" {
				t.Fatalf("cleanup recovery marker = status %q, last_error %q", kept.Status, kept.LastError)
			}
			if scenario == "foreign-root" || scenario == "missing-client" {
				if err := NewArtifactDeleter(db, owner).Delete(t.Context(), id); err != nil {
					t.Fatalf("retry with owner client failed: %v", err)
				}
				assertObject(owner, false)
				assertObject(peer, true)
				if err := db.Model(&models.RootfsArtifact{}).Where("artifact_id = ?", id).Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(file); !os.IsNotExist(err) || count != 0 {
					t.Fatalf("owner retry kept local or row: rows = %d, local stat = %v", count, err)
				}
			}
		})
	}
}
