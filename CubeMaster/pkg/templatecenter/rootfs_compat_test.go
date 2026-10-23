package templatecenter

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestRootfsLegacyModelAndPhase(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	artifact := models.RootfsArtifact{ArtifactID: "rfs-legacy", RootfsPath: "/rfs-legacy.ext4", RootfsSHA256: "abc", RootfsSizeBytes: 4096}
	if err := db.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	if artifact.LegacyExt4Path != artifact.RootfsPath || artifact.LegacyExt4SHA256 != "abc" {
		t.Fatalf("legacy projection: %+v", artifact)
	}
	// A legacy row can exist before canonical metadata was populated.
	if err := db.Model(&artifact).Updates(map[string]any{"rootfs_path": "", "rootfs_sha256": "", "rootfs_size_bytes": 0}).Error; err != nil {
		t.Fatal(err)
	}
	var got models.RootfsArtifact
	if err := db.First(&got, "artifact_id = ?", artifact.ArtifactID).Error; err != nil {
		t.Fatal(err)
	}
	if got.RootfsPath != "/rfs-legacy.ext4" || got.RootfsSHA256 != "abc" || got.RootfsFormat != "ext4" {
		t.Fatalf("legacy read: %+v", got)
	}
	values := map[string]any{"rootfs_format": "ext4", "rootfs_path": "/new.ext4", "rootfs_sha256": "def", "rootfs_size_bytes": 8192}
	if err := models.RootfsArtifactUpdate(values); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&artifact).Updates(values).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&got, "artifact_id = ?", artifact.ArtifactID).Error; err != nil {
		t.Fatal(err)
	}
	if got.RootfsPath != "/new.ext4" || got.LegacyExt4SHA256 != "def" {
		t.Fatalf("map update: %+v", got)
	}
	job := models.TemplateImageJob{Phase: "BUILDING_EXT4", ResumePhase: "BUILDING_EXT4"}
	if err := job.AfterFind(nil); err != nil {
		t.Fatal(err)
	}
	if job.Phase != JobPhaseBuildingRootfs || job.ResumePhase != JobPhaseBuildingRootfs {
		t.Fatalf("legacy phase: %+v", job)
	}
}

func TestRootfsArtifactUpdateRejectsAmbiguousMetadata(t *testing.T) {
	for _, values := range []map[string]any{
		{"rootfs_path": "/new.ext4"},
		{"rootfs_sha256": "new-sha"},
		{"rootfs_size_bytes": int64(8192)},
		{"rootfs_format": nil, "rootfs_path": "/new.ext4"},
		{"rootfs_format": 1, "rootfs_path": "/new.ext4"},
		{"rootfs_format": "erofs", "rootfs_path": "/new.erofs"},
		{"ext4_path": "/legacy.ext4"},
		{"ext4_sha256": "legacy-sha"},
		{"ext4_size_bytes": int64(8192)},
		{"rootfs_format": "ext4", "ext4_path": "/legacy.ext4"},
		{"rootfs_format": "ext4", "rootfs_path": "/new.ext4", "ext4_sha256": "legacy-sha"},
	} {
		before := make(map[string]any, len(values))
		for k, v := range values {
			before[k] = v
		}
		if err := models.RootfsArtifactUpdate(values); err == nil {
			t.Fatalf("accepted ambiguous or unsupported metadata: %v", values)
		}
		if !reflect.DeepEqual(values, before) {
			t.Fatalf("rejected update was modified: before=%v after=%v", before, values)
		}
	}
}

func TestRootfsArtifactRejectsConflictingStoredMetadata(t *testing.T) {
	for _, tc := range []struct {
		column string
		value  any
	}{
		{"ext4_path", "/different.ext4"},
		{"ext4_sha256", "different-sha"},
		{"ext4_size_bytes", int64(8192)},
	} {
		t.Run(tc.column, func(t *testing.T) {
			db := newMigrateSharedStoreTestDB(t)
			artifact := models.RootfsArtifact{ArtifactID: "rfs-conflict", RootfsPath: "/current.ext4", RootfsSHA256: "current-sha", RootfsSizeBytes: 4096}
			if err := db.Create(&artifact).Error; err != nil {
				t.Fatal(err)
			}
			// Simulate an old writer changing only the legacy representation.
			if err := db.Model(&models.RootfsArtifact{}).Where("artifact_id = ?", artifact.ArtifactID).UpdateColumn(tc.column, tc.value).Error; err != nil {
				t.Fatal(err)
			}
			var got models.RootfsArtifact
			err := db.First(&got, "artifact_id = ?", artifact.ArtifactID).Error
			if err == nil || !strings.Contains(err.Error(), "conflicting rootfs_*") || !strings.Contains(err.Error(), artifact.ArtifactID) {
				t.Fatalf("expected identifiable metadata conflict, got %v", err)
			}
		})
	}
}

