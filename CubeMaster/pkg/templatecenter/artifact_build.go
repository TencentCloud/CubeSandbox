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

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/cube_egress_ca"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/image"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// artifactBuildLocks is a same-process optimization for callers targeting the
// same deterministic artifact ID. Cross-CubeMaster exclusion is enforced by
// the durable lease/generation fields in claimRootfsArtifactForBuild.
var artifactBuildLocks sync.Map // map[string]*sync.Mutex

const (
	rootfsArtifactBuildLeaseTTL          = 15 * time.Minute
	rootfsArtifactBuildRenewEvery        = time.Minute
	rootfsArtifactBuildWaitEvery         = 500 * time.Millisecond
	rootfsArtifactBuildStateWriteTimeout = 10 * time.Second
	rootfsArtifactClaimMaxAttempts       = 8
)

var errRootfsArtifactBuildLeaseLost = errors.New("rootfs artifact build lease lost")

type publishedArtifactFinalizeError struct {
	publishedPath string
	cause         error
}

func (e *publishedArtifactFinalizeError) Error() string { return e.cause.Error() }
func (e *publishedArtifactFinalizeError) Unwrap() error { return e.cause }

var finalizeRootfsArtifactRecord = func(ctx context.Context, record *models.RootfsArtifact, values map[string]any) error {
	if record == nil || record.BuildOwnerToken == "" || record.BuildGeneration < 1 {
		return errRootfsArtifactBuildLeaseLost
	}
	tx := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ? AND status = ? AND build_owner_token = ? AND build_generation = ? AND build_lease_expire_at > ?",
			record.ArtifactID, ArtifactStatusBuilding, record.BuildOwnerToken, record.BuildGeneration, time.Now().Unix()).
		Updates(values)
	if tx.Error == nil && tx.RowsAffected == 1 {
		return nil
	}

	// A transport error does not prove that the UPDATE was rolled back: the
	// database may have committed READY before the response was lost. Re-read
	// the fenced generation on a detached context before reporting failure so
	// callers never delete a generation that durable metadata already uses.
	verifyCtx, verifyCancel := context.WithTimeout(context.WithoutCancel(ctx), rootfsArtifactBuildStateWriteTimeout)
	defer verifyCancel()
	committed, verifyErr := rootfsArtifactFinalizeCommitted(verifyCtx, record)
	if committed {
		return nil
	}
	if tx.Error != nil {
		if verifyErr != nil {
			return errors.Join(tx.Error, fmt.Errorf("verify rootfs artifact finalization: %w", verifyErr))
		}
		return tx.Error
	}
	if verifyErr != nil {
		return errors.Join(errRootfsArtifactBuildLeaseLost, fmt.Errorf("verify rootfs artifact finalization: %w", verifyErr))
	}
	return errRootfsArtifactBuildLeaseLost
}

func rootfsArtifactFinalizeCommitted(ctx context.Context, intended *models.RootfsArtifact) (bool, error) {
	var current models.RootfsArtifact
	err := store.db.WithContext(ctx).Unscoped().
		Select("artifact_id", "status", "build_owner_token", "build_generation", "ext4_path", "ext4_sha256", "ext4_size_bytes", "generated_request_json", "download_token").
		Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ?", intended.ArtifactID).
		First(&current).Error
	if err != nil {
		return false, err
	}
	return rootfsArtifactFinalizationMatches(&current, intended), nil
}

func rootfsArtifactFinalizationMatches(current, intended *models.RootfsArtifact) bool {
	return current != nil && intended != nil &&
		current.ArtifactID == intended.ArtifactID &&
		current.Status == ArtifactStatusReady &&
		current.BuildOwnerToken == intended.BuildOwnerToken &&
		current.BuildGeneration == intended.BuildGeneration &&
		current.Ext4Path == intended.Ext4Path &&
		current.Ext4SHA256 == intended.Ext4SHA256 &&
		current.Ext4SizeBytes == intended.Ext4SizeBytes &&
		current.GeneratedRequestJSON == intended.GeneratedRequestJSON &&
		current.DownloadToken == intended.DownloadToken
}

