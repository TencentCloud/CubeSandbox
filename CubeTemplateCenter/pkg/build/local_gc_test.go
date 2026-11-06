// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"gorm.io/gorm"
)

func TestLocalArtifactSweepDurableReferences(t *testing.T) {
	for _, owner := range []string{"artifact", "definition", "replica", "placement", "snapshot", "built", "unassigned-build", "orphan"} {
		t.Run(owner, func(t *testing.T) {
			root := t.TempDir()
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "gc.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			if err := db.AutoMigrate(&models.RootfsArtifact{}, &models.TemplateDefinition{}, &models.TemplateReplica{}, &models.ArtifactNodePlacement{}, &models.SnapshotRecord{}, &models.TemplateImageJob{}); err != nil {
				t.Fatal(err)
			}
			const id = "rfs-local-gc"
			var ref any
			switch owner {
			case "artifact":
				ref = &models.RootfsArtifact{ArtifactID: id, Status: templatecenter.ArtifactStatusReady}
			case "definition":
				ref = &models.TemplateDefinition{TemplateID: "tpl-existing", RootfsArtifactID: id}
			case "replica":
				ref = &models.TemplateReplica{TemplateID: "tpl-existing", ArtifactID: id}
			case "placement":
				ref = &models.ArtifactNodePlacement{ArtifactID: id, NodeID: "node-existing"}
			case "snapshot":
				ref = &models.SnapshotRecord{SnapshotID: "snap-existing", RootfsArtifactID: id}
			case "built":
				ref = &models.TemplateImageJob{JobID: "job-unregistered", ArtifactID: id, Status: templatecenter.JobStatusBuilt}
			case "unassigned-build":
				ref = &models.TemplateImageJob{JobID: "job-unassigned", Status: templatecenter.JobStatusRunning}
			}
			if ref != nil {
				if err := db.Create(ref).Error; err != nil {
					t.Fatal(err)
				}
			}
			dir := filepath.Join(root, id)
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-3 * time.Hour)
			if err := os.Chtimes(dir, old, old); err != nil {
				t.Fatal(err)
			}
			deleter := NewArtifactDeleter(db, nil)
			executor := &Executor{inFlight: map[string]struct{}{}}
			if err := sweepLocalArtifactDirs(t.Context(), []string{root}, executor.WithIdle, deleter.localArtifactUnreferenced); err != nil {
				t.Fatal(err)
			}
			_, err = os.Stat(dir)
			if owner == "orphan" {
				if !os.IsNotExist(err) {
					t.Fatalf("orphan not removed: %v", err)
				}
			} else if err != nil {
				t.Fatalf("%s reference did not protect local file: %v", owner, err)
			}
		})
	}
}

func TestLocalArtifactSweepSafety(t *testing.T) {
	for _, name := range []string{"orphan", "referenced", "database-error", "in-flight", "recent", "marker", "symlink"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "rfs-orphan")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if name == "marker" {
				if err := os.WriteFile(filepath.Join(dir, publicationMarkerName), []byte("active"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if name == "symlink" {
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), dir); err != nil {
					t.Fatal(err)
				}
			} else if name != "recent" {
				old := time.Now().Add(-3 * time.Hour)
				if err := os.Chtimes(dir, old, old); err != nil {
					t.Fatal(err)
				}
			}
			executor := &Executor{inFlight: make(map[string]struct{})}
			if name == "in-flight" {
				executor.inFlight["uploading"] = struct{}{}
			}
			err := sweepLocalArtifactDirs(t.Context(), []string{root}, executor.WithIdle, func(context.Context, string) (bool, error) {
				if name == "database-error" {
					return false, errors.New("db unavailable")
				}
				return name != "referenced", nil
			})
			if (err != nil) != (name == "database-error") {
				t.Fatalf("unexpected sweep error: %v", err)
			}
			_, statErr := os.Lstat(dir)
			if name == "orphan" {
				if !os.IsNotExist(statErr) {
					t.Fatalf("orphan not removed: %v", statErr)
				}
			} else if statErr != nil {
				t.Fatalf("protected directory removed: %v", statErr)
			}
		})
	}
}

func TestArtifactPublicationMarkerLifecycle(t *testing.T) {
	dir := t.TempDir()
	release, err := holdArtifactPublicationMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	if _, err := os.Stat(filepath.Join(dir, publicationMarkerName)); err != nil {
		t.Fatal(err)
	}
	release()
	release()
	if _, err := os.Stat(filepath.Join(dir, publicationMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("marker not released: %v", err)
	}
}

func TestExecutorWithIdleSkipsActiveBuild(t *testing.T) {
	e := &Executor{inFlight: map[string]struct{}{"building": {}}}
	called := false
	if err := e.WithIdle(func() error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("cleanup executed during a build")
	}
	delete(e.inFlight, "building")
	sentinel := errors.New("cleanup error")
	if err := e.WithIdle(func() error { called = true; return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("idle cleanup did not execute")
	}
}

func TestLocalArtifactSweepRechecksReferencesInsideIdleGate(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "rfs-late-reference")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	inside := false
	err := sweepLocalArtifactDirs(t.Context(), []string{root}, func(fn func() error) error { inside = true; return fn() },
		func(context.Context, string) (bool, error) {
			if !inside {
				t.Fatal("reference lookup occurred before acquiring idle gate")
			}
			return false, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("new reference did not protect artifact:", err)
	}
}
