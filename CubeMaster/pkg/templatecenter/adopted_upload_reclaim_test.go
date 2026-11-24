// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"errors"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"gorm.io/gorm"
)

// A losing remote build uploads an object keyed by its own artifact id. When
// Master adopts a different winner for the same fingerprint, that artifact id
// never gets a row, and the artifact GC only walks rows -- so without an
// explicit reclaim the bytes are unreachable forever (one full rootfs leaked
// per lost race). A wrong delete, on the other hand, destroys a live template,
// so every guard below matters.
func TestReclaimAdoptedArtifactUpload(t *testing.T) {
	cases := []struct {
		name string
		// rowErr is what the row lookup returns for the losing artifact id.
		rowErr     error
		winner     *models.RootfsArtifact
		result     *RemoteBuildResult
		wantDelete string
	}{
		{
			name:       "lost race with no row reclaims the upload",
			rowErr:     gorm.ErrRecordNotFound,
			winner:     &models.RootfsArtifact{ArtifactID: "rfs-winner", StorageBackend: "s3", ObjectKey: "rfs-winner.ext4"},
			result:     &RemoteBuildResult{ArtifactID: "rfs-loser", StorageBackend: "s3", ObjectKey: "rfs-loser.ext4"},
			wantDelete: "rfs-loser",
		},
		{
			name: "adopting our own row never deletes the adopted bytes",
			// A duplicate BUILT replay or a lost finalize CAS adopts the row
			// under OUR artifact id. Deleting the reported object here would
			// delete the artifact the sandbox is about to run.
			rowErr: gorm.ErrRecordNotFound,
			winner: &models.RootfsArtifact{ArtifactID: "rfs-1", StorageBackend: "s3", ObjectKey: "rfs-1.ext4"},
			result: &RemoteBuildResult{ArtifactID: "rfs-1", StorageBackend: "s3", ObjectKey: "rfs-1.ext4"},
		},
		{
			name: "a shared object key is the winner's own data",
			// Different ids but the same key: the two builds wrote the same
			// object, so the reported upload IS the adopted artifact.
			rowErr: gorm.ErrRecordNotFound,
			winner: &models.RootfsArtifact{ArtifactID: "rfs-winner", StorageBackend: "s3", ObjectKey: "shared.ext4"},
			result: &RemoteBuildResult{ArtifactID: "rfs-loser", StorageBackend: "s3", ObjectKey: "shared.ext4"},
		},
		{
			name: "an existing row is left to the artifact GC",
			// The row owns its data. Deleting the object here would pull the
			// bytes out from under a row that may still be serving pulls or
			// awaiting its own cleanup.
			rowErr: nil,
			winner: &models.RootfsArtifact{ArtifactID: "rfs-winner", StorageBackend: "s3", ObjectKey: "rfs-winner.ext4"},
			result: &RemoteBuildResult{ArtifactID: "rfs-loser", StorageBackend: "s3", ObjectKey: "rfs-loser.ext4"},
		},
		{
			name: "an unreadable row is not assumed absent",
			// A failed lookup is not evidence that the row is gone, and
			// guessing wrong here deletes live data.
			rowErr: errors.New("db unavailable"),
			winner: &models.RootfsArtifact{ArtifactID: "rfs-winner", StorageBackend: "s3", ObjectKey: "rfs-winner.ext4"},
			result: &RemoteBuildResult{ArtifactID: "rfs-loser", StorageBackend: "s3", ObjectKey: "rfs-loser.ext4"},
		},
		{
			name:   "a local-disk build uploaded nothing to reclaim",
			rowErr: gorm.ErrRecordNotFound,
			winner: &models.RootfsArtifact{ArtifactID: "rfs-winner", StorageBackend: "s3", ObjectKey: "rfs-winner.ext4"},
			result: &RemoteBuildResult{ArtifactID: "rfs-loser"},
		},
		{
			name:   "a report without a backend has no addressable object",
			rowErr: gorm.ErrRecordNotFound,
			winner: &models.RootfsArtifact{ArtifactID: "rfs-winner", StorageBackend: "s3", ObjectKey: "rfs-winner.ext4"},
			result: &RemoteBuildResult{ArtifactID: "rfs-loser", ObjectKey: "rfs-loser.ext4"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldLookup, oldDelete := lookupArtifactRowUnscoped, requestTemplateCenterArtifactDelete
			t.Cleanup(func() {
				lookupArtifactRowUnscoped, requestTemplateCenterArtifactDelete = oldLookup, oldDelete
			})
			lookupArtifactRowUnscoped = func(_ context.Context, artifactID string) (*models.RootfsArtifact, error) {
				if tc.rowErr != nil {
					return nil, tc.rowErr
				}
				return &models.RootfsArtifact{ArtifactID: artifactID}, nil
			}
			var deleted []string
			requestTemplateCenterArtifactDelete = func(_ context.Context, artifactID string) error {
				deleted = append(deleted, artifactID)
				return nil
			}

			reclaimAdoptedArtifactUpload(context.Background(), tc.winner, tc.result)

			if tc.wantDelete == "" {
				if len(deleted) != 0 {
					t.Fatalf("no delete expected, got %v", deleted)
				}
				return
			}
			if len(deleted) != 1 || deleted[0] != tc.wantDelete {
				t.Fatalf("deleted = %v, want exactly [%s]", deleted, tc.wantDelete)
			}
		})
	}
}

// The adopted artifact is READY and the job must succeed, so a failed reclaim
// is reported rather than propagated: a logged leak is recoverable, a failed
// template creation is not.
func TestReclaimAdoptedArtifactUploadSwallowsDeleteFailure(t *testing.T) {
	oldLookup, oldDelete := lookupArtifactRowUnscoped, requestTemplateCenterArtifactDelete
	t.Cleanup(func() {
		lookupArtifactRowUnscoped, requestTemplateCenterArtifactDelete = oldLookup, oldDelete
	})
	lookupArtifactRowUnscoped = func(context.Context, string) (*models.RootfsArtifact, error) {
		return nil, gorm.ErrRecordNotFound
	}
	called := false
	requestTemplateCenterArtifactDelete = func(context.Context, string) error {
		called = true
		return errors.New("template center unreachable")
	}
	reclaimAdoptedArtifactUpload(context.Background(),
		&models.RootfsArtifact{ArtifactID: "rfs-winner", StorageBackend: "s3", ObjectKey: "rfs-winner.ext4"},
		&RemoteBuildResult{ArtifactID: "rfs-loser", StorageBackend: "s3", ObjectKey: "rfs-loser.ext4"})
	if !called {
		t.Fatal("reclaim must attempt the delete")
	}
}

// Nil inputs reach this from the duplicate-key and lost-CAS adopt branches,
// where the winner is re-read from the database.
func TestReclaimAdoptedArtifactUploadToleratesNilInputs(t *testing.T) {
	oldDelete := requestTemplateCenterArtifactDelete
	t.Cleanup(func() { requestTemplateCenterArtifactDelete = oldDelete })
	requestTemplateCenterArtifactDelete = func(context.Context, string) error {
		t.Fatal("nil input must not trigger a delete")
		return nil
	}
	reclaimAdoptedArtifactUpload(context.Background(), nil, &RemoteBuildResult{ArtifactID: "rfs-loser"})
	reclaimAdoptedArtifactUpload(context.Background(), &models.RootfsArtifact{ArtifactID: "rfs-winner"}, nil)
}