func ensureRootfsArtifact(ctx context.Context, req *types.CreateTemplateFromImageReq, source *image.PreparedSource, downloadBaseURL string) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, bool, error) {
	var generatedReq *types.CreateCubeSandboxReq
	withCubeCA := resolveWithCubeCA(req.WithCubeCA)
	caPEM, caFingerprint, err := loadCubeEgressCA(ctx, withCubeCA)
	if err != nil {
		return nil, nil, false, err
	}
	fingerprint := buildTemplateSpecFingerprintWithCA(req, source.Digest, caFingerprint)
	artifactID := buildArtifactID(fingerprint)
	// Avoid duplicate polling/waiting inside this process; the DB lease below
	// is still authoritative across CubeMaster replicas.
	muV, _ := artifactBuildLocks.LoadOrStore(artifactID, &sync.Mutex{})
	mu := muV.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	for {
		record, wasDeleted, findErr := findReusableRootfsArtifact(ctx, fingerprint, artifactID)
		if findErr == nil && wasDeleted {
			if restoreErr := restoreRootfsArtifact(ctx, artifactID); restoreErr != nil {
				return nil, nil, false, restoreErr
			}
			record.DeletedAt = gorm.DeletedAt{}
		}
		if findErr == nil && record.Status == ArtifactStatusReady && record.GeneratedRequestJSON != "" {
			generatedReq, err = generateTemplateCreateRequest(req, record, source.Config, downloadBaseURL)
			if err == nil {
				return record, generatedReq, false, nil
			}
		}
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return nil, nil, false, findErr
		}

		// Claim the row under its FOR UPDATE lock. The returned ownership bit is
		// deliberately process-independent: a BUILDING row with a live lease is
		// observed and waited on, not rebuilt by another CubeMaster replica.
		claimed, owner, claimErr := claimRootfsArtifactForBuild(ctx, artifactID, fingerprint, req, source.Digest)
		if claimErr != nil {
			return nil, nil, false, claimErr
		}
		if !owner {
			if claimed.Status == ArtifactStatusReady && claimed.GeneratedRequestJSON != "" {
				generatedReq, err = generateTemplateCreateRequest(req, claimed, source.Config, downloadBaseURL)
				if err == nil {
					return claimed, generatedReq, false, nil
				}
			}
			if err := waitForRootfsArtifactBuild(ctx, artifactID, fingerprint, claimed.BuildGeneration); err != nil {
				return nil, nil, false, err
			}
			continue
		}

		buildCtx, stopLease := startRootfsArtifactBuildLease(ctx, claimed)
		record, generatedReq, err = buildRootfsArtifact(buildCtx, claimed, req, source, downloadBaseURL, caPEM, caFingerprint)
		leaseErr := stopLease()
		if err == nil && leaseErr != nil {
			err = leaseErr
		}
		if err != nil {
			failureCtx, failureCancel := context.WithTimeout(context.WithoutCancel(ctx), rootfsArtifactBuildStateWriteTimeout)
			markErr := markRootfsArtifactBuildFailed(failureCtx, claimed, err)
			failureCancel()
			// A successful fenced FAILED transition proves that a preceding
			// ambiguous finalize did not commit READY: both updates require the
			// same BUILDING generation. Only then is its published file safe to
			// remove. If marking fails or loses the fence, preserve the immutable
			// generation for READY readers or later lifecycle GC.
			err = cleanupPublishedArtifactAfterFailedFinalize(ctx, claimed, err, markErr)
			if markErr != nil && !errors.Is(markErr, errRootfsArtifactBuildLeaseLost) {
				err = errors.Join(err, fmt.Errorf("mark rootfs artifact build failed: %w", markErr))
			}
			return nil, nil, false, err
		}
		return record, generatedReq, true, nil
	}
}

