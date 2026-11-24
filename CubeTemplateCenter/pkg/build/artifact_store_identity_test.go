// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/s3store"
)

// identityStore is a minimal artifactBuildStore for identity checks. Stat is
// never reached by artifactMatchesStore, so it only needs to compile.
type identityStore struct {
	backend string
	bucket  string
	prefix  string
}

func (s *identityStore) Stat(context.Context, string) (bool, error) { return true, nil }
func (s *identityStore) BackendName() string                        { return s.backend }
func (s *identityStore) Bucket() string                             { return s.bucket }
func (s *identityStore) FullObjectKey(artifactID string) string {
	return s.prefix + artifactID + ".ext4"
}

func configuredStore() *identityStore {
	return &identityStore{backend: "s3", bucket: "cube-artifacts", prefix: "template-artifacts/"}
}

// Rows written before the bucket column existed carry an empty bucket, and the
// migration does not backfill it. Reading that as "a different bucket" made
// every pre-upgrade artifact permanently unreusable and undeletable.
func TestArtifactMatchesStoreTreatsMissingIdentityAsUnknown(t *testing.T) {
	store := configuredStore()
	cases := []struct {
		name     string
		artifact *models.RootfsArtifact
	}{
		{"pre-upgrade row: no bucket, no object key", &models.RootfsArtifact{
			ArtifactID: "rfs-1", StorageBackend: "s3",
		}},
		{"legacy row: only an artifact URL", &models.RootfsArtifact{
			ArtifactID: "rfs-1",
		}},
		{"bucket backfilled, object key still empty", &models.RootfsArtifact{
			ArtifactID: "rfs-1", StorageBackend: "s3", Bucket: "cube-artifacts",
		}},
		{"fully populated and consistent", &models.RootfsArtifact{
			ArtifactID: "rfs-1", StorageBackend: "s3", Bucket: "cube-artifacts",
			ObjectKey: "template-artifacts/rfs-1.ext4",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ok, reason := artifactMatchesStore(tc.artifact, store); !ok {
				t.Fatalf("artifact must stay eligible, got refusal: %s", reason)
			}
		})
	}
}

// Only a populated value that disagrees is a real mismatch.
func TestArtifactMatchesStoreRejectsPopulatedMismatch(t *testing.T) {
	store := configuredStore()
	cases := []struct {
		name     string
		artifact *models.RootfsArtifact
		wantIn   string
	}{
		{"different bucket", &models.RootfsArtifact{
			ArtifactID: "rfs-1", StorageBackend: "s3", Bucket: "someone-elses-bucket",
		}, "bucket"},
		{"different backend", &models.RootfsArtifact{
			ArtifactID: "rfs-1", StorageBackend: "fs", Bucket: "cube-artifacts",
		}, "storage backend"},
		{"object key from another layout", &models.RootfsArtifact{
			ArtifactID: "rfs-1", StorageBackend: "s3", Bucket: "cube-artifacts",
			ObjectKey: "other-prefix/rfs-1.ext4",
		}, "object key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := artifactMatchesStore(tc.artifact, store)
			if ok {
				t.Fatal("a populated, disagreeing identity must be refused")
			}
			if !strings.Contains(reason, tc.wantIn) {
				t.Fatalf("reason %q must name the field %q", reason, tc.wantIn)
			}
		})
	}
}

// In local-storage mode s3Client is a nil *s3store.Client. Converting it
// straight into the interface yields a non-nil interface holding a nil
// pointer, so a `store != nil` guard passes and the first method call panics.
func TestArtifactStoreNilHandlingAvoidsTypedNilPanic(t *testing.T) {
	var client *s3store.Client

	if got := artifactStoreOrNil(client); got != nil {
		t.Fatal("a nil client must convert to a nil interface, not a typed nil")
	}
	if !artifactStoreAbsent(artifactBuildStore(client)) {
		t.Fatal("a typed-nil store must be reported as absent")
	}
	if !artifactStoreAbsent(nil) {
		t.Fatal("an untyped nil store must be reported as absent")
	}
	if artifactStoreAbsent(configuredStore()) {
		t.Fatal("a usable store must not be reported as absent")
	}

	// The typed nil must be refused instead of dereferenced.
	artifact := &models.RootfsArtifact{ArtifactID: "rfs-1", StorageBackend: "s3"}
	ok, reason := artifactMatchesStore(artifact, artifactBuildStore(client))
	if ok {
		t.Fatal("an unconfigured store must never match")
	}
	if !strings.Contains(reason, "not configured") {
		t.Fatalf("reason %q must say the store is not configured", reason)
	}
}
