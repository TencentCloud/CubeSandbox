// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

// resumeArtifactLocks serializes concurrent resume attempts for the same
// template-spec fingerprint. A resume can be triggered from two sources at
// once: the internal status callback goroutine (TC reports BUILT) and the
// image-job reconciler (which replays jobs stuck in BUILT). Without a mutex
// both would run the register/distribute pipeline concurrently and
// regenerate the artifact's DownloadToken -- the in-flight download from the
// first attempt then 404s.
//
// Keyed by FINGERPRINT, not artifact_id or job_id: buildArtifactID mints a
// UUID-suffixed artifact_id per build, so two concurrent same-spec builds
// report BUILT with DIFFERENT artifact_ids, and the resource they collide on
// is the fingerprint unique index (idx_artifact_fingerprint) — the second
// registration's insert fails with Error 1062. Locking by fingerprint
// serializes exactly that collision (and matches the cluster-wide DB lock
// withArtifactRegisterLock takes inside registerRemoteBuiltArtifact, which
// covers the cross-process case this mutex cannot).
//
// Entries are reference-counted and removed once no goroutine holds/awaits
// the lock for that artifact_id. Without this, resumeArtifactLocks used to
// grow by one entry for EVERY artifact_id ever processed over the process
// lifetime and never shrink -- an unbounded map (memory leak) on a
// long-running Master.
var (
	resumeArtifactLocksMu sync.Mutex
	resumeArtifactLocks   = map[string]*refCountedMutex{}
)

type refCountedMutex struct {
	mu  sync.Mutex
	ref int
}

// acquireResumeArtifactLock locks (creating the entry on first use) and
// returns a release function that must be called exactly once to unlock and,
// once no other goroutine references the entry, remove it from the map.
func acquireResumeArtifactLock(artifactID string) func() {
	resumeArtifactLocksMu.Lock()
	entry, ok := resumeArtifactLocks[artifactID]
	if !ok {
		entry = &refCountedMutex{}
		resumeArtifactLocks[artifactID] = entry
	}
	entry.ref++
	resumeArtifactLocksMu.Unlock()

	entry.mu.Lock()

	released := false
	return func() {
		if released {
			return
		}
		released = true
		entry.mu.Unlock()

		resumeArtifactLocksMu.Lock()
		entry.ref--
		if entry.ref == 0 {
			delete(resumeArtifactLocks, artifactID)
		}
		resumeArtifactLocksMu.Unlock()
	}
}

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
	MasterNodeIP string
	// ArtifactURL is the S3/MinIO presigned download URL. When non-empty,
	// distribution uses this URL directly instead of building a local HTTP URL
	// from MasterNodeIP.
	ArtifactURL             string
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
	// Path-traversal guard: the status callback is unauthenticated, so
	// ext4_path is attacker-controlled. It must resolve to a file INSIDE one of
	// the controlled artifact root directories, or Cubelet could be told to
	// download an arbitrary host path as a rootfs. The check is by prefix on the
	// cleaned absolute path, matching how artifact_cleanup.go already locates
	// the artifact dir.
	if err := r.validateExt4PathWithinStore(); err != nil {
		return err
	}
	return nil
}

