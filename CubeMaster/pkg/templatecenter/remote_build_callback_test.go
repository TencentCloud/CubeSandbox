// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func validRemoteBuildResultForCallbackTest(t *testing.T) *RemoteBuildResult {
	t.Helper()
	return &RemoteBuildResult{
		ArtifactID:              "rfs-callback",
		TemplateSpecFingerprint: "fingerprint-callback",
		SourceImageDigest:       "repo/image@sha256:abc",
		Ext4Path:                writeFakeArtifactFile(t, "callback.ext4"),
		Ext4SHA256:              "sha256:def",
		Ext4SizeBytes:           1024,
		ImageConfigJSON:         `{}`,
		MasterNodeIP:            "http://master:8080",
	}
}

func patchRemoteBuildPrepareDependencies(t *testing.T, updates *[]map[string]any, status, phase string) {
	t.Helper()
	patches := gomonkey.NewPatches()
	patches.ApplyFunc(getTemplateImageJobRecordByID, func(context.Context, string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{JobID: "job-1", TemplateID: "tpl-1", Status: status, Phase: phase}, nil
	})
	patches.ApplyFunc(unmarshalTemplateImageJobRequest, func(string) (*types.CreateTemplateFromImageReq, error) {
		return &types.CreateTemplateFromImageReq{TemplateID: "tpl-1"}, nil
	})
	patches.ApplyFunc(claimTemplateImageJobDistribution, func(_ context.Context, _ string, values map[string]any) (bool, error) {
		cloned := make(map[string]any, len(values))
		for key, value := range values {
			cloned[key] = value
		}
		*updates = append(*updates, cloned)
		return true, nil
	})
	t.Cleanup(patches.Reset)

	db := newMigrateSharedStoreTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.TemplateImageJob{}))
	require.NoError(t, db.Create(&models.TemplateImageJob{
		JobID:      "job-1",
		TemplateID: "tpl-1",
		Status:     status,
		Phase:      phase,
		ResultJSON: `{"artifact_id":"rfs-callback"}`,
	}).Error)
}

func TestRemoteBuildResultValidateRejectsInvalidS3Locator(t *testing.T) {
	t.Setenv("CUBE_S3_ENDPOINT", "http://minio:9000")
	t.Setenv("CUBE_S3_BUCKET", "artifact-bucket")
	t.Setenv("CUBE_S3_ACCESS_KEY_ID", "access")
	t.Setenv("CUBE_S3_SECRET_ACCESS_KEY", "secret")
	t.Setenv("CUBE_S3_ARTIFACT_PREFIX", "template-artifacts")

	result := validRemoteBuildResultForCallbackTest(t)
	result.StorageBackend = "s3"
	result.ObjectKey = "template-artifacts/rfs-callback.ext4"
	result.StorageRef = "file:///legacy/rfs-callback.ext4"
	if err := result.Validate(); err == nil || !strings.Contains(err.Error(), "invalid S3 storage_ref") {
		t.Fatalf("Validate() error = %v, want invalid S3 storage_ref", err)
	}

	result.StorageRef = "s3://artifact-bucket/template-artifacts?endpoint=http%3A%2F%2Fminio%3A9000"
	result.Ext4Path = ""
	require.NoError(t, result.Validate(), "S3 report must not depend on a Master-local ext4 path")
}

func TestRemoteBuildResultValidateAcceptsTCFSLocator(t *testing.T) {
	result := validRemoteBuildResultForCallbackTest(t)
	result.StorageBackend = "fs"
	result.ObjectKey = "rfs-callback.ext4"
	result.StorageRef = "file:///var/lib/templatecenter/artifacts/rfs-callback.ext4"
	result.OwnerTCDownloadBase = "http://templatecenter:8090"
	result.Ext4Path = ""
	require.NoError(t, result.Validate(), "TC-FS report must not depend on a Master-local ext4 path")
}

func TestValidateReadyArtifactForAdoptionRequiresS3Locator(t *testing.T) {
	artifact := &models.RootfsArtifact{
		ArtifactID:     "rfs-ready-without-locator",
		StorageBackend: "s3",
		ObjectKey:      "template-artifacts/rfs-ready-without-locator.ext4",
	}
	err := validateReadyArtifactForAdoption(t.Context(), artifact)
	require.ErrorContains(t, err, "storage_ref is empty")
}