// claimRootfsArtifactForBuild atomically ensures the artifact row exists and is
// marked BUILDING while holding its FOR UPDATE lock. It resurrects a
// soft-deleted or CLEANUP_PENDING row (raced with a concurrent
// last-owner-cleanup) instead of letting the build proceed against a row that
// is about to vanish. Because the deleter takes the same row lock in both its
// decision (TX1) and finalisation (TX2) phases, after this commit the deleter's
// phase-3 re-check observes a live BUILDING row plus the active build job and
// backs off without deleting or overwriting the in-flight build status.
func claimRootfsArtifactForBuild(ctx context.Context, artifactID, fingerprint string, req *types.CreateTemplateFromImageReq, sourceDigest string) (*models.RootfsArtifact, bool, error) {
	for attempt := 0; attempt < rootfsArtifactClaimMaxAttempts; attempt++ {
		claimed, owner, err := claimRootfsArtifactForBuildOnce(ctx, artifactID, fingerprint, req, sourceDigest)
		if !isRetryableRootfsArtifactClaimError(err) {
			return claimed, owner, err
		}
		if attempt == rootfsArtifactClaimMaxAttempts-1 {
			return nil, false, fmt.Errorf("claim rootfs artifact after %d attempts: %w", rootfsArtifactClaimMaxAttempts, err)
		}
		// Concurrent first claims can both observe an absent row before one
		// inserts it. PostgreSQL normally reports a duplicate key; InnoDB can
		// instead abort one transaction with a deadlock/lock-timeout while both
		// sessions hold gap locks. Retry either transient result so the loser
		// observes the winner's live lease instead of failing the request.
		delay := 10 * time.Millisecond * time.Duration(1<<min(attempt, 4))
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, false, errors.New("unreachable rootfs artifact claim retry state")
}

func claimRootfsArtifactForBuildOnce(ctx context.Context, artifactID, fingerprint string, req *types.CreateTemplateFromImageReq, sourceDigest string) (*models.RootfsArtifact, bool, error) {
	var (
		claimed *models.RootfsArtifact
		owner   bool
	)
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing models.RootfsArtifact
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Unscoped().
			Table(constants.RootfsArtifactTableName).
			Where("artifact_id = ?", artifactID).First(&existing).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			token := uuid.NewString()
			row := &models.RootfsArtifact{
				ArtifactID:              artifactID,
				TemplateSpecFingerprint: fingerprint,
				SourceImageRef:          req.SourceImageRef,
				SourceImageDigest:       sourceDigest,
				WritableLayerSize:       req.WritableLayerSize,
				Status:                  ArtifactStatusBuilding,
				BuildOwnerToken:         token,
				BuildGeneration:         1,
				BuildLeaseExpireAt:      time.Now().Add(rootfsArtifactBuildLeaseTTL).Unix(),
			}
			if createErr := tx.Table(constants.RootfsArtifactTableName).Create(row).Error; createErr != nil {
				return createErr
			}
			claimed = row
			owner = true
			return nil
		case err != nil:
			return err
		default:
			if _, err := validateReusableRootfsArtifact(&existing, fingerprint, artifactID); err != nil {
				return err
			}
			// A READY row without the serialized Cubelet request is incomplete
			// metadata (for example, from an interrupted legacy build). It cannot
			// be reused and must be claimed for a new generation instead of making
			// followers wait on a state that can never become usable.
			if existing.Status == ArtifactStatusReady && existing.GeneratedRequestJSON != "" && !rootfsArtifactSoftDeleted(&existing) {
				claimed = &existing
				return nil
			}
			now := time.Now()
			if existing.Status == ArtifactStatusBuilding &&
				existing.BuildOwnerToken != "" &&
				existing.BuildLeaseExpireAt > now.Unix() {
				claimed = &existing
				return nil
			}
			generation := existing.BuildGeneration + 1
			if generation < 1 {
				generation = 1
			}
			token := uuid.NewString()
			if updErr := tx.Unscoped().Table(constants.RootfsArtifactTableName).
				Where("artifact_id = ?", artifactID).
				Updates(map[string]any{
					"template_spec_fingerprint": fingerprint,
					"source_image_ref":          req.SourceImageRef,
					"source_image_digest":       sourceDigest,
					"writable_layer_size":       req.WritableLayerSize,
					"status":                    ArtifactStatusBuilding,
					"build_owner_token":         token,
					"build_generation":          generation,
					"build_lease_expire_at":     now.Add(rootfsArtifactBuildLeaseTTL).Unix(),
					"last_error":                "",
					"deleted_at":                nil,
					"updated_at":                time.Now(),
				}).Error; updErr != nil {
				return updErr
			}
			existing.Status = ArtifactStatusBuilding
			existing.DeletedAt = gorm.DeletedAt{}
			existing.BuildOwnerToken = token
			existing.BuildGeneration = generation
			existing.BuildLeaseExpireAt = now.Add(rootfsArtifactBuildLeaseTTL).Unix()
			claimed = &existing
			owner = true
			return nil
		}
	})
	if err != nil {
		return nil, false, err
	}
	return claimed, owner, nil
}

func isRootfsArtifactDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate entry") ||
		strings.Contains(message, "duplicate key") ||
		strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "1062")
}

func isRetryableRootfsArtifactClaimError(err error) bool {
	if isRootfsArtifactDuplicateKeyError(err) {
		return true
	}
	var mysqlErr *mysqldriver.MySQLError
	if errors.As(err, &mysqlErr) && (mysqlErr.Number == 1205 || mysqlErr.Number == 1213) {
		return true
	}
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "error 1205") ||
		strings.Contains(message, "error 1213") ||
		strings.Contains(message, "lock wait timeout exceeded") ||
		strings.Contains(message, "deadlock found when trying to get lock")
}

// waitForRootfsArtifactBuild waits for the current generation to publish or
// lose its lease. Callers loop back through claimRootfsArtifactForBuild after
// this returns so that an expired/failed generation can be safely taken over.
func waitForRootfsArtifactBuild(ctx context.Context, artifactID, fingerprint string, generation int64) error {
	ticker := time.NewTicker(rootfsArtifactBuildWaitEvery)
	defer ticker.Stop()
	for {
		record, _, err := findReusableRootfsArtifact(ctx, fingerprint, artifactID)
		if err == nil {
			switch record.Status {
			case ArtifactStatusReady:
				return nil
			case ArtifactStatusBuilding:
				if record.BuildGeneration != generation || record.BuildLeaseExpireAt <= time.Now().Unix() {
					return nil
				}
			default:
				return nil
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func renewRootfsArtifactBuildLease(ctx context.Context, record *models.RootfsArtifact) error {
	if record == nil || record.BuildOwnerToken == "" || record.BuildGeneration < 1 {
		return errRootfsArtifactBuildLeaseLost
	}
	tx := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ? AND status = ? AND build_owner_token = ? AND build_generation = ? AND build_lease_expire_at > ?",
			record.ArtifactID, ArtifactStatusBuilding, record.BuildOwnerToken, record.BuildGeneration, time.Now().Unix()).
		Updates(map[string]any{
			"build_lease_expire_at": time.Now().Add(rootfsArtifactBuildLeaseTTL).Unix(),
			"updated_at":            time.Now(),
		})
	if tx.Error != nil {
		return tx.Error
	}
	if tx.RowsAffected == 1 {
		return nil
	}
	// MySQL reports zero changed rows when a rapid renewal writes the same
	// second-resolution values. A concurrent READY transition has the same
	// result. Re-read the fence tuple to distinguish both safe outcomes from an
	// actual takeover/expiry; this also closes the renew-vs-finalize race.
	var current models.RootfsArtifact
	if err := store.db.WithContext(ctx).Select("status", "build_owner_token", "build_generation", "build_lease_expire_at").
		Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ?", record.ArtifactID).First(&current).Error; err != nil {
		return err
	}
	if current.BuildOwnerToken != record.BuildOwnerToken || current.BuildGeneration != record.BuildGeneration {
		return errRootfsArtifactBuildLeaseLost
	}
	if current.Status == ArtifactStatusReady ||
		(current.Status == ArtifactStatusBuilding && current.BuildLeaseExpireAt > time.Now().Unix()) {
		return nil
	}
	return errRootfsArtifactBuildLeaseLost
}

func markRootfsArtifactBuildFailed(ctx context.Context, record *models.RootfsArtifact, buildErr error) error {
	if record == nil || record.BuildOwnerToken == "" || record.BuildGeneration < 1 {
		return errRootfsArtifactBuildLeaseLost
	}
	updates := map[string]any{
		"status":                ArtifactStatusFailed,
		"last_error":            buildErr.Error(),
		"build_lease_expire_at": int64(0),
		"updated_at":            time.Now(),
	}
	var finalizeErr *publishedArtifactFinalizeError
	if errors.As(buildErr, &finalizeErr) {
		// Keep enough metadata for lifecycle GC to retry if immediate shared
		// storage cleanup fails after the FAILED fence is committed.
		updates["ext4_path"] = finalizeErr.publishedPath
		updates["ext4_sha256"] = record.Ext4SHA256
		updates["ext4_size_bytes"] = record.Ext4SizeBytes
	}
	tx := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ? AND status = ? AND build_owner_token = ? AND build_generation = ?",
			record.ArtifactID, ArtifactStatusBuilding, record.BuildOwnerToken, record.BuildGeneration).
		Updates(updates)
	if tx.Error != nil {
		return tx.Error
	}
	if tx.RowsAffected != 1 {
		return errRootfsArtifactBuildLeaseLost
	}
	return nil
}

func startRootfsArtifactBuildLease(ctx context.Context, record *models.RootfsArtifact) (context.Context, func() error) {
	buildCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	stopped := make(chan struct{})
	var leaseErr error
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(rootfsArtifactBuildRenewEvery)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-buildCtx.Done():
				return
			case <-ticker.C:
				renewCtx, renewCancel := context.WithTimeout(context.Background(), 10*time.Second)
				err := renewRootfsArtifactBuildLease(renewCtx, record)
				renewCancel()
				if err != nil {
					leaseErr = err
					cancel()
					return
				}
			}
		}
	}()
	return buildCtx, func() error {
		close(done)
		cancel()
		<-stopped
		return leaseErr
	}
}

