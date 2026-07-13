// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

func TestDeleteTemplateWithTargetsAllowsJobOnlyCleanup(t *testing.T) {
	origReplicaCleanup := runReplicaCleanup
	origArtifactCleanup := runArtifactCleanup
	origMetadataCleanup := runMetadataCleanup
	origJobCleanup := runTemplateJobCleanup
	t.Cleanup(func() {
		runReplicaCleanup = origReplicaCleanup
		runArtifactCleanup = origArtifactCleanup
		runMetadataCleanup = origMetadataCleanup
		runTemplateJobCleanup = origJobCleanup
	})

	var replicaCalled, artifactCalled, metadataCalled, jobCalled bool
	runReplicaCleanup = func(ctx context.Context, templateID string, locators []templateCleanupLocator) error {
		replicaCalled = true
		// v4: locators are deduplicated by (NodeID|NodeIP); SnapshotPath is
		// no longer part of the identity or the cleanup payload.
		if len(locators) != 1 || locators[0].NodeIP != "10.0.0.8" {
			t.Fatalf("unexpected locators: %+v", locators)
		}
		// v5: SnapshotPath / Objects no longer exist on templateCleanupLocator —
		// cubelet is the sole authority and resolves both from its local
		// catalog. The locator now contains only (NodeID, NodeIP).
		return nil
	}
	runArtifactCleanup = func(ctx context.Context, templateID string, targets *templateCleanupTargets) error {
		artifactCalled = true
		if targets == nil {
			t.Fatal("expected cleanup targets")
		}
		if _, ok := targets.ArtifactIDs["artifact-1"]; !ok {
			t.Fatalf("artifact locator missing: %+v", targets.ArtifactIDs)
		}
		if targets.InstanceType != cubeboxv1.InstanceType_cubebox.String() {
			t.Fatalf("unexpected instance type: %s", targets.InstanceType)
		}
		return nil
	}
	runMetadataCleanup = func(ctx context.Context, templateID string) error {
		metadataCalled = true
		return nil
	}
	runTemplateJobCleanup = func(ctx context.Context, templateID string) error {
		jobCalled = true
		return nil
	}

	err := deleteTemplateWithTargets(context.Background(), "tpl-job-only", &templateCleanupTargets{
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
		Jobs: []models.TemplateImageJob{
			{
				TemplateID:   "tpl-job-only",
				NodeIP:       "10.0.0.8",
				SnapshotPath: "/tmp/snap",
				ArtifactID:   "artifact-1",
			},
		},
		Locators: []templateCleanupLocator{
			{NodeIP: "10.0.0.8"},
		},
		ArtifactIDs: map[string]struct{}{"artifact-1": {}},
	})
	if err != nil {
		t.Fatalf("deleteTemplateWithTargets failed: %v", err)
	}
	if !replicaCalled || !artifactCalled || !metadataCalled || !jobCalled {
		t.Fatalf("expected all cleanup stages to run, got replica=%v artifact=%v metadata=%v job=%v", replicaCalled, artifactCalled, metadataCalled, jobCalled)
	}
}

func TestDeleteTemplateWithTargetsRejectsActiveJobs(t *testing.T) {
	err := deleteTemplateWithTargets(context.Background(), "tpl-active", &templateCleanupTargets{
		Jobs: []models.TemplateImageJob{
			{TemplateID: "tpl-active", Status: JobStatusPending},
		},
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	})
	if !errors.Is(err, ErrTemplateAttemptInProgress) {
		t.Fatalf("expected ErrTemplateAttemptInProgress, got %v", err)
	}
}

func TestDeleteTemplateWithTargetsRejectsPendingDefinitionBuild(t *testing.T) {
	err := deleteTemplateWithTargets(context.Background(), "tpl-pending", &templateCleanupTargets{
		Definition:   &models.TemplateDefinition{TemplateID: "tpl-pending", Status: StatusPending},
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	})
	if !errors.Is(err, ErrTemplateAttemptInProgress) {
		t.Fatalf("expected ErrTemplateAttemptInProgress for pending definition, got %v", err)
	}
}

