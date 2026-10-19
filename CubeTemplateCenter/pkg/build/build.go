// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package build implements the core template building logic for CubeTemplateCenter.
// It ONLY does the build work (pull image, build rootfs) and reports status back
// to CubeMaster via HTTP callback.
package build

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/tcconfig"
	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
	imagev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
	"gorm.io/gorm"
)

// artifactBuildLocks serializes concurrent same-spec builds (fingerprint) in
// this process. The DB advisory lock is per connection, so two goroutines
// here could otherwise both hold it.
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
//   - Build rootfs (image.BuildRootfs), baking envd + CubeEgress CA into rootfs
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
	// at PULLING with a clear message instead of deep inside BUILDING_ROOTFS.
	reportPhase(templatecenter.JobPhasePulling, 5)
	if err := image.EnsureRootfsBuildPreflight(ctx, req.RootfsFormat); err != nil {
		errMsg := fmt.Sprintf("build preflight fail: %v", err)
		logger.Errorf(errMsg)
		reportFailed(templatecenter.JobPhasePulling, errMsg)
		return fmt.Errorf("build preflight: %w", err)
	}
	// Resolve the shared store before pulling. An explicitly selected fs store
	// cannot fall back to local-only artifacts when opening it fails.
	s3Client, s3CfgEnabled := SharedS3Client()
	if !s3CfgEnabled && tcconfig.ArtifactStoreBackend() == "fs" {
		err := fmt.Errorf("CUBE_ARTIFACT_STORE_BACKEND=fs but the artifact store failed to open")
		reportFailed(templatecenter.JobPhasePulling, err.Error())
		return err
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
	// native modes keep streaming during BuildRootfs, so their flush waits.
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

	// Step 6: Serialize same-spec builds (fingerprint, not artifactID — the
	// latter includes a UUID). The in-process lock protects filesystem work
	// between goroutines; the DB session lock excludes sibling TC replicas.
	// DB-less tests use only the in-process mutex. After waiting, reuse a
	// READY artifact another job already produced.
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

		if existing, ok := reuseExistingArtifact(ctx, db, fingerprint, s3CfgEnabled, s3Client); ok {
			return reportExistingArtifact(ctx, jobID, existing, fingerprint, source, reporter, caPEM, caFingerprint, s3CfgEnabled, s3Client, logger)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
}

// runBuildLocked performs the reuse check, rootfs construction, S3 upload and
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
		if existing, ok := reuseExistingArtifact(ctx, db, fingerprint, s3CfgEnabled, s3Client); ok {
			logger.Infof("artifact already built by a sibling job, reusing: artifact_id=%s path=%s", existing.ArtifactID, existing.RootfsPath)
			// The reused rootfs already contains the CA baked at build time; report
			// the fingerprint we resolved so CubeMaster records it consistently.
			return reportExistingArtifact(ctx, jobID, existing, fingerprint, source, reporter, caPEM, caFingerprint, s3CfgEnabled, s3Client, logger)
		}
	}

	// Step 7: Build rootfs (export rootfs, bake envd + CA, mkfs)
	reportPhase(templatecenter.JobPhaseBuildingRootfs, 40)
	var caBakeResult cube_egress_ca.Result
	opts := image.BuildOptions{ArtifactID: artifactID, RootfsFormat: req.RootfsFormat}
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
	result, err := image.BuildRootfs(ctx, source, opts)
	if !pullProgressFlushed {
		// Dockerless / native modes stream pull progress during BuildRootfs, so
		// flush only once all pull callbacks can no longer fire.
		pullProgress.flush(err == nil)
		pullProgressFlushed = true
	}
	if err != nil {
		errMsg := fmt.Sprintf("build rootfs fail: %v", err)
		logger.Errorf(errMsg)
		// Remove the half-written store dir so a failed build does not leak
		// disk. BuildRootfs cleans up on its own error paths, but a partially
		// created dir (or a PostRootfsExport failure) can survive.
		if cleanupErr := cleanupArtifactResidue(ctx, artifactID); cleanupErr != nil {
			logger.Warnf("cleanup artifact residue after failed build: %v", cleanupErr)
		}
		reportFailed(templatecenter.JobPhaseBuildingRootfs, errMsg)
		return fmt.Errorf("build rootfs: %w", err)
	}

	// Step 8: Upload to the configured blob backend, then report BUILT.
	// backend=s3 keeps today's fallback-to-local-disk on upload failure.
	// backend=fs is explicit: a failed put is a failed build.
	var location artifactStoreLocation
	if s3CfgEnabled && s3Client != nil {
		objectKey, err := s3Client.Upload(ctx, s3Client.FullObjectKey(filepath.Base(result.RootfsPath)), result.RootfsPath, result.SHA256)
		if err != nil {
			if s3Client.BackendName() == "fs" {
				reportFailed(templatecenter.JobPhaseBuildingRootfs, fmt.Sprintf("upload artifact to fs store: %v", err))
				return fmt.Errorf("upload artifact to fs store: %w", err)
			}
			logger.Warnf("upload artifact to s3 fail, falling back to local storage: %v", err)
		} else {
			location = storedArtifactLocation(ctx, s3Client, objectKey, logger)
		}
	}

	// Step 9: Report BUILT. CubeMaster persists the payload into result_json
	// and resumes the job: finalize rootfs_artifacts, distribute to Cubelet
	// nodes, register template_definitions.
	if err := reportArtifactBuilt(ctx, jobID, &result, artifactID, fingerprint, source, reporter, caBakeResult, location, logger); err != nil {
		return err
	}

	logger.Infof("template build completed: artifact_id=%s sha256=%s size=%d",
		artifactID, result.SHA256, result.SizeBytes)
	return nil
}

