// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"testing"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

// deleteFlowRecorder captures which parts of the delete flow ran, so ordering
// properties (notably "in-use is evaluated before any status rewrite") can be
// asserted without a database.
type deleteFlowRecorder struct {
	stages      map[string]bool
	inUseCalled bool
	failedWork  []string
}

// stubDeleteFlow replaces every seam of deleteTemplateWithTargets. inUse drives
// the in-use verdict; all cleanup stages succeed.
func stubDeleteFlow(t *testing.T, inUse bool) *deleteFlowRecorder {
	t.Helper()
	rec := &deleteFlowRecorder{stages: map[string]bool{}}

	origReplica := runReplicaCleanup
	origArtifact := runArtifactCleanup
	origMetadata := runMetadataCleanup
	origJob := runTemplateJobCleanup
	origInUse := runInUseCheck
	origFail := runFailActiveWork
	t.Cleanup(func() {
		runReplicaCleanup = origReplica
		runArtifactCleanup = origArtifact
		runMetadataCleanup = origMetadata
		runTemplateJobCleanup = origJob
		runInUseCheck = origInUse
		runFailActiveWork = origFail
	})

	runReplicaCleanup = func(context.Context, string, []templateCleanupLocator, string) error {
		rec.stages["replica"] = true
		return nil
	}
	runArtifactCleanup = func(context.Context, string, *templateCleanupTargets) error {
		rec.stages["artifact"] = true
		return nil
	}
	runMetadataCleanup = func(context.Context, string) error {
		rec.stages["metadata"] = true
		return nil
	}
	runTemplateJobCleanup = func(context.Context, string) error {
		rec.stages["job"] = true
		return nil
	}
	runInUseCheck = func(context.Context, string, string) (bool, error) {
		rec.inUseCalled = true
		return inUse, nil
	}
	runFailActiveWork = func(_ context.Context, templateID string, _ *templateCleanupTargets) error {
		rec.failedWork = append(rec.failedWork, templateID)
		return nil
	}
	return rec
}