func TestDeleteTemplateWithTargetsRejectsMissingCleanupLocator(t *testing.T) {
	// v4: a job that points at a host (node id / ip) but has no resolvable
	// locator entry must be rejected so we never silently drop cubelet-side
	// artifacts. SnapshotPath is no longer authoritative; node identity is.
	err := deleteTemplateWithTargets(context.Background(), "tpl-missing-locator", &templateCleanupTargets{
		Jobs: []models.TemplateImageJob{
			{TemplateID: "tpl-missing-locator", Status: JobStatusFailed, NodeID: "node-a"},
		},
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	})
	if !errors.Is(err, ErrTemplateCleanupLocatorMissing) {
		t.Fatalf("expected ErrTemplateCleanupLocatorMissing, got %v", err)
	}
}

func TestDeleteTemplateWithTargetsAllowsOrphanedJobCleanup(t *testing.T) {
	// v4: a job with no NodeID/NodeIP is an orphaned record with nothing to
	// clean up on any cubelet. Deletion should succeed without a locator.
	origReplicaCleanup := runReplicaCleanup
	origArtifactCleanup := runArtifactCleanup
	origMetadataCleanup := runMetadataCleanup
	origJobCleanup := runTemplateJobCleanup
	t.Cleanup(func() {
		runReplicaCleanup = origReplicaCleanup
		runArtifactCleanup = origArtifactCleanup
		runMetadataCleanup = origMetadataCleanup
		runTemplateJobCleanup = origJobCleanup
	})

	var metadataCalled, jobCalled bool
	runReplicaCleanup = func(ctx context.Context, templateID string, locators []templateCleanupLocator) error {
		if len(locators) != 0 {
			t.Fatalf("expected no locators for orphaned job, got %+v", locators)
		}
		return nil
	}
	runArtifactCleanup = func(ctx context.Context, templateID string, targets *templateCleanupTargets) error {
		return nil
	}
	runMetadataCleanup = func(ctx context.Context, templateID string) error {
		metadataCalled = true
		return nil
	}
	runTemplateJobCleanup = func(ctx context.Context, templateID string) error {
		jobCalled = true
		return nil
	}

	err := deleteTemplateWithTargets(context.Background(), "tpl-orphaned-job", &templateCleanupTargets{
		Jobs: []models.TemplateImageJob{
			// No NodeID, NodeIP, SnapshotPath, or ArtifactID — purely orphaned.
			{TemplateID: "tpl-orphaned-job", Status: JobStatusFailed},
		},
		ArtifactIDs:  map[string]struct{}{},
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	})
	if err != nil {
		t.Fatalf("orphaned job cleanup should succeed, got: %v", err)
	}
	if !metadataCalled || !jobCalled {
		t.Fatalf("expected metadata and job cleanup to run, got metadata=%v job=%v", metadataCalled, jobCalled)
	}
}

func TestDeleteTemplateWithTargetsAllowsArtifactOnlyCleanupWithoutLocator(t *testing.T) {
	origReplicaCleanup := runReplicaCleanup
	origArtifactCleanup := runArtifactCleanup
	origMetadataCleanup := runMetadataCleanup
	origJobCleanup := runTemplateJobCleanup
	t.Cleanup(func() {
		runReplicaCleanup = origReplicaCleanup
		runArtifactCleanup = origArtifactCleanup
		runMetadataCleanup = origMetadataCleanup
		runTemplateJobCleanup = origJobCleanup
	})

	var artifactCalled, metadataCalled, jobCalled bool
	runReplicaCleanup = func(ctx context.Context, templateID string, locators []templateCleanupLocator) error {
		if len(locators) != 0 {
			t.Fatalf("expected no locators, got %+v", locators)
		}
		return nil
	}
	runArtifactCleanup = func(ctx context.Context, templateID string, targets *templateCleanupTargets) error {
		artifactCalled = true
		if targets == nil {
			t.Fatal("expected cleanup targets")
		}
		if _, ok := targets.ArtifactIDs["artifact-only"]; !ok {
			t.Fatalf("expected artifact cleanup target, got %+v", targets.ArtifactIDs)
		}
		return nil
	}
	runMetadataCleanup = func(ctx context.Context, templateID string) error {
		metadataCalled = true
		return nil
	}
	runTemplateJobCleanup = func(ctx context.Context, templateID string) error {
		jobCalled = true
		return nil
	}

	err := deleteTemplateWithTargets(context.Background(), "tpl-artifact-only", &templateCleanupTargets{
		Jobs: []models.TemplateImageJob{
			{TemplateID: "tpl-artifact-only", Status: JobStatusFailed, ArtifactID: "artifact-only"},
		},
		ArtifactIDs:  map[string]struct{}{"artifact-only": {}},
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	})
	if err != nil {
		t.Fatalf("deleteTemplateWithTargets failed: %v", err)
	}
	if !artifactCalled || !metadataCalled || !jobCalled {
		t.Fatalf("expected artifact/metadata/job cleanup to run, got artifact=%v metadata=%v job=%v", artifactCalled, metadataCalled, jobCalled)
	}
}

