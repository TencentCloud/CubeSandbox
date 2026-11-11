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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// nilLogger satisfies the logger interface used by
// markDefinitionFailedOnPersistErr. It is safe because the test stubs
// control publishTemplateStatusWithAlias and the helper only calls
// Errorf when the stub returns a non-nil update error.
type nilLogger struct{}

func (nilLogger) Errorf(string, ...any) {}

// TestMarkDefinitionFailedOnPersistErr_PublishesFailed verifies that when
// replica persistence fails, the template definition is published as FAILED
// with a sanitized error message, and the returned error is the original
// persistErr when the publish succeeds.
func TestMarkDefinitionFailedOnPersistErr_PublishesFailed(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	persistErr := errors.New("upsert template replica fail: invalid connection")

	var publishedStatus, publishedLastError string
	patches.ApplyFunc(publishTemplateStatusWithAlias,
		func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
			publishedStatus = status
			publishedLastError = lastError
			return "", "", nil
		})
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {})

	userMsg, err := markDefinitionFailedOnPersistErr(
		context.Background(), "tpl-1", "job-1", 2, persistErr, nilLogger{},
	)

	// Definition must be published as FAILED.
	if publishedStatus != StatusFailed {
		t.Fatalf("published status = %q, want %q", publishedStatus, StatusFailed)
	}
	// The error written to the definition must be sanitized: no raw DB error.
	if strings.Contains(publishedLastError, "invalid connection") {
		t.Fatalf("last_error leaks raw DB error: %q", publishedLastError)
	}
	if !strings.Contains(publishedLastError, "2 target node(s)") {
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
// publishTemplateStatusWithAlias returns ErrTemplateNotFound (concurrent
// delete removed the definition row), the update error is suppressed and
// the returned error contains only persistErr.
func TestMarkDefinitionFailedOnPersistErr_TemplateNotFound(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	persistErr := errors.New("upsert template replica fail: invalid connection")

	patches.ApplyFunc(publishTemplateStatusWithAlias,
		func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
			return "", "", ErrTemplateNotFound
		})
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {})

	userMsg, err := markDefinitionFailedOnPersistErr(
		context.Background(), "tpl-gone", "job-2", 1, persistErr, nilLogger{},
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
// publishTemplateStatusWithAlias returns a non-ErrTemplateNotFound error,
// the returned error satisfies errors.Is for both the original persistErr
// and the update error (via errors.Join).
func TestMarkDefinitionFailedOnPersistErr_JoinsErrors(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	persistErr := errors.New("upsert template replica fail: invalid connection")
	updateErr := errors.New("database connection timeout")

	patches.ApplyFunc(publishTemplateStatusWithAlias,
		func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
			return "", "", updateErr
		})
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {})

	userMsg, err := markDefinitionFailedOnPersistErr(
		context.Background(), "tpl-1", "job-3", 3, persistErr, nilLogger{},
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
	patches.ApplyFunc(publishTemplateStatusWithAlias,
		func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
			publishedLastError = lastError
			return "", "", nil
		})
	patches.ApplyFunc(invalidateTemplateCaches, func(templateID string) {})

	userMsg, _ := markDefinitionFailedOnPersistErr(
		context.Background(), "tpl-1", "job-sanitize", 1, rawDBErr, nilLogger{},
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
// branch through the real create/redo call chains.
type persistErrCallChainRecorder struct {
	publishedStatuses []string
	publishedErrors   []string
	jobUpdates        []map[string]any
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

// patchPersistErrCommon stubs the steps shared by the create and redo paths
// from "definition written" onwards, with createTemplateReplicasOnNodes
// returning a raw DB error.
func patchPersistErrCommon(patches *gomonkey.Patches, rec *persistErrCallChainRecorder, persistErr error) {
	patches.ApplyFunc(updateTemplateImageJob, func(ctx context.Context, jobID string, values map[string]any) error {
		rec.jobUpdates = append(rec.jobUpdates, values)
		return nil
	})
	patches.ApplyFunc(normalizeStoredTemplateRequest, func(req *types.CreateCubeSandboxReq) (*types.CreateCubeSandboxReq, error) {
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
	patches.ApplyFunc(publishTemplateStatusWithAlias, func(ctx context.Context, templateID, jobID, status, lastError string) (string, string, error) {
		rec.publishedStatuses = append(rec.publishedStatuses, status)
		rec.publishedErrors = append(rec.publishedErrors, lastError)
		return "", "", nil
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

// TestRunRedoTemplateImageJob_PersistErrMarksDefinitionFailed drives the real
// redo call chain (SNAPSHOTTING resume with a reusable artifact) into the
// persistErr branch and asserts the same outcome as the create path. It also
// asserts refreshTemplateReplicaSummary is skipped, since the replica rows it
// would summarize are the ones that failed to persist.
func TestRunRedoTemplateImageJob_PersistErrMarksDefinitionFailed(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	rec := &persistErrCallChainRecorder{}
	persistErr := errors.New(rawDBErrText)
	patchPersistErrCommon(patches, rec, persistErr)

	const artifactID = "artifact-redo"
	targets := []*node.Node{{InsID: "node-a", IP: "10.0.0.1", Healthy: true}}

	patches.ApplyFunc(getTemplateImageJobRecordByID, func(ctx context.Context, jobID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:       jobID,
			TemplateID:  "tpl-1",
			ResumePhase: JobPhaseSnapshotting,
			ArtifactID:  artifactID,
		}, nil
	})
	patches.ApplyFunc(unmarshalTemplateImageJobRequest, func(payload string) (*types.CreateTemplateFromImageReq, error) {
		return &types.CreateTemplateFromImageReq{
			Request:        &types.Request{RequestID: "req-1"},
			TemplateID:     "tpl-1",
			InstanceType:   cubeboxv1.InstanceType_cubebox.String(),
			SourceImageRef: "docker.io/library/nginx:latest",
		}, nil
	})
	patches.ApplyFunc(ListReplicas, func(ctx context.Context, templateID string) ([]models.TemplateReplica, error) {
		return []models.TemplateReplica{{NodeID: "node-a", Status: ReplicaStatusReady}}, nil
	})
	patches.ApplyFunc(resolveRedoTargets, func(instanceType string, req *types.RedoTemplateFromImageReq, replicas []models.TemplateReplica) ([]*node.Node, error) {
		return targets, nil
	})
	patches.ApplyFunc(rootfsArtifactReusableForRedo, func(ctx context.Context, gotArtifactID string) bool {
		return true
	})
	patches.ApplyFunc(getRootfsArtifactByID, func(ctx context.Context, gotArtifactID string) (*models.RootfsArtifact, error) {
		return &models.RootfsArtifact{ArtifactID: gotArtifactID, Status: ArtifactStatusReady}, nil
	})
	patches.ApplyFunc(generateTemplateCreateRequest, func(ctx context.Context, req *types.CreateTemplateFromImageReq, artifact *models.RootfsArtifact, imageCfg DockerImageConfig, downloadBaseURL string) (*types.CreateCubeSandboxReq, error) {
		return &types.CreateCubeSandboxReq{
			InstanceType: req.InstanceType,
			Annotations: map[string]string{
				constants.CubeAnnotationAppSnapshotTemplateID: req.TemplateID,
			},
		}, nil
	})
	patches.ApplyFunc(cleanupTemplateReplicasOnNodes, func(ctx context.Context, templateID string, replicas []models.TemplateReplica, targets []*node.Node, backend string) error {
		return nil
	})
	refreshed := false
	patches.ApplyFunc(refreshTemplateReplicaSummary, func(ctx context.Context, templateID, jobID string) (string, error) {
		refreshed = true
		return "", nil
	})

	runRedoTemplateImageJob(context.Background(), "job-redo", &types.RedoTemplateFromImageReq{
		Request:    &types.Request{RequestID: "req-redo"},
		TemplateID: "tpl-1",
	}, "http://master.example")

	if refreshed {
		t.Fatal("refreshTemplateReplicaSummary must not run after persistErr")
	}
	assertPersistErrOutcome(t, rec)
}
