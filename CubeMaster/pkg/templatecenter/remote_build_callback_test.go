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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
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

	oldDB := store.db
	store.db = &gorm.DB{}
	t.Cleanup(func() { store.db = oldDB })
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
		t.Fatalf("registration error must leave the BUILT job untouched, got updates %#v", updates)
	}
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

// patchRemoteBuildRootfsFormatPrepare is the rootfs-format-aware variant of
// patchRemoteBuildPrepareDependencies: it lets the caller pin the decoded
// request's RootfsFormat (so the mixed-version guard sees it) and captures
// updateTemplateImageJob calls, which the guard issues on the failure path
// (the BUILT-job success path only touches claimTemplateImageJobDistribution).
func patchRemoteBuildRootfsFormatPrepare(t *testing.T, updates *[]map[string]any, jobUpdates *[]map[string]any, rootfsFormat string) {
	t.Helper()
	patches := gomonkey.NewPatches()
	patches.ApplyFunc(getTemplateImageJobRecordByID, func(context.Context, string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{JobID: "job-1", TemplateID: "tpl-1", Status: JobStatusBuilt, Phase: JobPhaseReady}, nil
	})
	patches.ApplyFunc(unmarshalTemplateImageJobRequest, func(string) (*types.CreateTemplateFromImageReq, error) {
		return &types.CreateTemplateFromImageReq{TemplateID: "tpl-1", RootfsFormat: rootfsFormat}, nil
	})
	patches.ApplyFunc(claimTemplateImageJobDistribution, func(_ context.Context, _ string, values map[string]any) (bool, error) {
		cloned := make(map[string]any, len(values))
		for key, value := range values {
			cloned[key] = value
		}
		*updates = append(*updates, cloned)
		return true, nil
	})
	patches.ApplyFunc(updateTemplateImageJob, func(_ context.Context, _ string, values map[string]any) error {
		cloned := make(map[string]any, len(values))
		for key, value := range values {
			cloned[key] = value
		}
		*jobUpdates = append(*jobUpdates, cloned)
		return nil
	})
	t.Cleanup(patches.Reset)

	oldDB := store.db
	store.db = &gorm.DB{}
	t.Cleanup(func() { store.db = oldDB })
}

// TestPrepareRemoteBuildCallbackFailsErofsRequestLegacyTCReportsExt4 covers the
// mixed-version rollout guard: an older CubeTemplateCenter (predating erofs
// step 3/4) silently builds ext4 and omits rootfs_format from the BUILT report.
// An erofs request must fail loudly here — before registration — rather than
// recording an (erofs fingerprint, ext4 format) artifact that every later
// identical erofs request would then reuse forever.
func TestPrepareRemoteBuildCallbackFailsErofsRequestLegacyTCReportsExt4(t *testing.T) {
	var claimUpdates, jobUpdates []map[string]any
	// req.RootfsFormat=erofs (request wants erofs); result.RootfsFormat="" (old
	// TC does not report the field) -> NormalizeRootfsFormat("")=="ext4".
	patchRemoteBuildRootfsFormatPrepare(t, &claimUpdates, &jobUpdates, config.RootfsFormatErofs)
	result := validRemoteBuildResultForCallbackTest(t)
	result.RootfsFormat = "" // legacy TC: no rootfs_format in the BUILT report
	registered := false
	registrar := func(context.Context, *types.CreateTemplateFromImageReq, *RemoteBuildResult) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, bool, error) {
		registered = true
		return nil, nil, false, nil
	}

	continuation, err := prepareTemplateImageJobAfterRemoteBuild(context.Background(), "job-1", result, registrar, true)
	if err == nil {
		t.Fatal("expected a rootfs_format mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "rootfs_format mismatch") {
		t.Fatalf("error = %q, want it to contain \"rootfs_format mismatch\"", err.Error())
	}
	if !strings.Contains(err.Error(), "upgrade CubeTemplateCenter") {
		t.Fatalf("error = %q, want it to name CubeTemplateCenter upgrade", err.Error())
	}
	if continuation != nil {
		t.Fatal("continuation must be nil on a format mismatch")
	}
	if registered {
		t.Fatal("registrar must NOT be called when the format guard rejects the build")
	}
	if len(claimUpdates) != 0 {
		t.Fatalf("distribution must not be claimed on a format mismatch, got claim updates %#v", claimUpdates)
	}
	if len(jobUpdates) != 1 {
		t.Fatalf("expected exactly one FAILED job update, got %d", len(jobUpdates))
	}
	got := jobUpdates[0]
	if got["status"] != JobStatusFailed {
		t.Errorf("job status = %v, want %q", got["status"], JobStatusFailed)
	}
	if got["phase"] != JobPhaseBuildingErofs {
		t.Errorf("job phase = %v, want %q (request's format is authoritative)", got["phase"], JobPhaseBuildingErofs)
	}
	if got["artifact_id"] != result.ArtifactID {
		t.Errorf("job artifact_id = %v, want %q", got["artifact_id"], result.ArtifactID)
	}
	if got["artifact_status"] != ArtifactStatusFailed {
		t.Errorf("job artifact_status = %v, want %q", got["artifact_status"], ArtifactStatusFailed)
	}
	if msg, _ := got["error_message"].(string); !strings.Contains(msg, "rootfs_format mismatch") {
		t.Errorf("job error_message = %q, want it to contain \"rootfs_format mismatch\"", msg)
	}
}

// TestPrepareRemoteBuildCallbackPassesExt4RequestLegacyTCReportsEmpty locks
// the backward-compatible case: an ext4 request (the historical default) against
// the same older CubeTemplateCenter that omits rootfs_format must still succeed.
// Both sides normalize to ext4, so the guard accepts it and registration runs.
func TestPrepareRemoteBuildCallbackPassesExt4RequestLegacyTCReportsEmpty(t *testing.T) {
	var claimUpdates, jobUpdates []map[string]any
	// req.RootfsFormat="" (ext4 default) and result.RootfsFormat="" (legacy TC);
	// both NormalizeRootfsFormat to "ext4", so the guard passes.
	patchRemoteBuildRootfsFormatPrepare(t, &claimUpdates, &jobUpdates, "")
	result := validRemoteBuildResultForCallbackTest(t)
	result.RootfsFormat = ""
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
		t.Fatalf("ext4 request + legacy TC must succeed, got error = %v", err)
	}
	if !registered {
		t.Fatal("artifact must be registered for an ext4 request against a legacy TC")
	}
	if continuation == nil {
		t.Fatal("continuation must not be nil for an accepted ext4 build")
	}
	if len(jobUpdates) != 0 {
		t.Fatalf("ext4 success path must not write a FAILED job update, got %#v", jobUpdates)
	}
	if len(claimUpdates) != 1 {
		t.Fatalf("distribution must be claimed once on success, got %d updates", len(claimUpdates))
	}
}