func TestActiveWorkBlocker(t *testing.T) {
	cases := []struct {
		name       string
		targets    *templateCleanupTargets
		wantBlock  bool
		wantReason string
	}{
		{
			name:    "nil targets do not block",
			targets: nil,
		},
		{
			name:    "no jobs and no definition do not block",
			targets: &templateCleanupTargets{},
		},
		{
			name: "pending job blocks",
			targets: &templateCleanupTargets{
				Jobs: []models.TemplateImageJob{{Status: JobStatusPending}},
			},
			wantBlock:  true,
			wantReason: "build job is still active",
		},
		{
			name: "running job blocks",
			targets: &templateCleanupTargets{
				Jobs: []models.TemplateImageJob{{Status: JobStatusRunning}},
			},
			wantBlock:  true,
			wantReason: "build job is still active",
		},
		{
			name: "lowercase status still blocks",
			targets: &templateCleanupTargets{
				Jobs: []models.TemplateImageJob{{Status: "running"}},
			},
			wantBlock:  true,
			wantReason: "build job is still active",
		},
		{
			// BUILT is the remote-build handoff state: the ext4 exists and the
			// job is waiting for distribution. It is NOT treated as active.
			name: "built job does not block",
			targets: &templateCleanupTargets{
				Jobs: []models.TemplateImageJob{{Status: JobStatusBuilt}},
			},
		},
		{
			name: "terminal jobs do not block",
			targets: &templateCleanupTargets{
				Jobs: []models.TemplateImageJob{
					{Status: JobStatusFailed},
					{Status: JobStatusReady},
				},
			},
		},
		{
			name: "pending definition blocks",
			targets: &templateCleanupTargets{
				Definition: &models.TemplateDefinition{Status: StatusPending},
			},
			wantBlock:  true,
			wantReason: "definition creation is still active",
		},
		{
			name: "creating definition blocks",
			targets: &templateCleanupTargets{
				Definition: &models.TemplateDefinition{Status: StatusCreating},
			},
			wantBlock:  true,
			wantReason: "definition creation is still active",
		},
		{
			name: "ready definition does not block",
			targets: &templateCleanupTargets{
				Definition: &models.TemplateDefinition{Status: StatusReady},
			},
		},
		{
			// An active job is reported ahead of the definition so the message
			// names the thing that is actually running.
			name: "job takes precedence over definition",
			targets: &templateCleanupTargets{
				Jobs:       []models.TemplateImageJob{{Status: JobStatusRunning}},
				Definition: &models.TemplateDefinition{Status: StatusCreating},
			},
			wantBlock:  true,
			wantReason: "build job is still active",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := activeWorkBlocker(tc.targets, "tpl-x")
			if !tc.wantBlock {
				if err != nil {
					t.Fatalf("expected no blocker, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a blocker")
			}
			if !errors.Is(err, ErrTemplateAttemptInProgress) {
				t.Fatalf("blocker must wrap ErrTemplateAttemptInProgress, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("blocker %q does not mention %q", err.Error(), tc.wantReason)
			}
		})
	}
}

// Without force, an active job is still refused: force must be opt-in.
func TestDeleteTemplateWithTargetsWithoutForceStillRejectsActiveJob(t *testing.T) {
	rec := stubDeleteFlow(t, false)

	err := deleteTemplateWithTargets(context.Background(), "tpl-active", &templateCleanupTargets{
		Jobs:         []models.TemplateImageJob{{TemplateID: "tpl-active", Status: JobStatusRunning}},
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	}, DeleteTemplateOptions{})

	if !errors.Is(err, ErrTemplateAttemptInProgress) {
		t.Fatalf("expected ErrTemplateAttemptInProgress, got %v", err)
	}
	if len(rec.failedWork) != 0 {
		t.Fatalf("non-forced delete must not terminate in-flight work, got %v", rec.failedWork)
	}
	if len(rec.stages) != 0 {
		t.Fatalf("non-forced delete must not run cleanup, got %v", rec.stages)
	}
}

// With force, the active job is terminated first and cleanup proceeds.
func TestDeleteTemplateWithTargetsForceClearsActiveJob(t *testing.T) {
	rec := stubDeleteFlow(t, false)

	err := deleteTemplateWithTargets(context.Background(), "tpl-stuck", &templateCleanupTargets{
		Jobs: []models.TemplateImageJob{
			{TemplateID: "tpl-stuck", Status: JobStatusPending, NodeIP: "10.0.0.9"},
		},
		Locators:     []templateCleanupLocator{{NodeIP: "10.0.0.9"}},
		ArtifactIDs:  map[string]struct{}{},
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	}, DeleteTemplateOptions{Force: true})

	if err != nil {
		t.Fatalf("force delete failed: %v", err)
	}
	if len(rec.failedWork) != 1 || rec.failedWork[0] != "tpl-stuck" {
		t.Fatalf("expected in-flight work to be terminated, got %v", rec.failedWork)
	}
	for _, stage := range []string{"replica", "artifact", "metadata", "job"} {
		if !rec.stages[stage] {
			t.Fatalf("force delete must run %s cleanup, ran=%v", stage, rec.stages)
		}
	}
}

// The critical safety property: force must NOT delete a template a live sandbox
// still uses, and must not have rewritten any status before finding that out.
func TestDeleteTemplateWithTargetsForceStillHonoursInUse(t *testing.T) {
	rec := stubDeleteFlow(t, true)

	err := deleteTemplateWithTargets(context.Background(), "tpl-in-use", &templateCleanupTargets{
		Definition:   &models.TemplateDefinition{TemplateID: "tpl-in-use", Status: StatusCreating},
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	}, DeleteTemplateOptions{Force: true})

	if !errors.Is(err, ErrTemplateInUse) {
		t.Fatalf("force must not override the in-use check, got %v", err)
	}
	if !rec.inUseCalled {
		t.Fatal("the in-use check must run on the force path")
	}
	// If the definition had been failed first, shouldCheckInUse would have
	// returned false and the in-use check would have been skipped entirely.
	if len(rec.failedWork) != 0 {
		t.Fatalf("in-use check must run before any status rewrite, got %v", rec.failedWork)
	}
	if len(rec.stages) != 0 {
		t.Fatalf("an in-use template must not be cleaned up, got %v", rec.stages)
	}
}

// A FAILED definition already skips the in-use check today; force must not
// change that pre-existing behaviour.
func TestDeleteTemplateWithTargetsForceOnFailedDefinitionSkipsInUse(t *testing.T) {
	rec := stubDeleteFlow(t, true)

	err := deleteTemplateWithTargets(context.Background(), "tpl-failed", &templateCleanupTargets{
		Definition:   &models.TemplateDefinition{TemplateID: "tpl-failed", Status: StatusFailed},
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	}, DeleteTemplateOptions{Force: true})

	if err != nil {
		t.Fatalf("force delete failed: %v", err)
	}
	if rec.inUseCalled {
		t.Fatal("a FAILED definition must not trigger the in-use check")
	}
	if !rec.stages["metadata"] {
		t.Fatalf("cleanup must run, ran=%v", rec.stages)
	}
}

// Force also clears the "no node locator" dead end, which is otherwise
// permanently undeletable.
func TestDeleteTemplateWithTargetsForceClearsMissingLocator(t *testing.T) {
	rec := stubDeleteFlow(t, false)

	targets := &templateCleanupTargets{
		Jobs: []models.TemplateImageJob{
			{TemplateID: "tpl-no-locator", Status: JobStatusFailed, NodeID: "node-a"},
		},
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	}
	if !targets.requiresCleanupLocator() {
		t.Fatal("test setup should require a cleanup locator")
	}

	// Without force this is rejected.
	if err := deleteTemplateWithTargets(context.Background(), "tpl-no-locator", targets,
		DeleteTemplateOptions{}); !errors.Is(err, ErrTemplateCleanupLocatorMissing) {
		t.Fatalf("expected ErrTemplateCleanupLocatorMissing, got %v", err)
	}

	if err := deleteTemplateWithTargets(context.Background(), "tpl-no-locator", targets,
		DeleteTemplateOptions{Force: true}); err != nil {
		t.Fatalf("force delete failed: %v", err)
	}
	if !rec.stages["metadata"] || !rec.stages["job"] {
		t.Fatalf("force delete must still clean metadata and jobs, ran=%v", rec.stages)
	}
	// A locator-less template has no in-flight work, so nothing is terminated.
	if len(rec.failedWork) != 0 {
		t.Fatalf("no active work to terminate, got %v", rec.failedWork)
	}
}

// Force on a template with nothing to clean up is still NotFound: force does not
// invent state.
func TestDeleteTemplateWithTargetsForceOnEmptyStateIsNotFound(t *testing.T) {
	rec := stubDeleteFlow(t, false)
	err := deleteTemplateWithTargets(context.Background(), "tpl-none",
		&templateCleanupTargets{}, DeleteTemplateOptions{Force: true})
	if !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("expected ErrTemplateNotFound, got %v", err)
	}
	if len(rec.stages) != 0 {
		t.Fatalf("nothing should be cleaned up, got %v", rec.stages)
	}
}

// DeleteTemplate must keep its original non-forced behaviour.
func TestDeleteTemplateDefaultsToNonForced(t *testing.T) {
	rec := stubDeleteFlow(t, false)

	err := deleteTemplateWithTargets(context.Background(), "tpl-default", &templateCleanupTargets{
		Jobs:         []models.TemplateImageJob{{TemplateID: "tpl-default", Status: JobStatusPending}},
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	}, DeleteTemplateOptions{})

	if !errors.Is(err, ErrTemplateAttemptInProgress) {
		t.Fatalf("default options must behave as non-forced, got %v", err)
	}
	if len(rec.failedWork) != 0 {
		t.Fatalf("default options must not terminate work, got %v", rec.failedWork)
	}
}
