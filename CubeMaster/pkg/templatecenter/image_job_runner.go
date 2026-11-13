// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// distributionFailure decides whether a distribution outcome is terminal, and
// returns the error to record when it is (nil means "keep going").
//
// Kept as a pure function so the whole outcome matrix is testable without a DB.
//
// There is deliberately NO minimum node count: expected==1 is a perfectly
// valid single-node deployment and 1 ready of 1 expected is success. What
// matters is only whether any node ended up ready.
//
// The two zero-ready cases are distinguished because they need different
// diagnoses, and conflating them is what made this failure mode opaque:
//
//	expected == 0  distribution never reached a node at all — no healthy node
//	               for the instance type, a distribution_scope matching
//	               nothing, or an artifact that failed the readiness guard.
//	               distErr is wrapped with %w so callers can still match it.
//	expected  > 0  every node was tried and every node failed.
//
// The earlier guard read `expected > 0 && ready == 0`, so the expected==0 case
// fell through it: the job still ended FAILED (summarizeStatus of zero replicas
// is FAILED), but only after writing a template_definition for a template that
// cannot have replicas, and finalizeTemplateReplicas then overwrote
// error_message with a generic "failed on all nodes", destroying the only
// explanation of what actually went wrong.
func distributionFailure(expected, ready int32, distErr error) error {
	if ready > 0 {
		return nil
	}
	if expected == 0 {
		// distErr is always set on this path today; keep a usable message if a
		// future caller ever returns zero targets without an error.
		if distErr == nil {
			distErr = ErrNoTemplateNodes
		}
		return fmt.Errorf("artifact distribution did not reach any node: %w", distErr)
	}
	return fmt.Errorf("artifact distribution failed on all %d node(s): %v", expected, distErr)
}

// finishTemplateImageJobAfterArtifact runs every step that follows a ready
// rootfs artifact: distribute to nodes, write template_definitions, create
// replicas, claim the alias, aggregate status and write the job's terminal row.
//
// Extracted from runTemplateImageJob so the remote build path
// (ResumeTemplateImageJobAfterRemoteBuild) executes the exact same logic —
// duplicating it would let local and remote modes drift apart.
//
// builtFreshArtifact tells the failure paths whether this call owns the
// artifact (and may therefore delete it) or merely reused an existing one.
// The job status is always persisted, so callers may simply log the returned
// error. The returned error is NOT what the job row holds: the job row's
// error_message is exposed through the build-status API and may carry a
// sanitized message (see markDefinitionFailedOnPersistErr), while the returned
// error can wrap the raw cause (driver errors with internal hosts, table
// names). Callers must only log it and never surface it to users.
func finishTemplateImageJobAfterArtifact(
	ctx context.Context,
	jobID string,
	req *types.CreateTemplateFromImageReq,
	artifact *models.RootfsArtifact,
	generatedReq *types.CreateCubeSandboxReq,
	builtFreshArtifact bool,
) error {
	logger := log.G(ctx).WithFields(map[string]any{
		"job_id":      jobID,
		"template_id": req.TemplateID,
		"artifact_id": artifact.ArtifactID,
	})
	readyTargets, expected, ready, failed, distErr := distributeRootfsArtifact(ctx, req, generatedReq, artifact, req.TemplateID, jobID)
	// Logged at Info even on success: before this, the only way to tell a
	// distribution that failed on every node from one that never ran was that
	// template_definitions/replicas stayed empty, which looks identical to the
	// pipeline silently not executing at all.
	logger.Infof("distribute artifact: expected=%d ready=%d failed=%d err=%v",
		expected, ready, failed, distErr)
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"phase":               JobPhaseCreatingTemplate,
		"progress":            85,
		"expected_node_count": expected,
		"ready_node_count":    ready,
		"failed_node_count":   failed,
		"error_message":       errorString(distErr),
	}); err != nil {
		logger.Errorf("update distribution status fail: %v", err)
	}
	// Zero ready nodes is terminal, whatever the reason.
	if failErr := distributionFailure(expected, ready, distErr); failErr != nil {
		logger.Errorf("%v; template_definitions/replicas are intentionally NOT written for this job", failErr)
		// Persist the diagnosis BEFORE cleaning up. Cleanup deletes this
		// template's per-node replica rows along with the artifact, so the
		// per-node error messages recorded during distribution do not survive
		// it; the job row is the only place the reason can still be read
		// afterwards, and it must be written even if cleanup then fails.
		if err := updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhaseDistributing,
			"progress":      100,
			"error_message": failErr.Error(),
		}); err != nil {
			logger.Errorf("record distribution failure on job fail: %v", err)
		}
		if builtFreshArtifact {
			if cleanupErr := cleanupFailedRootfsArtifact(ctx, artifact, req.InstanceType, req.TemplateID); cleanupErr != nil {
				logger.Errorf("cleanup fresh rootfs artifact after distribution failure fail: %v", cleanupErr)
			}
		}
		return failErr
	}
	var info *TemplateInfo
	storedReq, err := normalizeStoredTemplateRequest(generatedReq)
	if err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseCreatingTemplate,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   err.Error(),
		})
		return err
	}
	if _, err := ensureTemplateDefinitionWithOptions(ctx, req.TemplateID, storedReq, generatedReq.InstanceType, constants.GetAppSnapshotVersion(generatedReq.Annotations), definitionCreateOptions{
		StorageBackend: req.Backend,
	}); err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseCreatingTemplate,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   err.Error(),
		})
		return err
	}
	logger.Infof("template definition written; creating replicas on %d node(s)", len(readyTargets))
	replicas, persistErr := createTemplateReplicasOnNodes(ctx, req.TemplateID, generatedReq, readyTargets, replicaRunOptions{
		ArtifactID: artifact.ArtifactID,
		JobID:      jobID,
	})
	claimWarning := ""
	// jobErrMsg overrides err.Error() in the job row when the raw error must
	// not be exposed through the build-status API.
	jobErrMsg := ""
	if persistErr != nil {
		jobErrMsg, err = markDefinitionFailedOnPersistErr(ctx, req.TemplateID, jobID, len(readyTargets), persistErr)
	} else {
		// finalizeTemplateReplicas claims the alias *before* publishing the
		// READY status so a client that sees READY can always resolve the
		// alias; claimWarning captures a non-fatal alias-transfer note.
		info, claimWarning, err = finalizeTemplateReplicas(ctx, req.TemplateID, jobID, generatedReq.InstanceType, constants.GetAppSnapshotVersion(generatedReq.Annotations), replicas)
	}
	if err != nil {
		if builtFreshArtifact {
			if cleanupErr := cleanupFailedRootfsArtifact(ctx, artifact, req.InstanceType, req.TemplateID); cleanupErr != nil {
				logger.Errorf("cleanup fresh rootfs artifact after create template error fail: %v", cleanupErr)
			}
		}
		if jobErrMsg == "" {
			jobErrMsg = err.Error()
		}
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseCreatingTemplate,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   jobErrMsg,
		})
		return err
	}
	// Alias was already claimed by finalizeTemplateReplicas (before the READY
	// status was published) to avoid the create/claim publish-ordering race.
	resultPayload, _ := json.Marshal(info)
	// Keep TC's BUILT report if this job was built remotely: writing the
	// template payload straight over result_json used to erase the only durable
	// proof of where the ext4 came from, on failed jobs too.
	resultPayload = preserveRemoteBuildReport(ctx, jobID, resultPayload)
	jobStatus := JobStatusReady
	jobPhase := JobPhaseReady
	if info.Status == StatusFailed {
		if builtFreshArtifact {
			if cleanupErr := cleanupFailedRootfsArtifact(ctx, artifact, req.InstanceType, req.TemplateID); cleanupErr != nil {
				logger.Errorf("cleanup fresh rootfs artifact after failed template status fail: %v", cleanupErr)
			}
		}
		jobStatus = JobStatusFailed
		jobPhase = JobPhaseCreatingTemplate
	}
	errorMessage := info.LastError
	if errorMessage == "" && claimWarning != "" {
		errorMessage = claimWarning
	}
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":          jobStatus,
		"phase":           jobPhase,
		"progress":        100,
		"template_status": info.Status,
		"result_json":     string(resultPayload),
		"error_message":   errorMessage,
	})
	if jobStatus == JobStatusFailed {
		return fmt.Errorf("template creation failed: %s", errorMessage)
	}
	return nil
}