func TestDeleteTemplateWithTargetsPreservesJobsAfterPartialFailure(t *testing.T) {
	origReplicaCleanup := runReplicaCleanup
	origArtifactCleanup := runArtifactCleanup
	origMetadataCleanup := runMetadataCleanup
	origJobCleanup := runTemplateJobCleanup
	t.Cleanup(func() {
		runReplicaCleanup = origReplicaCleanup
		runArtifactCleanup = origArtifactCleanup
		runMetadataCleanup = origMetadataCleanup
		runTemplateJobCleanup = origJobCleanup
	})

	replicaErr := errors.New("cubelet temporarily unavailable")
	var metadataCalled, jobCalled, invalidated bool
	runReplicaCleanup = func(ctx context.Context, templateID string, locators []templateCleanupLocator) error {
		return replicaErr
	}
	runArtifactCleanup = func(ctx context.Context, templateID string, targets *templateCleanupTargets) error {
		return nil
	}
	runMetadataCleanup = func(ctx context.Context, templateID string) error {
		metadataCalled = true
		return nil
	}
	runTemplateJobCleanup = func(ctx context.Context, templateID string) error {
		jobCalled = true
		return nil
	}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {
		invalidated = true
	})

	err := deleteTemplateWithTargets(context.Background(), "tpl-partial", &templateCleanupTargets{
		Jobs:         []models.TemplateImageJob{{TemplateID: "tpl-partial"}},
		Locators:     []templateCleanupLocator{{NodeIP: "10.0.0.8"}},
		ArtifactIDs:  map[string]struct{}{},
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	})
	if !errors.Is(err, replicaErr) {
		t.Fatalf("expected replica cleanup error, got %v", err)
	}
	if metadataCalled {
		t.Fatal("metadata cleanup should not run after partial failure")
	}
	if jobCalled {
		t.Fatal("job cleanup should be skipped so locator metadata can be retried")
	}
	if invalidated {
		t.Fatal("cache should remain intact after partial failure")
	}
}

func TestDeleteTemplateWithTargetsPreservesMetadataAfterArtifactFailure(t *testing.T) {
	origReplicaCleanup := runReplicaCleanup
	origArtifactCleanup := runArtifactCleanup
	origMetadataCleanup := runMetadataCleanup
	origJobCleanup := runTemplateJobCleanup
	t.Cleanup(func() {
		runReplicaCleanup = origReplicaCleanup
		runArtifactCleanup = origArtifactCleanup
		runMetadataCleanup = origMetadataCleanup
		runTemplateJobCleanup = origJobCleanup
	})

	artifactErr := errors.New("artifact delete failed")
	var metadataCalled, jobCalled, invalidated bool
	runReplicaCleanup = func(ctx context.Context, templateID string, locators []templateCleanupLocator) error {
		return nil
	}
	runArtifactCleanup = func(ctx context.Context, templateID string, targets *templateCleanupTargets) error {
		return artifactErr
	}
	runMetadataCleanup = func(ctx context.Context, templateID string) error {
		metadataCalled = true
		return nil
	}
	runTemplateJobCleanup = func(ctx context.Context, templateID string) error {
		jobCalled = true
		return nil
	}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {
		invalidated = true
	})

	err := deleteTemplateWithTargets(context.Background(), "tpl-artifact-failure", &templateCleanupTargets{
		Jobs:         []models.TemplateImageJob{{TemplateID: "tpl-artifact-failure", ArtifactID: "artifact-1"}},
		ArtifactIDs:  map[string]struct{}{"artifact-1": {}},
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	})
	if !errors.Is(err, artifactErr) {
		t.Fatalf("expected artifact cleanup error, got %v", err)
	}
	if metadataCalled {
		t.Fatal("metadata cleanup should not run after artifact cleanup failure")
	}
	if jobCalled {
		t.Fatal("job cleanup should be skipped after artifact cleanup failure")
	}
	if invalidated {
		t.Fatal("cache should remain intact after artifact cleanup failure")
	}
}