// validateExt4PathWithinStore rejects an ext4_path that escapes the controlled
// artifact store roots.
func (r *RemoteBuildResult) validateExt4PathWithinStore() error {
	clean := filepath.Clean(r.Ext4Path)
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("remote build result ext4_path %q is not an absolute path", r.Ext4Path)
	}
	roots := []string{ArtifactWorkRootDir(), ArtifactStoreRootDir()}
	if strings.TrimSpace(os.Getenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR")) == "" {
		roots = append(roots, ArtifactFallbackStoreRootDir())
	}
	for _, root := range roots {
		root = filepath.Clean(root)
		rel, err := filepath.Rel(root, clean)
		if err != nil {
			continue
		}
		// rel must not climb out of the root.
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("remote build result ext4_path %q is outside the artifact store roots %v", r.Ext4Path, roots)
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

	// Serialize concurrent resumes of this fingerprint (callback goroutine +
	// reconciler replay, including DIFFERENT job_ids/artifact_ids from
	// concurrent same-spec builds) and re-check the job state under the lock,
	// so a duplicate or late BUILT report cannot flip an already-terminal job
	// back through the pipeline or double-register the artifact (which would
	// regenerate DownloadToken and break the in-flight download of the first
	// attempt, or hit the fingerprint unique index).
	release := acquireResumeArtifactLock(result.TemplateSpecFingerprint)
	defer release()

	job, err := getTemplateImageJobRecordByID(ctx, jobID)
	if err != nil {
		return fmt.Errorf("load job %s: %w", jobID, err)
	}

	// State guard: resume is only meaningful for a job still awaiting the
	// post-build pipeline. A job already terminal (READY/FAILED) or already
	// resumed (a previous attempt moved it past BUILT) must not re-run. The
	// callback applies TC's status update before spawning resume, so a freshly
	// reported job reads BUILT here; anything else is a duplicate/stale report.
	switch job.Status {
	case JobStatusBuilt:
		// Expected state: proceed.
	case JobStatusRunning:
		// A resume is already in progress elsewhere (it set phase past BUILT but
		// has not finished); the per-job lock above serializes us behind it, so
		// reaching here means it already completed. Skip.
		log.G(ctx).Infof("resume remote-built template job: job_id=%s already running past BUILT, skipping duplicate resume", jobID)
		return nil
	default:
		log.G(ctx).Warnf("resume remote-built template job: job_id=%s status=%s is not resumable, ignoring stale/duplicate BUILT report", jobID, job.Status)
		return nil
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
	// Hard invariant: the decoded snapshot's template_id MUST equal the
	// job row's own template_id. This is the exact class of bug that
	// previously orphaned a job's template_id (unmarshalTemplateImageJobRequest
	// silently minting a second, different ID) while the real definition +
	// replicas got registered under the regenerated ID instead. Fail loudly
	// here rather than letting registerRemoteBuiltArtifact silently write the
	// definition/replicas under a mismatched template_id again in the future.
	if decodedID := strings.TrimSpace(req.TemplateID); decodedID != strings.TrimSpace(job.TemplateID) {
		failErr := fmt.Errorf("decoded request template_id %q does not match job %s template_id %q; refusing to resume to avoid orphaning the template", decodedID, jobID, job.TemplateID)
		log.G(ctx).Errorf("%v", failErr)
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
	artifact, generatedReq, adopted, err := registerRemoteBuiltArtifact(ctx, req, result)
	if err != nil {
		if errors.Is(err, errArtifactRegisterRetryable) {
			// Lock contention or a crashed predecessor's row: leave the job in
			// BUILT — the image-job reconciler replays the resume later.
			logger.Warnf("resume step 1/3 deferred: %v", err)
			return err
		}
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

	// Step 3: distribute to nodes. builtFreshArtifact is true only when THIS
	// job's build produced the artifact; an adopted row belongs to a peer
	// build whose replicas may already reference it, so a distribution
	// failure here must not clean it up.
	logger.Infof("resume step 2/3: artifact registered, entering distribution: artifact_status=%s ext4_size_bytes=%d adopted=%v",
		artifact.Status, artifact.Ext4SizeBytes, adopted)
	err = finishTemplateImageJobAfterArtifact(ctx, jobID, req, artifact, generatedReq, !adopted)
	if err != nil {
		logger.Errorf("resume step 3/3 failed: %v", err)
		return err
	}
	logger.Infof("resume step 3/3: template is READY")
	return nil
}

// registerRemoteBuiltArtifact registers the artifact TC reported in the
// BUILT callback, instead of building the ext4 locally.
//
// The claim+lookup runs under the cluster-wide fingerprint lock
// (withArtifactRegisterLock, same named lock TC builds hold), and the lookup
// is fingerprint-FIRST: two concurrent same-spec builds report BUILT with
// different UUID-suffixed artifact_ids, so an artifact_id-keyed claim cannot
// see the winner's row and the second insert dies on the fingerprint unique
// index (Error 1062 / idx_artifact_fingerprint). Under the lock the loser
// instead ADOPTS the winner's READY row as-is — finalizing it again would
// regenerate download_token and break the winner's in-flight pulls.
//
// adopted=true means the returned artifact was registered by a peer build of
// the same spec: the caller must NOT treat it as freshly built (no cleanup
// of it on distribution failure — the winner's replicas may already exist).
func registerRemoteBuiltArtifact(ctx context.Context, req *types.CreateTemplateFromImageReq, result *RemoteBuildResult) (record *models.RootfsArtifact, generatedReq *types.CreateCubeSandboxReq, adopted bool, err error) {
	fingerprint := result.TemplateSpecFingerprint
	if strings.TrimSpace(fingerprint) == "" {
		return nil, nil, false, fmt.Errorf("remote build result is missing template_spec_fingerprint")
	}

	imageCfg, err := decodeImageConfigJSON(result.ImageConfigJSON)
	if err != nil {
		return nil, nil, false, err
	}

	err = withArtifactRegisterLock(ctx, fingerprint, func(sess *gorm.DB) error {
		existing, findErr := findRootfsArtifactByFingerprintForUpdate(sess, fingerprint)
		switch {
		case findErr == nil && existing.Status == ArtifactStatusReady && !existing.DeletedAt.Valid:
			// Adopt — INCLUDING our own artifact_id: a duplicate BUILT replay
			// (callback + reconciler, possibly on different masters) must not
			// re-finalize an already-READY row and rotate the download_token
			// its in-flight pulls are using.
			record = existing
			adopted = true
			return nil
		case findErr == nil && existing.ArtifactID != result.ArtifactID:
			// A non-READY row under a DIFFERENT artifact_id is a crashed
			// predecessor's half-registration: a live peer finalizes inside
			// this same lock, so one cannot be observed mid-flight. The
			// artifact GC owns that row's lifecycle; leave the job BUILT and
			// let the image-job reconciler replay once the row is resolved.
			return fmt.Errorf("%w: fingerprint %.16s held by non-ready artifact %s (status %s, deleted=%v)",
				errArtifactRegisterRetryable, fingerprint, existing.ArtifactID, existing.Status, existing.DeletedAt.Valid)
		case findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound):
			return fmt.Errorf("lookup artifact by fingerprint: %w", findErr)
		}

		// Our own row (same artifact_id — replay/redo) or no row at all: the
		// pre-existing claim path, under the fingerprint lock.
		claimed, claimErr := claimRootfsArtifactForBuild(ctx, result.ArtifactID, fingerprint, req, result.SourceImageDigest)
		if claimErr != nil {
			// Defense in depth: a 1062 on idx_artifact_fingerprint here means
			// a writer skipped the named lock. Re-read by fingerprint and
			// adopt a READY winner instead of failing the job.
			if isDuplicateKeyError(claimErr) {
				winner, werr := findRootfsArtifactByFingerprintForUpdate(sess, fingerprint)
				if werr == nil && winner.Status == ArtifactStatusReady && !winner.DeletedAt.Valid {
					record = winner
					adopted = true
					return nil
				}
			}
			return fmt.Errorf("claim artifact row: %w", claimErr)
		}
		record = claimed
		if record == nil {
			record, err = getRootfsArtifactByID(ctx, result.ArtifactID)
			if err != nil {
				return fmt.Errorf("load claimed artifact row: %w", err)
			}
		}

		// Finalize INSIDE the register lock (the lock's critical section is
		// claim + finalize, so a peer can never observe a half-registered
		// row). The final UPDATE is additionally a status CAS: a duplicate
		// finalizer that somehow skipped the named lock still loses.
		finRec, finReq, finErr := finalizeRemoteArtifact(ctx, record, req, result, imageCfg)
		if errors.Is(finErr, errArtifactFinalizeLostCAS) {
			winner, werr := getRootfsArtifactByID(ctx, result.ArtifactID)
			if werr == nil && winner.Status == ArtifactStatusReady {
				record = winner
				adopted = true
				return nil
			}
			if werr != nil {
				return fmt.Errorf("finalize lost CAS and reload failed: %w", werr)
			}
			return fmt.Errorf("finalize lost CAS but row %s is %s", result.ArtifactID, winner.Status)
		}
		if finErr != nil {
			return finErr
		}
		record = finRec
		generatedReq = finReq
		return nil
	})
	if err != nil {
		return nil, nil, false, err
	}

	if adopted {
		// Reuse the winner's finalized row verbatim; only the per-job create
		// request is derived fresh, from the winner's metadata and THIS job's
		// request (template_id differs between the two jobs).
		if cfg, derr := decodeImageConfigJSON(record.ImageConfigJSON); derr == nil {
			imageCfg = cfg
		}
		generatedReq, err = generateTemplateCreateRequest(ctx, req, record, imageCfg, record.MasterNodeIP)
		if err != nil {
			return nil, nil, false, fmt.Errorf("generate template create request: %w", err)
		}
		return record, generatedReq, true, nil
	}
	return record, generatedReq, false, nil
}

// finalizeRemoteArtifact mirrors finalizeArtifact but sources its values from
// TC's report rather than a local *image.PreparedSource. Keep the persisted
// column set identical to finalizeArtifact.
func finalizeRemoteArtifact(
	ctx context.Context,
	record *models.RootfsArtifact,
	req *types.CreateTemplateFromImageReq,
	result *RemoteBuildResult,
	imageCfg DockerImageConfig,
) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, error) {
	record.SourceImageDigest = result.SourceImageDigest
	record.MasterNodeIP = result.MasterNodeIP
	record.Ext4Path = result.Ext4Path
	record.Ext4SHA256 = result.Ext4SHA256
	record.Ext4SizeBytes = result.Ext4SizeBytes
	record.ImageConfigJSON = result.ImageConfigJSON
	record.DownloadToken = uuid.New().String()
	record.ArtifactURL = result.ArtifactURL
	record.Status = ArtifactStatusReady
	record.GCDeadline = time.Now().Add(defaultTemplateArtifactTTL).Unix()
	record.CubeEgressCABaked = result.CubeEgressCABaked
	record.CubeEgressCAFingerprint = result.CubeEgressCAFingerprint
	record.CubeEgressCATargetsWritten = result.CubeEgressCATargets

	// Same base URL local mode uses (CubeMaster's own request base URL, echoed
	// back by TC), so the generated request and the distribution annotations
	// are byte-identical across build modes: Cubelet pulls from CubeMaster.
	generatedReq, err := generateTemplateCreateRequest(ctx, req, record, imageCfg, record.MasterNodeIP)
	if err != nil {
		return nil, nil, fmt.Errorf("generate template create request: %w", err)
	}
	reqPayload, err := json.Marshal(generatedReq)
	if err != nil {
		return nil, nil, err
	}
	record.GeneratedRequestJSON = string(reqPayload)

	// Status CAS: the row only becomes READY while it is still the BUILDING
	// row this registration claimed. A duplicate BUILT replay that skipped
	// the named register lock finds the row already READY and loses, so the
	// winner's download_token (already handed to Cubelet pulls) is never
	// rotated underneath it.
	ok, err := updateRootfsArtifactIfStatus(ctx, record.ArtifactID, ArtifactStatusBuilding, map[string]any{
		"source_image_digest":            record.SourceImageDigest,
		"master_node_ip":                 record.MasterNodeIP,
		"ext4_path":                      record.Ext4Path,
		"ext4_sha256":                    record.Ext4SHA256,
		"ext4_size_bytes":                record.Ext4SizeBytes,
		"image_config_json":              record.ImageConfigJSON,
		"generated_request_json":         record.GeneratedRequestJSON,
		"download_token":                 record.DownloadToken,
		"artifact_url":                   record.ArtifactURL,
		"status":                         record.Status,
		"gc_deadline":                    record.GCDeadline,
		"last_error":                     "",
		"cube_egress_ca_baked":           record.CubeEgressCABaked,
		"cube_egress_ca_fingerprint":     record.CubeEgressCAFingerprint,
		"cube_egress_ca_targets_written": record.CubeEgressCATargetsWritten,
	})
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, errArtifactFinalizeLostCAS
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
func decodeImageConfigJSON(payload string) (DockerImageConfig, error) {
	var cfg DockerImageConfig
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
		Config       DockerImageConfig `json:"config"`
		ConfigUpper  DockerImageConfig `json:"Config"`
		ContainerCfg DockerImageConfig `json:"container_config"`
	}
	if err := json.Unmarshal([]byte(payload), &wrapper); err != nil {
		return cfg, fmt.Errorf("decode image_config_json: %w", err)
	}
	for _, candidate := range []DockerImageConfig{wrapper.Config, wrapper.ConfigUpper, wrapper.ContainerCfg} {
		if !imageConfigEmpty(candidate) {
			return candidate, nil
		}
	}
	// A genuinely empty config is legal for some scratch images; return the
	// zero value so the caller can still proceed.
	return cfg, nil
}

func imageConfigEmpty(cfg DockerImageConfig) bool {
	return len(cfg.Entrypoint) == 0 && len(cfg.Cmd) == 0 && len(cfg.Env) == 0 &&
		cfg.WorkingDir == "" && cfg.User == ""
}