func findReusableRootfsArtifact(ctx context.Context, fingerprint, artifactID string) (*models.RootfsArtifact, bool, error) {
	record, err := getRootfsArtifactByFingerprint(ctx, fingerprint)
	if err == nil {
		record, err = validateReusableRootfsArtifact(record, fingerprint, artifactID)
		return record, rootfsArtifactSoftDeleted(record), err
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}

	record, err = getRootfsArtifactByFingerprintUnscoped(ctx, fingerprint)
	if err == nil {
		record, err = validateReusableRootfsArtifact(record, fingerprint, artifactID)
		return record, rootfsArtifactSoftDeleted(record), err
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}

	record, err = getRootfsArtifactByID(ctx, artifactID)
	if err != nil {
		record, err = getRootfsArtifactByIDUnscoped(ctx, artifactID)
		if err != nil {
			return nil, false, err
		}
	}
	record, err = validateReusableRootfsArtifact(record, fingerprint, artifactID)
	return record, rootfsArtifactSoftDeleted(record), err
}

func validateReusableRootfsArtifact(record *models.RootfsArtifact, fingerprint, artifactID string) (*models.RootfsArtifact, error) {
	if record == nil {
		return nil, gorm.ErrRecordNotFound
	}
	if record.ArtifactID != artifactID {
		return nil, fmt.Errorf("rootfs artifact id mismatch: want %s got %s", artifactID, record.ArtifactID)
	}
	if record.TemplateSpecFingerprint != "" && record.TemplateSpecFingerprint != fingerprint {
		return nil, fmt.Errorf("rootfs artifact %s fingerprint mismatch: want %s got %s", artifactID, fingerprint, record.TemplateSpecFingerprint)
	}
	return record, nil
}

func rootfsArtifactSoftDeleted(record *models.RootfsArtifact) bool {
	return record != nil && record.DeletedAt.Valid
}

