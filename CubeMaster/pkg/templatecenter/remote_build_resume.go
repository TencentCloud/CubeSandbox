// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/image"
)

// RemoteBuildResult is the artifact metadata reported by CubeTemplateCenter
// when a remote build finishes. It carries everything CubeMaster needs to
// register the artifact and continue the pipeline WITHOUT touching the image
// registry itself: TC pulled the image, so TC reports the image config.
type RemoteBuildResult struct {
	ArtifactID              string
	TemplateSpecFingerprint string
	SourceImageDigest       string
	Ext4Path                string
	Ext4SHA256              string
	Ext4SizeBytes           int64
	ImageConfigJSON         string
	// MasterNodeIP mirrors the column of the same (misleading) name: it holds
	// the artifact DOWNLOAD BASE URL, not an IP. TC echoes back the base URL
	// CubeMaster handed it at submit time, so Cubelet keeps pulling the ext4
	// from CubeMaster exactly as in local mode.
	//
	// This is why TC and CubeMaster must share the artifact directory (one CBS
	// disk / PVC): the file is written by TC and served by CubeMaster.
	MasterNodeIP            string
	CubeEgressCABaked       bool
	CubeEgressCAFingerprint string
	CubeEgressCATargets     int
}

// Validate checks the fields the downstream distribution guard requires.
// distributeRootfsArtifact refuses incomplete records (ext4_size_bytes=0 /
// empty download_token), so catching it here yields a far clearer error.
func (r *RemoteBuildResult) Validate() error {
	var missing []string
	if strings.TrimSpace(r.ArtifactID) == "" {
		missing = append(missing, "artifact_id")
	}
	if strings.TrimSpace(r.Ext4Path) == "" {
		missing = append(missing, "ext4_path")
	}
	if strings.TrimSpace(r.Ext4SHA256) == "" {
		missing = append(missing, "ext4_sha256")
	}
	if r.Ext4SizeBytes <= 0 {
		missing = append(missing, "ext4_size_bytes")
	}
	// distributeRootfsArtifact refuses to push a CreateImage when this is
	// empty, and the resulting download URL would fall back to os.Hostname().
	if strings.TrimSpace(r.MasterNodeIP) == "" {
		missing = append(missing, "master_node_ip")
	}
	if len(missing) > 0 {
		return fmt.Errorf("remote build result is missing required field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// remoteBuildReportKey is where TC's BUILT report is preserved inside the job's
// final result_json.
const remoteBuildReportKey = "remote_build_report"

// preserveRemoteBuildReport folds a remote BUILT report into the job's final
// result_json instead of letting it be overwritten.
//
// The status callback stores TC's raw BUILT payload in result_json, which is the
// only durable record that the ext4 was produced by CubeTemplateCenter rather
// than in-process — it carries the ext4 sha256, size and image config TC
// measured itself. The finalize step then writes the template payload to the
// same column, so that evidence used to be destroyed on every job, including the
// failed ones where it is most needed for diagnosis.
//
// Returns finalResult unchanged on any problem: the primary payload must never
// be lost in order to keep a diagnostic.
func preserveRemoteBuildReport(ctx context.Context, jobID string, finalResult []byte) []byte {
	record, err := getTemplateImageJobRecordByID(ctx, jobID)
	if err != nil || record == nil {
		return finalResult
	}
	return mergeRemoteBuildReport(record.ResultJSON, finalResult)
}

// mergeRemoteBuildReport is the decision half of preserveRemoteBuildReport,
// separated from the DB read so the matrix is testable.
//
// Returns finalResult unchanged whenever there is nothing worth keeping or
// anything fails to parse: the payload being stored is the primary record and
// must never be sacrificed in order to retain a diagnostic.
func mergeRemoteBuildReport(prior string, finalResult []byte) []byte {
	prior = strings.TrimSpace(prior)
	if prior == "" {
		// A local build never wrote a BUILT report.
		return finalResult
	}

	var priorPayload map[string]any
	if err := json.Unmarshal([]byte(prior), &priorPayload); err != nil || priorPayload == nil {
		return finalResult
	}
	// Only a BUILT report is worth keeping. Anything else is either already a
	// finalize payload (a retry writing over its own output) or unrecognised.
	if status, _ := priorPayload["status"].(string); !strings.EqualFold(status, JobStatusBuilt) {
		return finalResult
	}
	// Defensive: never nest a report inside a report, which would grow the
	// column on every finalize.
	delete(priorPayload, remoteBuildReportKey)

	var merged map[string]any
	if err := json.Unmarshal(finalResult, &merged); err != nil || merged == nil {
		return finalResult
	}
	merged[remoteBuildReportKey] = priorPayload
	combined, err := json.Marshal(merged)
	if err != nil {
		return finalResult
	}
	return combined
}

// DetachRemoteBuildResumeContext derives the context the resume goroutine runs
// on. It must be detached from the HTTP request (returning the callback
// response must not cancel a cross-node distribution) but it must still carry a
// CubeLog.RequestTrace, exactly like every other detached template job.
//
// This is not cosmetic: log.G(ctx) on a context without a trace loses the
// request labels and the trace-scoped log sink, which is what made a failing
// resume look completely silent in templatecenter-req.log.
func DetachRemoteBuildResumeContext(ctx context.Context, jobID, artifactID string) context.Context {
	return detachTemplateImageJobContext(ctx, "template_image_resume_remote", map[string]any{
		"job_id":      jobID,
		"artifact_id": artifactID,
		"build_mode":  "remote",
	})
}

// ResumeTemplateImageJobAfterRemoteBuild continues a template build that was
// performed by the standalone CubeTemplateCenter process.
//
// TC owns only the data-plane work (pull image, bake rootfs, mkfs ext4) and
// reports the result back; every DB write and the whole distribution pipeline
// stay here, on CubeMaster. This function therefore performs exactly the steps
// runTemplateImageJob does after ensureRootfsArtifact returns, reusing the same
// helpers so local and remote modes cannot diverge:
//
//  1. register the artifact row (claim + finalize) using TC's metadata
//  2. generate the template's create-sandbox request
//  3. distribute the artifact to Cubelet nodes
//  4. write template_definitions
//  5. create template_replicas
//  6. claim the alias and aggregate replica status
//  7. write the job's terminal status
//
// It is invoked from the internal status-callback handler when TC reports
// status=BUILT. Errors are reported into the job row, so the caller only needs
// to log them.
func ResumeTemplateImageJobAfterRemoteBuild(ctx context.Context, jobID string, result *RemoteBuildResult) error {
	// Logged unconditionally and BEFORE any early return: a resume that bails
	// out at the guards below used to produce no trace at all, which is
	// indistinguishable from the goroutine never having run.
	log.G(ctx).Infof("resume remote-built template job: start job_id=%s", jobID)

	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	if result == nil {
		return fmt.Errorf("remote build result is nil")
	}
	if err := result.Validate(); err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhaseBuildingExt4,
			"progress":      100,
			"error_message": err.Error(),
		})
		return err
	}

	job, err := getTemplateImageJobRecordByID(ctx, jobID)
	if err != nil {
		return fmt.Errorf("load job %s: %w", jobID, err)
	}

	// The original request was snapshotted at submit time; rebuild it so the
	// downstream helpers see exactly the same input local mode would pass.
	req, err := unmarshalTemplateImageJobRequest(job.RequestJSON)
	if err != nil {
		failErr := fmt.Errorf("decode job request snapshot: %w", err)
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhaseCreatingTemplate,
			"progress":      100,
			"error_message": failErr.Error(),
		})
		return failErr
	}

	logger := log.G(ctx).WithFields(map[string]any{
		"job_id":      jobID,
		"template_id": req.TemplateID,
		"artifact_id": result.ArtifactID,
		"build_mode":  "remote",
	})

	// Step 1 + 2: register the artifact row and derive the create request.
	logger.Infof("resume step 1/3: register remote-built artifact")
	artifact, generatedReq, err := registerRemoteBuiltArtifact(ctx, req, result)
	if err != nil {
		failErr := fmt.Errorf("register remote artifact: %w", err)
		logger.Errorf("%v", failErr)
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseBuildingExt4,
			"artifact_id":     result.ArtifactID,
			"artifact_status": ArtifactStatusFailed,
			"progress":        100,
			"error_message":   failErr.Error(),
		})
		return failErr
	}

	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"artifact_id":               artifact.ArtifactID,
		"template_spec_fingerprint": artifact.TemplateSpecFingerprint,
		"source_image_digest":       artifact.SourceImageDigest,
		"artifact_status":           artifact.Status,
		"status":                    JobStatusRunning,
		"phase":                     JobPhaseDistributing,
		"progress":                  70,
	}); err != nil {
		logger.Errorf("update job artifact fail: %v", err)
	}

	// Step 3: distribute to nodes. builtFreshArtifact is always true here: TC
	// just produced this ext4, so on failure the artifact is ours to clean up.
	logger.Infof("resume step 2/3: artifact registered, entering distribution: artifact_status=%s ext4_size_bytes=%d",
		artifact.Status, artifact.Ext4SizeBytes)
	err = finishTemplateImageJobAfterArtifact(ctx, jobID, req, artifact, generatedReq, true)
	if err != nil {
		logger.Errorf("resume step 3/3 failed: %v", err)
		return err
	}
	logger.Infof("resume step 3/3: template is READY")
	return nil
}

