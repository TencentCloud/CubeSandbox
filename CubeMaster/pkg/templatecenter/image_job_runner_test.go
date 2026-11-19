// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// TestMarkDefinitionFailedOnPersistErr_PublishesFailed verifies that when
// replica persistence fails, the template definition is published as FAILED
// with a sanitized error message, and the returned error is the original
// persistErr when the publish succeeds.
func TestMarkDefinitionFailedOnPersistErr_PublishesFailed(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	persistErr := errors.New("upsert template replica fail: invalid connection")

	var publishedStatus, publishedLastError string
	patches.ApplyFunc(publishCreateFailedIfJobActive,
		func(ctx context.Context, templateID, jobID, lastError string) (bool, error) {
			publishedStatus = StatusFailed
			publishedLastError = lastError
			return true, nil
		})
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {})

	userMsg, err := markDefinitionFailedOnPersistErr(
		context.Background(), "tpl-1", "job-1", 2, persistErr,
	)

	// Definition must be published as FAILED.
	if publishedStatus != StatusFailed {
		t.Fatalf("published status = %q, want %q", publishedStatus, StatusFailed)
	}
	// The error written to the definition must be sanitized: no raw DB error.
	if strings.Contains(publishedLastError, "invalid connection") {
		t.Fatalf("last_error leaks raw DB error: %q", publishedLastError)
	}
	if !strings.Contains(publishedLastError, "up to 2 target node(s)") {
		t.Fatalf("last_error missing node count: %q", publishedLastError)
	}
	if !strings.Contains(publishedLastError, "job-1") {
		t.Fatalf("last_error missing job ID: %q", publishedLastError)
	}
	// Returned error is the original persistErr (publish succeeded).
	if !errors.Is(err, persistErr) {
		t.Fatalf("returned error %v does not wrap persistErr", err)
	}
	// The job-row message is the same sanitized text as the definition.
	if userMsg != publishedLastError {
		t.Fatalf("userMsg = %q, want definition last_error %q", userMsg, publishedLastError)
	}
}

// TestMarkDefinitionFailedOnPersistErr_TemplateNotFound verifies that when
// publishCreateFailedIfJobActive returns ErrTemplateNotFound (concurrent
// delete removed the definition row), the update error is suppressed and
// the returned error contains only persistErr.
func TestMarkDefinitionFailedOnPersistErr_TemplateNotFound(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	persistErr := errors.New("upsert template replica fail: invalid connection")

	patches.ApplyFunc(publishCreateFailedIfJobActive,
		func(ctx context.Context, templateID, jobID, lastError string) (bool, error) {
			return false, ErrTemplateNotFound
		})
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {})

	userMsg, err := markDefinitionFailedOnPersistErr(
		context.Background(), "tpl-gone", "job-2", 1, persistErr,
	)

	// ErrTemplateNotFound must be suppressed.
	if errors.Is(err, ErrTemplateNotFound) {
		t.Fatal("returned error should not wrap ErrTemplateNotFound")
	}
	// Only persistErr should remain.
	if !errors.Is(err, persistErr) {
		t.Fatalf("returned error %v does not wrap persistErr", err)
	}
	if strings.Contains(userMsg, "could not be updated") {
		t.Fatalf("userMsg should not report a status-update failure for a deleted template: %q", userMsg)
	}
}