func restoreRootfsArtifact(ctx context.Context, artifactID string) error {
	tx := store.db.WithContext(ctx).Unscoped().Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ?", artifactID).
		Updates(map[string]any{
			"deleted_at": nil,
			"updated_at": time.Now(),
		})
	if tx.Error != nil {
		return tx.Error
	}
	if tx.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func buildRootfsArtifact(ctx context.Context, record *models.RootfsArtifact, req *types.CreateTemplateFromImageReq, source *image.PreparedSource, downloadBaseURL string, caPEM []byte, caFingerprint string) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, error) {
	var caBakeResult cube_egress_ca.Result
	opts := image.BuildOptions{ArtifactID: record.ArtifactID, Generation: record.BuildGeneration}
	// Bake the CubeEgress root CA into the rootfs while it's still a mutable
	// host-side directory, before mkfs.ext4 freezes the layout. See
	// design/cube-egress-ca-bake.md for the contract.
	opts.PostRootfsExport = func(ctx context.Context, rootfsDir string) error {
		var err error
		caBakeResult, err = applyCubeEgressCAToRootfs(ctx, rootfsDir, caPEM, caFingerprint)
		return err
	}
	result, err := image.BuildExt4(ctx, source, opts)
	if err != nil {
		return nil, nil, err
	}
	localBuildDir := filepath.Dir(result.Ext4Path)
	if rel, relErr := filepath.Rel(filepath.Clean(image.ArtifactWorkRootDir()), localBuildDir); relErr == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
		defer func() {
			_ = os.RemoveAll(localBuildDir) // NOCC:Path Traversal()
		}()
	}
	publishedPath, publishedSize, err := image.PublishExt4(ctx, record.ArtifactID, record.BuildGeneration, result.SHA256, result.Ext4Path)
	if err != nil {
		return nil, nil, fmt.Errorf("publish rootfs artifact generation %d: %w", record.BuildGeneration, err)
	}
	if publishedSize != result.SizeBytes {
		sizeErr := fmt.Errorf("published rootfs artifact size mismatch: got %d want %d", publishedSize, result.SizeBytes)
		return nil, nil, cleanupUncommittedPublishedExt4(ctx, record, publishedPath, sizeErr)
	}
	finalRecord, generatedReq, err := finalizeArtifact(ctx, record, source, publishedPath, result.SHA256, result.SizeBytes, downloadBaseURL, req, caBakeResult)
	if err != nil {
		return nil, nil, &publishedArtifactFinalizeError{publishedPath: publishedPath, cause: err}
	}
	return finalRecord, generatedReq, nil
}

func cleanupUncommittedPublishedExt4(ctx context.Context, record *models.RootfsArtifact, publishedPath string, cause error) error {
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), rootfsArtifactBuildStateWriteTimeout)
	defer cleanupCancel()
	if err := image.RemovePublishedExt4(cleanupCtx, record.ArtifactID, record.BuildGeneration, publishedPath); err != nil {
		return errors.Join(cause, fmt.Errorf("cleanup uncommitted rootfs artifact generation %d: %w", record.BuildGeneration, err))
	}
	return cause
}

func cleanupPublishedArtifactAfterFailedFinalize(ctx context.Context, record *models.RootfsArtifact, buildErr, markErr error) error {
	if markErr != nil {
		return buildErr
	}
	var finalizeErr *publishedArtifactFinalizeError
	if !errors.As(buildErr, &finalizeErr) {
		return buildErr
	}
	return cleanupUncommittedPublishedExt4(ctx, record, finalizeErr.publishedPath, buildErr)
}

// finalizeArtifact populates the artifact record with computed values and
// atomically persists READY under the live lease fence.
func finalizeArtifact(ctx context.Context, record *models.RootfsArtifact, source *image.PreparedSource, ext4Path, shaValue string, sizeBytes int64, downloadBaseURL string, req *types.CreateTemplateFromImageReq, caBakeResult cube_egress_ca.Result) (*models.RootfsArtifact, *types.CreateCubeSandboxReq, error) {
	downloadToken := uuid.New().String()
	record.SourceImageDigest = source.Digest
	record.MasterNodeIP = source.MasterNodeIP
	record.Ext4Path = ext4Path
	record.Ext4SHA256 = shaValue
	record.Ext4SizeBytes = sizeBytes
	record.ImageConfigJSON = source.ConfigJSON
	record.DownloadToken = downloadToken
	record.Status = ArtifactStatusReady
	record.GCDeadline = time.Now().Add(defaultTemplateArtifactTTL).Unix()
	record.CubeEgressCABaked = caBakeResult.Baked
	record.CubeEgressCAFingerprint = caBakeResult.Fingerprint
	record.CubeEgressCATargetsWritten = caBakeResult.TargetsWritten

	generatedReq, err := generateTemplateCreateRequest(req, record, source.Config, downloadBaseURL)
	if err != nil {
		return nil, nil, err
	}
	reqPayload, err := json.Marshal(generatedReq)
	if err != nil {
		return nil, nil, err
	}
	record.GeneratedRequestJSON = string(reqPayload)
	if err := finalizeRootfsArtifactRecord(ctx, record, map[string]any{
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
		"build_lease_expire_at":          int64(0),
		"updated_at":                     time.Now(),
	}); err != nil {
		return nil, nil, err
	}
	record.BuildLeaseExpireAt = 0
	return record, generatedReq, nil
}