// registerRemoteBuiltArtifact claims the artifact row (same FOR UPDATE guard
// local mode uses, so a concurrent delete cannot remove it mid-flight) and
// finalizes it with the metadata TC reported, instead of building the ext4
// locally.
func registerRemoteBuiltArtifact(ctx context.Context, req *types.CreateTemplateFromImageReq, result *RemoteBuildResult) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, error) {
	fingerprint := result.TemplateSpecFingerprint
	if strings.TrimSpace(fingerprint) == "" {
		return nil, nil, fmt.Errorf("remote build result is missing template_spec_fingerprint")
	}

	claimed, err := claimRootfsArtifactForBuild(ctx, result.ArtifactID, fingerprint, req, result.SourceImageDigest)
	if err != nil {
		return nil, nil, fmt.Errorf("claim artifact row: %w", err)
	}
	record := claimed
	if record == nil {
		record, err = getRootfsArtifactByID(ctx, result.ArtifactID)
		if err != nil {
			return nil, nil, fmt.Errorf("load claimed artifact row: %w", err)
		}
	}

	imageCfg, err := decodeImageConfigJSON(result.ImageConfigJSON)
	if err != nil {
		return nil, nil, err
	}

	return finalizeRemoteArtifact(ctx, record, req, result, imageCfg)
}

