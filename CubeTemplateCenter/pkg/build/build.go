// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package build implements the core template building logic for CubeTemplateCenter.
// It ONLY does the build work (pull image, mkfs ext4) and reports status back
// to CubeMaster via HTTP callback.
package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/cube_egress_ca"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/lock"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/s3store"
	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"gorm.io/gorm"
)

// artifactBuildLocks serializes concurrent builds of the SAME artifact within
// this process. Concurrent same-spec creates share one fingerprint, hence one
// artifactID and one ext4 output path; without this lock the native rootfs
// export races itself (lchown/link/utimes ENOENT) and one build's cleanup
// deletes files another build is still writing.
//
// This lock is kept even when using the cross-replica DB session lock, because
// advisory locks are per DB connection: two goroutines in the same process use
// different connections and could otherwise both acquire the same lock.
var artifactBuildLocks = newKeyedMutex()

// keyedMutex is a per-key mutex set with automatic cleanup of idle entries.
type keyedMutex struct {
	mu    sync.Mutex
	items map[string]*keyedMutexItem
}

type keyedMutexItem struct {
	mu   *sync.Mutex
	refs int
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{items: make(map[string]*keyedMutexItem)}
}

// Lock acquires the mutex for key and returns the unlock function.
func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	it, ok := k.items[key]
	if !ok {
		it = &keyedMutexItem{mu: &sync.Mutex{}}
		k.items[key] = it
	}
	it.refs++
	k.mu.Unlock()

	it.mu.Lock()

	return func() {
		it.mu.Unlock()
		k.mu.Lock()
		it.refs--
		if it.refs == 0 {
			delete(k.items, key)
		}
		k.mu.Unlock()
	}
}