func TestRootfsArtifactPartialUpdatesPreserveMetadata(t *testing.T) {
	db := newMigrateSharedStoreTestDB(t)
	artifact := models.RootfsArtifact{ArtifactID: "rfs-partial", RootfsPath: "/old.ext4", RootfsSHA256: "old-sha", RootfsSizeBytes: 4096, Status: ArtifactStatusReady}
	if err := db.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := updateRootfsArtifact(ctx, artifact.ArtifactID, map[string]any{"rootfs_path": "/rejected.ext4"}); err == nil {
		t.Fatal("metadata update without format reached the database")
	}
	if _, err := updateRootfsArtifactIfStatus(ctx, artifact.ArtifactID, ArtifactStatusReady, map[string]any{"rootfs_sha256": "rejected-sha"}); err == nil {
		t.Fatal("conditional metadata update without format reached the database")
	}
	var got models.RootfsArtifact
	if err := db.First(&got, "artifact_id = ?", artifact.ArtifactID).Error; err != nil {
		t.Fatal(err)
	}
	if got.RootfsPath != artifact.RootfsPath || got.RootfsSHA256 != artifact.RootfsSHA256 {
		t.Fatalf("rejected update changed metadata: %+v", got)
	}
	// Explicit empty format retains the historical default, while omitted
	// metadata fields retain both their canonical and legacy values.
	ok, err := updateRootfsArtifactIfStatus(ctx, artifact.ArtifactID, ArtifactStatusReady, map[string]any{"rootfs_format": "", "rootfs_path": "/new.ext4"})
	if err != nil || !ok {
		t.Fatalf("partial metadata update: matched=%t err=%v", ok, err)
	}
	if err := updateRootfsArtifact(ctx, artifact.ArtifactID, map[string]any{"last_error": "status-only"}); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&got, "artifact_id = ?", artifact.ArtifactID).Error; err != nil {
		t.Fatal(err)
	}
	if got.RootfsFormat != "ext4" || got.RootfsPath != "/new.ext4" || got.LegacyExt4Path != got.RootfsPath ||
		got.RootfsSHA256 != artifact.RootfsSHA256 || got.LegacyExt4SHA256 != artifact.RootfsSHA256 ||
		got.RootfsSizeBytes != artifact.RootfsSizeBytes || got.LegacyExt4SizeBytes != artifact.RootfsSizeBytes || got.LastError != "status-only" {
		t.Fatalf("partial/status update lost metadata: %+v", got)
	}
}

func TestExplicitExt4KeepsHistoricalRequestIdentity(t *testing.T) {
	req := &types.CreateTemplateFromImageReq{SourceImageRef: "example/image", WritableLayerSize: "1Gi"}
	legacy := buildTemplateSpecFingerprint(req, "sha256:abc")
	before, err := marshalTemplateImageJobRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	req.RootfsFormat = "ext4"
	after, err := marshalTemplateImageJobRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if before != after || legacy != buildTemplateSpecFingerprint(req, "sha256:abc") {
		t.Fatal("explicit EXT4 changed historical identity")
	}
	info := types.RootfsArtifactInfo{}
	if err := json.Unmarshal([]byte(`{"artifact_id":"rfs-1","ext4_path":"/old.ext4","ext4_sha256":"abc","ext4_size_bytes":4096}`), &info); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	if info.RootfsFormat != "ext4" || fields["rootfs_path"] != "/old.ext4" || fields["ext4_path"] != "/old.ext4" {
		t.Fatalf("wire=%s", wire)
	}
}

func TestRootfsFormatParticipatesInArtifactIdentity(t *testing.T) {
	req := &types.CreateTemplateFromImageReq{WritableLayerSize: "1Gi"}
	historical := buildTemplateSpecFingerprint(req, "sha256:abc")
	if historical != "5347f746de8d0c41432c69ce296f6013ad1e50143d4f3953e7ef380cd38dbf4e" {
		t.Fatalf("historical identity changed: %s", historical)
	}
	req.RootfsFormat = "ext4"
	if got := buildTemplateSpecFingerprint(req, "sha256:abc"); got != historical {
		t.Fatal("explicit EXT4 changed identity")
	}
	// The fingerprint must distinguish formats even before another builder is enabled.
	req.RootfsFormat = "erofs"
	if got := buildTemplateSpecFingerprint(req, "sha256:abc"); got == historical {
		t.Fatal("rootfs formats share an artifact identity")
	}
}

func TestArtifactImageSpecCarriesResolvedFormat(t *testing.T) {
	for _, format := range []string{"", "ext4"} {
		spec, err := artifactImageSpec(&models.RootfsArtifact{ArtifactID: "rfs-1", RootfsFormat: format})
		if err != nil || spec.Image != "rfs-1" || spec.StorageMedia != "ext4" {
			t.Fatalf("format=%q spec=%v err=%v", format, spec, err)
		}
	}
	if _, err := artifactImageSpec(&models.RootfsArtifact{ArtifactID: "rfs-1", RootfsFormat: "unknown"}); err == nil {
		t.Fatal("unknown format must not be dispatched as EXT4")
	}
}