// TestMarkDefinitionFailedOnPersistErr_JoinsErrors verifies that when
// publishCreateFailedIfJobActive returns a non-ErrTemplateNotFound error,
// the returned error satisfies errors.Is for both the original persistErr
// and the update error (via errors.Join).
func TestMarkDefinitionFailedOnPersistErr_JoinsErrors(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	persistErr := errors.New("upsert template replica fail: invalid connection")
	updateErr := errors.New("database connection timeout")

	patches.ApplyFunc(publishCreateFailedIfJobActive,
		func(ctx context.Context, templateID, jobID, lastError string) (bool, error) {
			return false, updateErr
		})
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {})

	userMsg, err := markDefinitionFailedOnPersistErr(
		context.Background(), "tpl-1", "job-3", 3, persistErr,
	)

	// Both errors must be matchable through errors.Is.
	if !errors.Is(err, persistErr) {
		t.Fatalf("returned error %v does not wrap persistErr", err)
	}
	if !errors.Is(err, updateErr) {
		t.Fatalf("returned error %v does not wrap updateErr", err)
	}
	// The wrapped update error must also be unwrap-able via the %w verb.
	if !strings.Contains(err.Error(), "database connection timeout") {
		t.Fatalf("returned error %q missing update error message", err)
	}
	// The user-facing message reports the update failure without the raw error.
	if !strings.Contains(userMsg, "template status could not be updated") {
		t.Fatalf("userMsg missing status-update note: %q", userMsg)
	}
	for _, leak := range []string{"invalid connection", "database connection timeout"} {
		if strings.Contains(userMsg, leak) {
			t.Fatalf("userMsg leaks %q: %q", leak, userMsg)
		}
	}
}

// TestMarkDefinitionFailedOnPersistErr_SanitizesErrorMessage verifies that
// the sanitized message written to the definition never contains raw SQL or
// driver error fragments, even when the persist error carries sensitive
// internal details.
func TestMarkDefinitionFailedOnPersistErr_SanitizesErrorMessage(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	// Simulate a raw GORM/driver error with SQL fragments.
	rawDBErr := errors.New(rawDBErrText)

	var publishedLastError string
	patches.ApplyFunc(publishCreateFailedIfJobActive,
		func(ctx context.Context, templateID, jobID, lastError string) (bool, error) {
			publishedLastError = lastError
			return true, nil
		})
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {})

	userMsg, _ := markDefinitionFailedOnPersistErr(
		context.Background(), "tpl-1", "job-sanitize", 1, rawDBErr,
	)

	// None of the raw DB error content should appear in either user-facing
	// message (definition last_error or job-row error_message).
	for _, leak := range rawDBErrLeaks {
		if strings.Contains(publishedLastError, leak) {
			t.Fatalf("last_error leaks %q: %q", leak, publishedLastError)
		}
		if strings.Contains(userMsg, leak) {
			t.Fatalf("userMsg leaks %q: %q", leak, userMsg)
		}
	}
}

// rawDBErrText mimics the raw GORM/driver error UpsertReplica can surface:
// SQL error codes, credentials context, table and column names.
const rawDBErrText = "upsert template replica fail, template=tpl-1 node=node-a: " +
	"Error 1045 (28000): Access denied for user 'cube'@'10.0.0.5' " +
	"(using password: YES) [table=template_replicas, col=node_id]"

var rawDBErrLeaks = []string{"Error 1045", "Access denied", "template_replicas", "col=node_id", "10.0.0.5"}

// persistErrCallChainRecorder captures the side effects of the persistErr
// branch through the real finish call chain.
type persistErrCallChainRecorder struct {
	publishedStatuses []string
	publishedErrors   []string
	jobUpdates        []map[string]any
	// publishErr, when set, is returned by the stubbed definition publish.
	publishErr error
}

// lastJobErrorMessage returns the error_message of the last job-row update
// that set one, i.e. the terminal value the build-status API returns.
func (r *persistErrCallChainRecorder) lastJobErrorMessage() (string, bool) {
	for i := len(r.jobUpdates) - 1; i >= 0; i-- {
		if msg, ok := r.jobUpdates[i]["error_message"]; ok {
			s, _ := msg.(string)
			return s, true
		}
	}
	return "", false
}

func (r *persistErrCallChainRecorder) lastJobStatus() string {
	for i := len(r.jobUpdates) - 1; i >= 0; i-- {
		if status, ok := r.jobUpdates[i]["status"]; ok {
			s, _ := status.(string)
			return s
		}
	}
	return ""
}

