// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
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