// finalizeRemoteArtifact mirrors finalizeArtifact but sources its values from
// TC's report rather than a local *image.PreparedSource. Keep the persisted
// column set identical to finalizeArtifact.
func finalizeRemoteArtifact(
	ctx context.Context,
	record *models.RootfsArtifact,
	req *types.CreateTemplateFromImageReq,
	result *RemoteBuildResult,
	imageCfg image.DockerImageConfig,
) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, error) {
	record.SourceImageDigest = result.SourceImageDigest
	record.MasterNodeIP = result.MasterNodeIP
	record.Ext4Path = result.Ext4Path
	record.Ext4SHA256 = result.Ext4SHA256
	record.Ext4SizeBytes = result.Ext4SizeBytes
	record.ImageConfigJSON = result.ImageConfigJSON
	record.DownloadToken = uuid.New().String()
	record.Status = ArtifactStatusReady
	record.GCDeadline = time.Now().Add(defaultTemplateArtifactTTL).Unix()
	record.CubeEgressCABaked = result.CubeEgressCABaked
	record.CubeEgressCAFingerprint = result.CubeEgressCAFingerprint
	record.CubeEgressCATargetsWritten = result.CubeEgressCATargets

	// Same base URL local mode uses (CubeMaster's own request base URL, echoed
	// back by TC), so the generated request and the distribution annotations
	// are byte-identical across build modes: Cubelet pulls from CubeMaster.
	generatedReq, err := generateTemplateCreateRequest(req, record, imageCfg, record.MasterNodeIP)
	if err != nil {
		return nil, nil, fmt.Errorf("generate template create request: %w", err)
	}
	reqPayload, err := json.Marshal(generatedReq)
	if err != nil {
		return nil, nil, err
	}
	record.GeneratedRequestJSON = string(reqPayload)

	if err := updateRootfsArtifact(ctx, record.ArtifactID, map[string]any{
		"source_image_digest":            record.SourceImageDigest,
		"master_node_ip":                 record.MasterNodeIP,
		"ext4_path":                      record.Ext4Path,
		"ext4_sha256":                    record.Ext4SHA256,
		"ext4_size_bytes":                record.Ext4SizeBytes,
		"image_config_json":              record.ImageConfigJSON,
		"generated_request_json":         record.GeneratedRequestJSON,
		"download_token":                 record.DownloadToken,
		"status":                         record.Status,
		"gc_deadline":                    record.GCDeadline,
		"last_error":                     "",
		"cube_egress_ca_baked":           record.CubeEgressCABaked,
		"cube_egress_ca_fingerprint":     record.CubeEgressCAFingerprint,
		"cube_egress_ca_targets_written": record.CubeEgressCATargetsWritten,
	}); err != nil {
		return nil, nil, err
	}

	latest, err := getRootfsArtifactByID(ctx, record.ArtifactID)
	if err != nil {
		return nil, nil, err
	}
	return latest, generatedReq, nil
}