// patchPersistErrCommon stubs the finish-path steps from "definition written"
// onwards, with createTemplateReplicasOnNodes returning a raw DB error.
func patchPersistErrCommon(patches *gomonkey.Patches, rec *persistErrCallChainRecorder, persistErr error) {
	patches.ApplyFunc(updateTemplateImageJob, func(ctx context.Context, jobID string, values map[string]any) error {
		rec.jobUpdates = append(rec.jobUpdates, values)
		return nil
	})
	patches.ApplyFunc(normalizeStoredTemplateRequest, func(req *types.CreateCubeSandboxReq, enableJFSVariants bool) (*types.CreateCubeSandboxReq, error) {
		return req, nil
	})
	patches.ApplyFunc(ensureTemplateDefinitionWithOptions, func(ctx context.Context, templateID string, storedReq *types.CreateCubeSandboxReq, instanceType, version string, opts definitionCreateOptions) (bool, error) {
		return true, nil
	})
	patches.ApplyFunc(createTemplateReplicasOnNodes, func(ctx context.Context, templateID string, req *types.CreateCubeSandboxReq, targets []*node.Node, opts replicaRunOptions) ([]ReplicaStatus, error) {
		// Every node reports READY in memory, but the DB write failed: the
		// exact shape that used to leave the definition stuck in PENDING.
		return []ReplicaStatus{{NodeID: "node-a", Status: ReplicaStatusReady}}, persistErr
	})
	patches.ApplyFunc(publishCreateFailedIfJobActive, func(ctx context.Context, templateID, jobID, lastError string) (bool, error) {
		rec.publishedStatuses = append(rec.publishedStatuses, StatusFailed)
		rec.publishedErrors = append(rec.publishedErrors, lastError)
		return true, rec.publishErr
	})
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {})
}

func assertPersistErrOutcome(t *testing.T, rec *persistErrCallChainRecorder) {
	t.Helper()
	// Definition: published exactly once, as FAILED, with a sanitized message.
	if len(rec.publishedStatuses) != 1 || rec.publishedStatuses[0] != StatusFailed {
		t.Fatalf("published definition statuses = %v, want exactly [%s]", rec.publishedStatuses, StatusFailed)
	}
	// Job row: terminal FAILED with a sanitized error_message.
	if got := rec.lastJobStatus(); got != JobStatusFailed {
		t.Fatalf("job status = %q, want %q", got, JobStatusFailed)
	}
	jobMsg, ok := rec.lastJobErrorMessage()
	if !ok || jobMsg == "" {
		t.Fatal("job row error_message was not written")
	}
	for _, leak := range rawDBErrLeaks {
		if strings.Contains(jobMsg, leak) {
			t.Fatalf("job error_message leaks %q: %q", leak, jobMsg)
		}
		if strings.Contains(rec.publishedErrors[0], leak) {
			t.Fatalf("definition last_error leaks %q: %q", leak, rec.publishedErrors[0])
		}
	}
	if jobMsg != rec.publishedErrors[0] {
		t.Fatalf("job error_message %q differs from definition last_error %q", jobMsg, rec.publishedErrors[0])
	}
}

// TestFinishTemplateImageJobAfterArtifact_PersistErrMarksDefinitionFailed
// drives the real create call chain into the persistErr branch: the
// definition must be published FAILED (not left PENDING, not finalized), the
// job row must be FAILED with a sanitized message, and the raw error must only
// come back through the return value.
func TestFinishTemplateImageJobAfterArtifact_PersistErrMarksDefinitionFailed(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	rec := &persistErrCallChainRecorder{}
	persistErr := errors.New(rawDBErrText)
	patchPersistErrCommon(patches, rec, persistErr)

	targets := []*node.Node{{InsID: "node-a", IP: "10.0.0.1", Healthy: true}}
	patches.ApplyFunc(distributeRootfsArtifact, func(ctx context.Context, req *types.CreateTemplateFromImageReq, generatedReq *types.CreateCubeSandboxReq, artifact *models.RootfsArtifact, templateID, jobID string) ([]*node.Node, int32, int32, int32, error) {
		return targets, 1, 1, 0, nil
	})
	// Upstream loads the job row before replica creation to tell CREATE from
	// a TC-forwarded REDO; this is a plain CREATE.
	patches.ApplyFunc(getTemplateImageJobRecordByID, func(ctx context.Context, jobID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{JobID: jobID, TemplateID: "tpl-1", Operation: JobOperationCreate}, nil
	})
	finalized := false
	patches.ApplyFunc(finalizeTemplateReplicas, func(ctx context.Context, templateID, jobID, instanceType, version string, replicas []ReplicaStatus) (*TemplateInfo, string, error) {
		finalized = true
		return &TemplateInfo{TemplateID: templateID, Status: StatusReady}, "", nil
	})
	cleanedUp := false
	patches.ApplyFunc(cleanupFailedRootfsArtifact, func(ctx context.Context, artifact *models.RootfsArtifact, instanceType, templateID string) error {
		cleanedUp = true
		return nil
	})

	req := &types.CreateTemplateFromImageReq{
		Request:      &types.Request{RequestID: "req-1"},
		TemplateID:   "tpl-1",
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	}
	artifact := &models.RootfsArtifact{ArtifactID: "artifact-1"}
	generatedReq := &types.CreateCubeSandboxReq{InstanceType: req.InstanceType}

	err := finishTemplateImageJobAfterArtifact(context.Background(), "job-1", req, artifact, generatedReq, true)

	if !errors.Is(err, persistErr) {
		t.Fatalf("returned error %v does not wrap persistErr", err)
	}
	if finalized {
		t.Fatal("finalizeTemplateReplicas must not run on persistErr: it would publish READY for unpersisted replicas")
	}
	if !cleanedUp {
		t.Fatal("fresh artifact must be cleaned up on persistErr")
	}
	assertPersistErrOutcome(t, rec)
}