func reportExistingArtifact(
	ctx context.Context,
	jobID string,
	existing *models.RootfsArtifact,
	fingerprint string,
	source *image.PreparedSource,
	reporter *Reporter,
	caPEM []byte,
	caFingerprint string,
	s3CfgEnabled bool,
	s3Client *s3store.Client,
	logger *cubelog.Entry,
) error {
	objectKey := templatecenter.ArtifactObjectKey(existing)
	if existing.ObjectKey == "" && s3Client != nil {
		objectKey = s3Client.FullObjectKey(objectKey)
	}
	var location artifactStoreLocation
	if s3CfgEnabled && s3Client != nil {
		location = findStoredArtifact(ctx, s3Client, objectKey, logger)
	}
	return reportArtifactBuilt(ctx, jobID, &image.BuildResult{
		RootfsFormat: existing.RootfsFormat,
		RootfsPath:   existing.RootfsPath,
		SHA256:       existing.RootfsSHA256,
		SizeBytes:    existing.RootfsSizeBytes,
	}, existing.ArtifactID, fingerprint, source, reporter, cube_egress_ca.Result{
		Baked:       len(caPEM) > 0,
		Fingerprint: caFingerprint,
	}, location, logger)
}

// artifactStoreLocation describes a confirmed durable copy, independently of
// whether a download URL could be signed for it.
type artifactStoreLocation struct {
	URL       string
	Backend   string
	ObjectKey string
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
	location artifactStoreLocation,
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
	// keeps the data plane identical to local mode: Cubelet pulls the rootfs
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
		"image_config_json":              source.ConfigJSON,
		"master_node_ip":                 source.MasterNodeIP,
		"cube_egress_ca_baked":           caResult.Baked,
		"cube_egress_ca_fingerprint":     caResult.Fingerprint,
		"cube_egress_ca_targets_written": caResult.TargetsWritten,
	}
	imagev1.AddRootfsMetadata(payload, imagev1.RootfsMetadata{Format: result.RootfsFormat, Path: result.RootfsPath, SHA256: result.SHA256, SizeBytes: result.SizeBytes})
	if location.URL != "" {
		payload["artifact_url"] = location.URL
	}
	// Only stamp backend/key after a successful upload (or reuse of an
	// already-stored object). A configured client that failed Put must
	// leave the three store columns empty so Master stays on local disk.
	if location.ObjectKey != "" {
		payload["storage_backend"] = location.Backend
		payload["object_key"] = location.ObjectKey
	}
	if err := reporter.Report(ctx, jobID, payload); err != nil {
		logger.Errorf("report BUILT status fail: %v", err)
		return fmt.Errorf("report BUILT status: %w", err)
	}
	return nil
}

// artifactS3Ops is the subset of s3store.Client used to resolve stored artifacts;
// an interface so tests can fake the bucket.
type artifactS3Ops interface {
	Stat(ctx context.Context, objectKey string) (bool, error)
	PresignedGetURL(ctx context.Context, objectKey string) (string, error)
	BackendName() string
}