// markDefinitionFailedOnPersistErr marks the template definition as FAILED
// after replica persistence fails and invalidates caches.
//
// It returns two values:
//   - userMsg: a sanitized, user-readable message. Both the definition's
//     last_error and the job row's error_message are exposed through the API
//     (GET /templates/:id and GET /templates/:id/builds/:bid), so callers must
//     write only userMsg to the job row.
//   - err: the raw error chain (persistErr, joined with the publish error if
//     any) for logs and for the caller's return value only.
//
// ErrTemplateNotFound from the publish is suppressed: a concurrent normal
// delete must not be turned into a second failure, and the job is still
// failed with the original persistErr.
func markDefinitionFailedOnPersistErr(
	ctx context.Context,
	templateID, jobID string,
	readyTargetCount int,
	persistErr error,
) (userMsg string, err error) {
	log.G(ctx).Errorf("replica persist error, template=%s job=%s err=%v", templateID, jobID, persistErr)
	userMsg = fmt.Sprintf(
		"failed to persist replica records (%d target node(s)); see CubeMaster logs for job %s",
		readyTargetCount, jobID,
	)
	err = persistErr
	if _, _, updErr := publishTemplateStatusWithAlias(ctx, templateID, jobID, StatusFailed, userMsg); updErr != nil {
		log.G(ctx).Errorf("mark template definition FAILED after replica persist error, template=%s job=%s err=%v", templateID, jobID, updErr)
		if !errors.Is(updErr, ErrTemplateNotFound) {
			err = errors.Join(err, fmt.Errorf("mark template definition failed: %w", updErr))
			userMsg += "; template status could not be updated"
		}
		invalidateTemplateCaches(templateID)
	}
	return userMsg, err
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func detachTemplateImageJobContext(ctx context.Context, name string, fields map[string]any) context.Context {
	detached := context.Background()
	rt := CubeLog.GetTraceInfo(ctx)
	if rt == nil {
		// Background workers (e.g. the snapshot reconciler) run without an
		// incoming request and therefore carry no trace. Synthesize one so the
		// detached context always has a usable trace and emits meaningful
		// metric labels. The caller names the operation explicitly so each
		// distinct job type gets its own trace label instead of sharing a
		// generic fallback.
		if strings.TrimSpace(name) == "" {
			name = "template_job"
		}
		rt = &CubeLog.RequestTrace{
			Action:         name,
			Caller:         name,
			Callee:         constants.CubeMasterServiceID,
			CalleeEndpoint: "localhost",
			Timestamp:      time.Now(),
		}
	} else {
		// Copy the inherited trace so mutations on the detached context do not
		// leak back into the originating request's trace.
		rt = rt.DeepCopy()
	}
	detached = CubeLog.WithRequestTrace(detached, rt)
	return log.WithLogger(detached, log.G(ctx).WithFields(fields))
}