// TestFinishTemplateImageJobAfterArtifact_NonCreatePersistErrLeavesDefinition
// covers a TC-forwarded REDO, which shares this finish path with create. The
// fix is scoped to CREATE, so a REDO keeps the previous behaviour: the job
// fails and the definition status is not published.
func TestFinishTemplateImageJobAfterArtifact_NonCreatePersistErrLeavesDefinition(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	rec := &persistErrCallChainRecorder{}
	persistErr := errors.New(rawDBErrText)
	patchPersistErrCommon(patches, rec, persistErr)

	targets := []*node.Node{{InsID: "node-a", IP: "10.0.0.1", Healthy: true}}
	patches.ApplyFunc(distributeRootfsArtifact, func(ctx context.Context, req *types.CreateTemplateFromImageReq, generatedReq *types.CreateCubeSandboxReq, artifact *models.RootfsArtifact, templateID, jobID string) ([]*node.Node, int32, int32, int32, error) {
		return targets, 1, 1, 0, nil
	})
	patches.ApplyFunc(getTemplateImageJobRecordByID, func(ctx context.Context, jobID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{JobID: jobID, TemplateID: "tpl-1", Operation: JobOperationRedo}, nil
	})
	patches.ApplyFunc(ListReplicas, func(ctx context.Context, templateID string) ([]models.TemplateReplica, error) {
		return []models.TemplateReplica{{NodeID: "node-a", Status: ReplicaStatusReady}}, nil
	})
	patches.ApplyFunc(prepareRedoTemplateReplicas, func(ctx context.Context, templateID, instanceType string, existingReplicas []models.TemplateReplica, targets []*node.Node, backend string) error {
		return nil
	})
	patches.ApplyFunc(cleanupFailedRootfsArtifact, func(ctx context.Context, artifact *models.RootfsArtifact, instanceType, templateID string) error {
		return nil
	})

	req := &types.CreateTemplateFromImageReq{
		Request:      &types.Request{RequestID: "req-1"},
		TemplateID:   "tpl-1",
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	}
	err := finishTemplateImageJobAfterArtifact(context.Background(), "job-1", req,
		&models.RootfsArtifact{ArtifactID: "artifact-1"}, &types.CreateCubeSandboxReq{InstanceType: req.InstanceType}, true)

	if !errors.Is(err, persistErr) {
		t.Fatalf("returned error %v does not wrap persistErr", err)
	}
	if len(rec.publishedStatuses) != 0 {
		t.Fatalf("published definition statuses = %v, want none for a non-CREATE job", rec.publishedStatuses)
	}
	if got := rec.lastJobStatus(); got != JobStatusFailed {
		t.Fatalf("job status = %q, want %q", got, JobStatusFailed)
	}
	// Sanitisation is scoped to CREATE: a REDO job row still stores the raw
	// error, as before this change. If sanitisation is ever extended to redo,
	// this assertion must be updated together with the docs.
	if jobMsg, _ := rec.lastJobErrorMessage(); jobMsg != persistErr.Error() {
		t.Fatalf("REDO job error_message = %q, want the raw persistErr %q", jobMsg, persistErr.Error())
	}
}