// findStoredArtifact confirms the durable copy before reporting its location.
//
// The Stat guard matters on the REUSE path: reuseExistingArtifact can match a
// legacy artifact that predates S3 (built local-only, artifact_url empty,
// rootfs still on disk). Presigning a URL for an object that was never uploaded
// hands Cubelets a signed URL that 404s (NoSuchKey) -- worse than no URL at
// all, because the row then looks S3-backed and the local-file serving path
// is skipped. An empty location keeps the artifact on the local-disk download path
// (and `tpl merge` can migrate it to S3 later).
func findStoredArtifact(ctx context.Context, store artifactS3Ops, objectKey string, logger *cubelog.Entry) artifactStoreLocation {
	exists, err := store.Stat(ctx, objectKey)
	if err != nil {
		logger.Warnf("stat s3 object before presign fail: %v", err)
		return artifactStoreLocation{}
	}
	if !exists {
		logger.Warnf("artifact %s has no s3 object (predates s3 or was never uploaded); not presigning, keeping the local-disk download path", objectKey)
		return artifactStoreLocation{}
	}
	return storedArtifactLocation(ctx, store, objectKey, logger)
}

// storedArtifactLocation is used after a successful upload or existence check.
// A signing failure must not discard the confirmed backend and object key.
func storedArtifactLocation(ctx context.Context, store artifactS3Ops, objectKey string, logger *cubelog.Entry) artifactStoreLocation {
	location := artifactStoreLocation{Backend: store.BackendName(), ObjectKey: objectKey}
	url, err := store.PresignedGetURL(ctx, objectKey)
	if err != nil {
		if !errors.Is(err, blobstore.ErrUnsupported) {
			logger.Warnf("generate s3 presigned url fail: %v", err)
		}
		return location
	}
	if blobstore.IsObjectLocator(url) {
		return location
	}
	location.URL = url
	return location
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

// reuseExistingArtifact returns a READY artifact for fingerprint when its
// data is still present (object store HEAD, or local rootfs). Used so a
// sibling replica's just-finished build is reused instead of rebuilt.
func reuseExistingArtifact(ctx context.Context, db *gorm.DB, fingerprint string, s3CfgEnabled bool, s3Client *s3store.Client) (*models.RootfsArtifact, bool) {
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

	var statS3 func() (bool, error)
	if s3CfgEnabled && s3Client != nil {
		statS3 = func() (bool, error) { return s3Client.Stat(ctx, templatecenter.ArtifactObjectKey(&artifact)) }
	}
	reason, ok := artifactDataExists(&artifact, statS3)
	if !ok {
		log.G(ctx).Warnf("reuse check: artifact %s is READY but data is missing: %s", artifact.ArtifactID, reason)
		return nil, false
	}

	log.G(ctx).Infof("reuse check: found READY artifact %s for fingerprint %s (%s)",
		artifact.ArtifactID, fingerprint[:16], reason)
	return &artifact, true
}

// artifactDataExists decides whether a READY artifact's underlying data is
// still present, and returns a short human-readable reason for logging.
//
//   - Object-store artifacts (StorageBackend or ArtifactURL set) are verified
//     via statS3 (a HEAD against the bucket), NOT os.Stat. Multiple TC
//     replicas do not necessarily share local disk when S3/MinIO is
//     configured -- checking only os.Stat(RootfsPath) meant a replica other
//     than the one that built the artifact would always see the local file
//     "missing" and silently fall through to a full rebuild.
//   - If the HEAD check itself errors (transient network issue, not
//     necessarily a missing object), fall back to os.Stat rather than
//     forcing an unnecessary rebuild when a valid local copy exists on this
//     replica.
//   - Local-only artifacts (no ArtifactURL, or S3 not configured on this
//     replica) are verified with os.Stat only, since that IS the artifact's
//     only copy in that mode.
func artifactDataExists(artifact *models.RootfsArtifact, statS3 func() (bool, error)) (string, bool) {
	if templatecenter.ArtifactUsesObjectStore(artifact) && statS3 != nil {
		exists, err := statS3()
		switch {
		case err == nil && exists:
			return "s3 object confirmed", true
		case err == nil && !exists:
			return "s3 object missing", false
		default:
			// err != nil: fall through to the local-disk check below.
		}
	}

	if _, err := os.Stat(artifact.RootfsPath); err != nil {
		return fmt.Sprintf("rootfs file %s missing: %v", artifact.RootfsPath, err), false
	}
	return fmt.Sprintf("rootfs (%s): %s, %d bytes", artifact.RootfsFormat, artifact.RootfsPath, artifact.RootfsSizeBytes), true
}