func TestPrepareRemoteBuildCallbackRegistersBeforeDistribution(t *testing.T) {
	var updates []map[string]any
	patchRemoteBuildPrepareDependencies(t, &updates, JobStatusBuilt, JobPhaseReady)
	result := validRemoteBuildResultForCallbackTest(t)
	registered := false
	registrar := func(_ context.Context, _ *types.CreateTemplateFromImageReq, got *RemoteBuildResult) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, bool, error) {
		registered = true
		if got != result {
			t.Fatal("registrar received a different build result")
		}
		return &models.RootfsArtifact{
			ArtifactID:              result.ArtifactID,
			TemplateSpecFingerprint: result.TemplateSpecFingerprint,
			SourceImageDigest:       result.SourceImageDigest,
			Status:                  ArtifactStatusReady,
		}, &types.CreateCubeSandboxReq{}, false, nil
	}

	continuation, err := prepareTemplateImageJobAfterRemoteBuild(context.Background(), "job-1", result, registrar, true)
	if err != nil {
		t.Fatalf("prepareTemplateImageJobAfterRemoteBuild() error = %v", err)
	}
	if !registered {
		t.Fatal("artifact was not registered synchronously")
	}
	if continuation == nil {
		t.Fatal("continuation is nil")
	}
	if len(updates) != 1 {
		t.Fatalf("job updates = %d, want 1", len(updates))
	}
	want := map[string]any{
		"artifact_id":               result.ArtifactID,
		"template_spec_fingerprint": result.TemplateSpecFingerprint,
		"source_image_digest":       result.SourceImageDigest,
		"artifact_status":           ArtifactStatusReady,
		"status":                    JobStatusRunning,
		"phase":                     JobPhaseDistributing,
		"progress":                  70,
		"error_message":             "",
	}
	if !reflect.DeepEqual(updates[0], want) {
		t.Fatalf("job update = %#v, want %#v", updates[0], want)
	}
}

func TestPrepareRemoteBuildCallbackLeavesBuiltOnRegistrationError(t *testing.T) {
	var updates []map[string]any
	patchRemoteBuildPrepareDependencies(t, &updates, JobStatusBuilt, JobPhaseReady)
	result := validRemoteBuildResultForCallbackTest(t)
	wantErr := errors.New("invalid connection")
	registrar := func(context.Context, *types.CreateTemplateFromImageReq, *RemoteBuildResult) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, bool, error) {
		return nil, nil, false, wantErr
	}

	continuation, err := prepareTemplateImageJobAfterRemoteBuild(context.Background(), "job-1", result, registrar, true)
	if continuation != nil {
		t.Fatal("continuation must be nil when registration fails")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if len(updates) != 0 {
		t.Fatalf("registration error must not claim distribution, got updates %#v", updates)
	}
	var saved models.TemplateImageJob
	require.NoError(t, store.db.Where("job_id = ?", "job-1").First(&saved).Error)
	require.Equal(t, JobStatusBuilt, saved.Status)
	require.Equal(t, JobPhaseReady, saved.Phase)
	require.JSONEq(t, `{"artifact_id":"rfs-callback"}`, saved.ResultJSON)
	require.Contains(t, saved.ErrorMessage, wantErr.Error())
}

func TestPrepareRemoteBuildCallbackSkipsAlreadyClaimedDistribution(t *testing.T) {
	var updates []map[string]any
	patchRemoteBuildPrepareDependencies(t, &updates, JobStatusRunning, JobPhaseDistributing)
	result := validRemoteBuildResultForCallbackTest(t)
	called := false
	registrar := func(context.Context, *types.CreateTemplateFromImageReq, *RemoteBuildResult) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, bool, error) {
		called = true
		return nil, nil, false, nil
	}

	continuation, err := prepareTemplateImageJobAfterRemoteBuild(context.Background(), "job-1", result, registrar, true)
	if err != nil {
		t.Fatalf("prepareTemplateImageJobAfterRemoteBuild() error = %v", err)
	}
	if continuation != nil {
		t.Fatal("duplicate callback must not create a continuation")
	}
	if called {
		t.Fatal("duplicate callback must not register the artifact again")
	}
	if len(updates) != 0 {
		t.Fatalf("duplicate callback changed job: %#v", updates)
	}
}