// runCreatePersistErrChain drives a CREATE through the persistErr branch of
// finishTemplateImageJobAfterArtifact and reports whether the artifact cleanup
// ran.
func runCreatePersistErrChain(t *testing.T, rec *persistErrCallChainRecorder, persistErr error, builtFreshArtifact bool) (error, bool) {
	t.Helper()
	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patchPersistErrCommon(patches, rec, persistErr)
	targets := []*node.Node{{InsID: "node-a", IP: "10.0.0.1", Healthy: true}}
	patches.ApplyFunc(distributeRootfsArtifact, func(ctx context.Context, req *types.CreateTemplateFromImageReq, generatedReq *types.CreateCubeSandboxReq, artifact *models.RootfsArtifact, templateID, jobID string) ([]*node.Node, int32, int32, int32, error) {
		return targets, 1, 1, 0, nil
	})
	patches.ApplyFunc(getTemplateImageJobRecordByID, func(ctx context.Context, jobID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{JobID: jobID, TemplateID: "tpl-1", Operation: JobOperationCreate}, nil
	})
	cleanedUp := false
	patches.ApplyFunc(cleanupFailedRootfsArtifact, func(ctx context.Context, artifact *models.RootfsArtifact, instanceType, templateID string) error {
		cleanedUp = true
		return nil
	})
	req := &types.CreateTemplateFromImageReq{
		Request:      &types.Request{RequestID: "req-1"},
		TemplateID:   "tpl-1",
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
	}
	err := finishTemplateImageJobAfterArtifact(context.Background(), "job-1", req,
		&models.RootfsArtifact{ArtifactID: "artifact-1"}, &types.CreateCubeSandboxReq{InstanceType: req.InstanceType}, builtFreshArtifact)
	return err, cleanedUp
}

// An adopted artifact (builtFreshArtifact == false) is not owned by this job:
// the definition is still published FAILED, but the artifact is not cleaned up.
func TestFinishTemplateImageJobAfterArtifact_PersistErrAdoptedArtifactSkipsCleanup(t *testing.T) {
	rec := &persistErrCallChainRecorder{}
	persistErr := errors.New(rawDBErrText)

	err, cleanedUp := runCreatePersistErrChain(t, rec, persistErr, false)

	if !errors.Is(err, persistErr) {
		t.Fatalf("returned error %v does not wrap persistErr", err)
	}
	if cleanedUp {
		t.Fatal("an adopted artifact must not be cleaned up")
	}
	assertPersistErrOutcome(t, rec)
}

// When the definition publish itself fails, the job row reports it with a
// sanitized note and the returned error still wraps the raw persistErr.
func TestFinishTemplateImageJobAfterArtifact_PersistErrPublishFailure(t *testing.T) {
	rec := &persistErrCallChainRecorder{publishErr: errors.New("database connection timeout")}
	persistErr := errors.New(rawDBErrText)

	err, _ := runCreatePersistErrChain(t, rec, persistErr, true)

	if !errors.Is(err, persistErr) {
		t.Fatalf("returned error %v does not wrap persistErr", err)
	}
	if !errors.Is(err, rec.publishErr) {
		t.Fatalf("returned error %v does not wrap the publish error", err)
	}
	if got := rec.lastJobStatus(); got != JobStatusFailed {
		t.Fatalf("job status = %q, want %q", got, JobStatusFailed)
	}
	jobMsg, _ := rec.lastJobErrorMessage()
	if !strings.Contains(jobMsg, "template status could not be updated") {
		t.Fatalf("job error_message missing status-update note: %q", jobMsg)
	}
	for _, leak := range append(rawDBErrLeaks, "database connection timeout") {
		if strings.Contains(jobMsg, leak) {
			t.Fatalf("job error_message leaks %q: %q", leak, jobMsg)
		}
	}
}