// Build is the TC-only entry point for template building.
//
// What TC does:
//   - Pull image (image.PrepareSource)
//   - Compute fingerprint + artifact ID (same helpers as local mode)
//   - Build ext4 (image.BuildExt4), baking envd + CubeEgress CA into rootfs
//   - Report status back to CubeMaster via HTTP callback
//
// What TC does NOT do (CubeMaster's job):
//   - Parameter validation (instance_type / image_ref legality)
//   - Write image_jobs (status updates arrive via the callback handler)
//   - Write rootfs_artifacts / template_definitions (the BUILT report carries
//     the artifact metadata in result_json; CubeMaster finalizes the record
//     when it resumes the job for distribution)
//   - Distribute artifact to Cubelet nodes
func Build(ctx context.Context, jobID string, req *types.CreateTemplateFromImageReq, downloadBaseURL string, envdSHA string, envdData []byte) error {
	logger := log.G(ctx).WithFields(map[string]any{
		"job_id":      jobID,
		"template_id": req.TemplateID,
		"image":       req.SourceImageRef,
	})

	reporter := NewReporter()
	defer reporter.Close()

	reportPhase := func(phase string, progress int) {
		if err := reporter.Report(ctx, jobID, map[string]any{
			"status":   templatecenter.JobStatusRunning,
			"phase":    phase,
			"progress": progress,
		}); err != nil {
			logger.Warnf("report %s phase fail: %v", phase, err)
		}
	}
	reportFailed := func(phase, errMsg string) {
		if err := reporter.Report(ctx, jobID, map[string]any{
			"status":        templatecenter.JobStatusFailed,
			"phase":         phase,
			"progress":      100,
			"error_message": errMsg,
		}); err != nil {
			logger.Warnf("report FAILED status fail: %v", err)
		}
	}

	// Step 1: Preflight. Assert mkfs.ext4/truncate/cp (and losetup et al. when
	// loop-mount is enabled) exist and that mkfs.ext4 supports -d BEFORE
	// spending minutes pulling an image, so a misconfigured host fails fast
	// at PULLING with a clear message instead of deep inside BUILDING_EXT4.
	reportPhase(templatecenter.JobPhasePulling, 5)
	if err := image.EnsureArtifactBuildPreflight(ctx); err != nil {
		errMsg := fmt.Sprintf("build preflight fail: %v", err)
		logger.Errorf(errMsg)
		reportFailed(templatecenter.JobPhasePulling, errMsg)
		return fmt.Errorf("build preflight: %w", err)
	}

	// Step 2: Pull image. Progress callbacks stream into the shared Redis
	// live-snapshot sink so any CubeMaster replica can serve the progress query.
	pullProgress := newPullProgressSink(ctx, jobID).withReporter(reporter)
	source, err := image.PrepareSource(ctx, image.SourceSpec{
		ImageRef:         req.SourceImageRef,
		RegistryUsername: req.RegistryUsername,
		RegistryPassword: req.RegistryPassword,
		DownloadBaseURL:  downloadBaseURL,
		OnPullProgress:   pullProgress.onProgress,
	})
	if err != nil {
		pullProgress.flush(false)
		errMsg := fmt.Sprintf("pull image fail: %v", err)
		logger.Errorf(errMsg)
		reportFailed(templatecenter.JobPhasePulling, errMsg)
		return fmt.Errorf("pull image: %w", err)
	}
	if source.Cleanup != nil {
		defer source.Cleanup(context.Background())
	}
	// Docker/Podman engine pulls complete inside PrepareSource; dockerless and
	// native modes keep streaming during BuildExt4, so their flush waits.
	pullProgressFlushed := false
	if source.ExportMode == image.ExportModeDocker {
		pullProgress.flush(true)
		pullProgressFlushed = true
	}

	// Step 3: Resolve envd payload (same validation as local mode)
	reportPhase(templatecenter.JobPhaseUnpacking, 20)
	var envdPayload *templatecenter.EnvdInjectionPayload
	if len(envdData) > 0 {
		envdPayload, err = templatecenter.NewEnvdInjectionPayloadFromBytes(envdData)
		if err != nil {
			if !pullProgressFlushed {
				pullProgress.flush(false)
				pullProgressFlushed = true
			}
			errMsg := fmt.Sprintf("validate envd payload fail: %v", err)
			logger.Errorf(errMsg)
			reportFailed(templatecenter.JobPhaseUnpacking, errMsg)
			return fmt.Errorf("validate envd payload: %w", err)
		}
		// Trust the locally computed digest over the caller-supplied one.
		envdSHA = envdPayload.SHA256
	}

	// Step 4: Resolve CubeEgress CA (nil WithCubeCA defaults to true, same as
	// resolveWithCubeCA in local mode)
	withCubeCA := req.WithCubeCA == nil || *req.WithCubeCA
	caPEM, caFingerprint, err := LoadCubeEgressCA(ctx, withCubeCA)
	if err != nil {
		if !pullProgressFlushed {
			pullProgress.flush(false)
			pullProgressFlushed = true
		}
		errMsg := fmt.Sprintf("load cube egress CA fail: %v", err)
		logger.Errorf(errMsg)
		reportFailed(templatecenter.JobPhaseUnpacking, errMsg)
		return fmt.Errorf("load cube egress CA: %w", err)
	}

	// Step 5: Fingerprint + artifact ID (identical to local mode so artifact
	// dedup stays compatible across build modes)
	fingerprint := templatecenter.BuildTemplateSpecFingerprintWithEnvdSHA(req, source.Digest, caFingerprint, envdSHA)
	artifactID := templatecenter.BuildArtifactID(fingerprint)
	if err := reporter.Report(ctx, jobID, map[string]any{
		"artifact_id":               artifactID,
		"template_spec_fingerprint": fingerprint,
		"source_image_digest":       source.Digest,
	}); err != nil {
		logger.Warnf("report fingerprint fail: %v", err)
	}

	// Resolve S3 config once for both reuse and fresh-build paths. This is
	// done before acquiring the artifact lock so the lock-free polling path
	// can still generate presigned URLs when reusing an artifact built by a
	// sibling replica.
	s3Client, s3CfgEnabled := SharedS3Client()
	if !s3CfgEnabled {
		logger.Warnf("s3 client unavailable, falling back to local storage")
	}

	// Step 6: Serialize same-artifact builds, then reuse a finished artifact
	// if a sibling job already produced it while we were waiting.
	//
	// The in-process lock is always taken first: it protects filesystem
	// operations (native rootfs export, cleanup) between goroutines in this
	// process. On top of it, the DB session lock excludes sibling TC replicas
	// when running with more than one replica. DB-less test setups use only
	// the in-process mutex.
	// Serialize concurrent builds of the same spec (fingerprint) within this
	// process; when running with more than one replica. DB-less test setups use
	// only the in-process mutex.
	//
	// The lock key is the fingerprint, NOT the artifactID, because artifactID
	// is unique per build (includes a UUID). Locking by fingerprint ensures
	// that only one build happens for a given spec, even across TC replicas.
	unlockLocal := artifactBuildLocks.Lock(fingerprint)
	defer unlockLocal()

	db := templatecenter.GetDB()
	if db == nil {
		return runBuildLocked(ctx, jobID, req, artifactID, fingerprint, source, reporter, caPEM, caFingerprint, envdPayload, pullProgress, pullProgressFlushed, s3CfgEnabled, s3Client, logger)
	}

	// Cross-instance lock with polling fallback. If another replica is already
	// building this spec, wait briefly and try to reuse its output.
	const lockPollInterval = 5 * time.Second
	for {
		err := lock.WithBuildLock(ctx, db, fingerprint, func() error {
			return runBuildLocked(ctx, jobID, req, artifactID, fingerprint, source, reporter, caPEM, caFingerprint, envdPayload, pullProgress, pullProgressFlushed, s3CfgEnabled, s3Client, logger)
		})
		if !errors.Is(err, lock.ErrBuildInProgress) {
			return err
		}
		logger.Infof("another replica is building spec %s, waiting to reuse", fingerprint[:16])

		if existing, ok := reuseExistingArtifact(ctx, db, fingerprint); ok {
			artifactURL := artifactPresignedURL(ctx, s3CfgEnabled, s3Client, existing.ArtifactID, logger)
			// Construct a BuildResult from the artifact metadata.
			buildResult := &image.BuildResult{
				Ext4Path:  existing.Ext4Path,
				SHA256:    existing.Ext4SHA256,
				SizeBytes: existing.Ext4SizeBytes,
			}
			if err := reportArtifactBuilt(ctx, jobID, buildResult, existing.ArtifactID, fingerprint, source, reporter, cube_egress_ca.Result{
				Baked:       len(caPEM) > 0,
				Fingerprint: caFingerprint,
			}, artifactURL, logger); err != nil {
				return err
			}
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
}

// runBuildLocked performs the reuse check, ext4 conversion, S3 upload and
// BUILT report while holding the per-artifact lock. It is used both by the
// in-process fallback and by the cross-instance DB session lock.
func runBuildLocked(
	ctx context.Context,
	jobID string,
	req *types.CreateTemplateFromImageReq,
	artifactID string,
	fingerprint string,
	source *image.PreparedSource,
	reporter *Reporter,
	caPEM []byte,
	caFingerprint string,
	envdPayload *templatecenter.EnvdInjectionPayload,
	pullProgress *pullProgressSink,
	pullProgressFlushed bool,
	s3CfgEnabled bool,
	s3Client *s3store.Client,
	logger *cubelog.Entry,
) error {
	reportPhase := func(phase string, progress int) {
		if err := reporter.Report(ctx, jobID, map[string]any{
			"status":   templatecenter.JobStatusRunning,
			"phase":    phase,
			"progress": progress,
		}); err != nil {
			logger.Warnf("report %s phase fail: %v", phase, err)
		}
	}
	reportFailed := func(phase, errMsg string) {
		if err := reporter.Report(ctx, jobID, map[string]any{
			"status":        templatecenter.JobStatusFailed,
			"phase":         phase,
			"progress":      100,
			"error_message": errMsg,
		}); err != nil {
			logger.Warnf("report FAILED status fail: %v", err)
		}
	}

	if db := templatecenter.GetDB(); db != nil {
		if existing, ok := reuseExistingArtifact(ctx, db, fingerprint); ok {
			logger.Infof("artifact already built by a sibling job, reusing: artifact_id=%s path=%s", existing.ArtifactID, existing.Ext4Path)
			// The reused ext4 already contains the CA baked at build time; report
			// the fingerprint we resolved so CubeMaster records it consistently.
			artifactURL := artifactPresignedURL(ctx, s3CfgEnabled, s3Client, existing.ArtifactID, logger)
			buildResult := &image.BuildResult{
				Ext4Path:  existing.Ext4Path,
				SHA256:    existing.Ext4SHA256,
				SizeBytes: existing.Ext4SizeBytes,
			}
			return reportArtifactBuilt(ctx, jobID, buildResult, existing.ArtifactID, fingerprint, source, reporter, cube_egress_ca.Result{
				Baked:       len(caPEM) > 0,
				Fingerprint: caFingerprint,
			}, artifactURL, logger)
		}
	}

	// Step 7: Build ext4 (export rootfs, bake envd + CA, mkfs)
	reportPhase(templatecenter.JobPhaseBuildingExt4, 40)
	var caBakeResult cube_egress_ca.Result
	opts := image.BuildOptions{ArtifactID: artifactID}
	opts.PostRootfsExport = func(ctx context.Context, rootfsDir string) error {
		if _, err := templatecenter.InjectEnvdPayloadIntoRootfs(ctx, rootfsDir, envdPayload); err != nil {
			return err
		}
		if envdPayload != nil {
			envdPayload.ReleaseData()
		}
		var err error
		caBakeResult, err = ApplyCubeEgressCAToRootfs(ctx, rootfsDir, caPEM, caFingerprint)
		return err
	}
	result, err := image.BuildExt4(ctx, source, opts)
	if !pullProgressFlushed {
		// Dockerless / native modes stream pull progress during BuildExt4, so
		// flush only once all pull callbacks can no longer fire.
		pullProgress.flush(err == nil)
		pullProgressFlushed = true
	}
	if err != nil {
		errMsg := fmt.Sprintf("build ext4 fail: %v", err)
		logger.Errorf(errMsg)
		// Remove the half-written store dir so a failed build does not leak
		// disk. BuildExt4 cleans up on its own error paths, but a partially
		// created dir (or a PostRootfsExport failure) can survive.
		if cleanupErr := cleanupArtifactResidue(ctx, artifactID); cleanupErr != nil {
			logger.Warnf("cleanup artifact residue after failed build: %v", cleanupErr)
		}
		reportFailed(templatecenter.JobPhaseBuildingExt4, errMsg)
		return fmt.Errorf("build ext4: %w", err)
	}

	// Step 8: Upload to S3/MinIO when configured, then report BUILT.
	artifactURL := ""
	if s3CfgEnabled && s3Client != nil {
		if _, err := s3Client.Upload(ctx, artifactID, result.Ext4Path); err != nil {
			logger.Warnf("upload artifact to s3 fail, falling back to local storage: %v", err)
		} else {
			artifactURL = artifactPresignedURL(ctx, s3CfgEnabled, s3Client, artifactID, logger)
		}
	}

	// Step 9: Report BUILT. CubeMaster persists the payload into result_json
	// and resumes the job: finalize rootfs_artifacts, distribute to Cubelet
	// nodes, register template_definitions.
	if err := reportArtifactBuilt(ctx, jobID, &result, artifactID, fingerprint, source, reporter, caBakeResult, artifactURL, logger); err != nil {
		return err
	}

	logger.Infof("template build completed: artifact_id=%s sha256=%s size=%d",
		artifactID, result.SHA256, result.SizeBytes)
	return nil
}

// reportArtifactBuilt emits the BUILT callback to CubeMaster.
func reportArtifactBuilt(
	ctx context.Context,
	jobID string,
	result *image.BuildResult,
	artifactID string,
	fingerprint string,
	source *image.PreparedSource,
	reporter *Reporter,
	caResult cube_egress_ca.Result,
	artifactURL string,
	logger *cubelog.Entry,
) error {
	// Two fields deserve explanation:
	//
	// image_config_json lets CubeMaster generate the template's
	// create-sandbox request (Entrypoint/Cmd/Env/WorkingDir/User) without
	// re-inspecting the image: TC pulled it, so TC reports it.
	//
	// master_node_ip is misleadingly named: it holds the artifact DOWNLOAD
	// BASE URL (image.PrepareSource sets it to
	// NormalizeBaseURL(spec.DownloadBaseURL)). CubeMaster passed its own
	// request base URL down when submitting the job, so echoing it back
	// keeps the data plane identical to local mode: Cubelet pulls the ext4
	// from CubeMaster, not from TC. distributeRootfsArtifact rejects an
	// empty value, so it must be reported.
	payload := map[string]any{
		"status":                         templatecenter.JobStatusBuilt,
		"phase":                          templatecenter.JobPhaseReady,
		"progress":                       100,
		"artifact_id":                    artifactID,
		"artifact_status":                templatecenter.ArtifactStatusReady,
		"template_spec_fingerprint":      fingerprint,
		"source_image_digest":            source.Digest,
		"ext4_path":                      result.Ext4Path,
		"ext4_sha256":                    result.SHA256,
		"ext4_size_bytes":                result.SizeBytes,
		"image_config_json":              source.ConfigJSON,
		"master_node_ip":                 source.MasterNodeIP,
		"cube_egress_ca_baked":           caResult.Baked,
		"cube_egress_ca_fingerprint":     caResult.Fingerprint,
		"cube_egress_ca_targets_written": caResult.TargetsWritten,
	}
	if artifactURL != "" {
		payload["artifact_url"] = artifactURL
	}
	if err := reporter.Report(ctx, jobID, payload); err != nil {
		logger.Errorf("report BUILT status fail: %v", err)
		return fmt.Errorf("report BUILT status: %w", err)
	}
	return nil
}

// artifactPresignedURL returns a presigned URL for an S3-backed artifact, or
// an empty string when S3 is disabled or URL generation fails.
func artifactPresignedURL(ctx context.Context, s3CfgEnabled bool, s3Client *s3store.Client, artifactID string, logger *cubelog.Entry) string {
	if !s3CfgEnabled || s3Client == nil {
		return ""
	}
	url, err := s3Client.PresignedGetURL(ctx, artifactID)
	if err != nil {
		logger.Warnf("generate s3 presigned url fail: %v", err)
		return ""
	}
	return url
}

// cleanupArtifactResidue removes the work dir and the artifact store dir for a
// failed build so a retry starts from a clean slate and disk is not leaked.
// TC owns no DB rows, so this is purely filesystem cleanup.
func cleanupArtifactResidue(ctx context.Context, artifactID string) error {
	var errs []string

	workDir := filepath.Join(image.ArtifactWorkRootDir(), artifactID)
	if err := os.RemoveAll(workDir); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Sprintf("remove work dir %s: %v", workDir, err))
	}

	storeDir, err := image.ResolveArtifactStoreDir(ctx, artifactID)
	switch {
	case err != nil:
		errs = append(errs, fmt.Sprintf("resolve store dir: %v", err))
	case isBuildInProgress(storeDir):
		// Another build already owns this directory. The per-artifact lock keeps
		// TC's own builds apart, but CubeMaster may have started a local build
		// for the same fingerprint in its own process, and the native exporter
		// keeps its layer prefetch dir in here. Wiping it would break that
		// build with a bogus "prefetched layer ... no such file" error, so the
		// residue is left for whoever finishes last.
		log.G(ctx).Warnf("skip removing artifact store dir %s: %s",
			storeDir, image.DescribeArtifactBuildMarker(storeDir))
	default:
		if err := os.RemoveAll(storeDir); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Sprintf("remove store dir %s: %v", storeDir, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// isBuildInProgress reports whether another build currently owns an artifact
// directory. TC's own build has already released its marker by the time the
// residue cleanup runs, so a live marker here always belongs to someone else.
func isBuildInProgress(storeDir string) bool {
	inProgress, _ := image.ArtifactBuildInProgress(storeDir)
	return inProgress
}

// reuseExistingArtifact queries the DB for a READY artifact with the given
// fingerprint. If found, it returns the artifact metadata (including the
// artifactID, which is unique per build and includes a UUID). This is used
// by the cross-replica deduplication logic: if another TC replica just
// finished building the same spec, we reuse its output instead of building
// again.
//
// The query checks:
//   - template_spec_fingerprint matches
//   - status is READY
//   - the ext4 file exists on disk (defensive check; the file may have been
//     cleaned up by GC or manual intervention)
func reuseExistingArtifact(ctx context.Context, db *gorm.DB, fingerprint string) (*models.RootfsArtifact, bool) {
	var artifact models.RootfsArtifact
	err := db.WithContext(ctx).
		Table(constants.RootfsArtifactTableName).
		Where("template_spec_fingerprint = ?", fingerprint).
		Where("status = ?", "READY").
		Where("deleted_at IS NULL").
		Order("created_at DESC").
		First(&artifact).Error
	if err != nil {
		return nil, false
	}

	// Defensive check: verify the ext4 file still exists.
	if _, err := os.Stat(artifact.Ext4Path); err != nil {
		log.G(ctx).Warnf("reuse check: artifact %s is READY but ext4 file %s is missing: %v", artifact.ArtifactID, artifact.Ext4Path, err)
		return nil, false
	}

	log.G(ctx).Infof("reuse check: found READY artifact %s for fingerprint %s (ext4: %s, %d bytes)",
		artifact.ArtifactID, fingerprint[:16], artifact.Ext4Path, artifact.Ext4SizeBytes)
	return &artifact, true
}

// fileSHA256 streams the file through sha256 (same algorithm as the build path).
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