// decodeImageConfigJSON parses the image config TC captured during the pull.
// An empty payload is an error rather than a silent default: without
// Entrypoint/Cmd the generated template would boot nothing.
func decodeImageConfigJSON(payload string) (image.DockerImageConfig, error) {
	var cfg image.DockerImageConfig
	if strings.TrimSpace(payload) == "" {
		return cfg, fmt.Errorf("remote build result is missing image_config_json; " +
			"CubeTemplateCenter must report it so the template's create request can be generated")
	}
	// TC forwards source.ConfigJSON verbatim. Depending on the export path it
	// is either the bare config object or the full image manifest that wraps
	// it under "config"/"Config", so try the wrappers before failing.
	if err := json.Unmarshal([]byte(payload), &cfg); err == nil && !imageConfigEmpty(cfg) {
		return cfg, nil
	}
	var wrapper struct {
		Config       image.DockerImageConfig `json:"config"`
		ConfigUpper  image.DockerImageConfig `json:"Config"`
		ContainerCfg image.DockerImageConfig `json:"container_config"`
	}
	if err := json.Unmarshal([]byte(payload), &wrapper); err != nil {
		return cfg, fmt.Errorf("decode image_config_json: %w", err)
	}
	for _, candidate := range []image.DockerImageConfig{wrapper.Config, wrapper.ConfigUpper, wrapper.ContainerCfg} {
		if !imageConfigEmpty(candidate) {
			return candidate, nil
		}
	}
	// A genuinely empty config is legal for some scratch images; return the
	// zero value so the caller can still proceed.
	return cfg, nil
}

func imageConfigEmpty(cfg image.DockerImageConfig) bool {
	return len(cfg.Entrypoint) == 0 && len(cfg.Cmd) == 0 && len(cfg.Env) == 0 &&
		cfg.WorkingDir == "" && cfg.User == ""
}