func TestCleanupTemplateReplicasWithLocatorsIgnoresNotFound(t *testing.T) {
	origCleanupTemplateOnCubelet := cleanupTemplateOnCubelet
	origGetCubeletAddrForDelete := getCubeletAddrForDelete
	t.Cleanup(func() {
		cleanupTemplateOnCubelet = origCleanupTemplateOnCubelet
		getCubeletAddrForDelete = origGetCubeletAddrForDelete
	})

	getCubeletAddrForDelete = func(hostIP string) string { return hostIP + ":9000" }
	cleanupTemplateOnCubelet = func(ctx context.Context, calleeEp string, req *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error) {
		// v4: master must not send Objects or SnapshotPath. Cubelet derives
		// both from its local catalog (with deterministic fallback).
		if got := req.GetObjects(); len(got) != 0 {
			t.Fatalf("v4: cleanup request must not carry Objects; got %d", len(got))
		}
		if got := req.GetSnapshotPath(); got != "" {
			t.Fatalf("v4: cleanup request must not carry SnapshotPath; got %q", got)
		}
		return &cubeboxv1.CleanupTemplateResponse{
			Ret: &errorcodev1.Ret{
				RetCode: errorcodev1.ErrorCode_Unknown,
				RetMsg:  "snapshot path not found",
			},
		}, nil
	}

	if err := cleanupTemplateReplicasWithLocators(context.Background(), "tpl-1", []templateCleanupLocator{{NodeIP: "10.0.0.8"}}); err != nil {
		t.Fatalf("expected not-found cleanup to be ignored, got %v", err)
	}
}

func TestIsIgnorableTemplateCleanupMessage(t *testing.T) {
	cases := []string{
		"snapshot path not found",
		"snapshot path does not exist",
		"no such file or directory",
	}
	for _, tc := range cases {
		if !isIgnorableTemplateCleanupMessage(tc) {
			t.Fatalf("expected %q to be ignorable", tc)
		}
	}
	if isIgnorableTemplateCleanupMessage("permission denied") {
		t.Fatal("permission denied should not be ignored")
	}
	if isIgnorableTemplateCleanupMessage("node not found") {
		t.Fatal("node not found should not be ignored")
	}
}

func TestIsIgnorableTemplateCleanupError(t *testing.T) {
	if !isIgnorableTemplateCleanupError(errors.New("snapshot path not found")) {
		t.Fatal("snapshot path not found should be ignored")
	}
	if isIgnorableTemplateCleanupError(errors.New("rpc timeout")) {
		t.Fatal("rpc timeout should not be ignored")
	}
}

func TestIsIgnorableArtifactDeleteMessage(t *testing.T) {
	if !isIgnorableArtifactDeleteMessage("image not found") {
		t.Fatal("image not found should be ignored")
	}
	if !isIgnorableArtifactDeleteMessage("no such image") {
		t.Fatal("no such image should be ignored")
	}
	if isIgnorableArtifactDeleteMessage("node not found") {
		t.Fatal("node not found should not be ignored")
	}
}

func TestIsIgnorableArtifactDeleteError(t *testing.T) {
	if !isIgnorableArtifactDeleteError(errors.New("image does not exist")) {
		t.Fatal("image does not exist should be ignored")
	}
	if isIgnorableArtifactDeleteError(errors.New("registry not found for node")) {
		t.Fatal("generic not found should not be ignored")
	}
}
