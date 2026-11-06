// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore/configenv"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/artifactticket"
	"gorm.io/gorm"
)

const (
	defaultArtifactDownloadTTL = time.Hour
	minArtifactDownloadTTL     = time.Minute
	maxArtifactDownloadTTL     = 24 * time.Hour
	artifactRefreshTokenTTL    = 24 * time.Hour
)

var (
	ErrArtifactDownloadUnauthorized = errors.New("artifact download is not authorized for this node")
	ErrArtifactDownloadUnconfigured = errors.New("artifact direct download is not configured")
)

func artifactDownloadSigningKey(artifact *models.RootfsArtifact) ([]byte, error) {
	if artifact == nil {
		return nil, ErrArtifactDownloadUnconfigured
	}
	// Legacy download tokens were sent to nodes and cannot be signing secrets.
	// Only Master derives this capability key; TC uses the existing token solely
	// for service authentication and never receives or verifies node tickets.
	key, err := artifactticket.DeriveSigningKey([]byte(strings.TrimSpace(os.Getenv(constants.TemplateCallbackTokenEnv))))
	if err != nil {
		return nil, fmt.Errorf("%w: a strong existing control-plane token is required", ErrArtifactDownloadUnconfigured)
	}
	return key, nil
}

// IssueArtifactDownloadTicket verifies current node placement and returns a
// fresh capability. It never returns a stored presigned URL.
func IssueArtifactDownloadTicket(ctx context.Context, artifactID, nodeID, refreshToken string) (*artifactticket.Ticket, error) {
	artifactID = strings.TrimSpace(artifactID)
	nodeID = strings.TrimSpace(nodeID)
	if artifactID == "" || nodeID == "" {
		return nil, ErrArtifactDownloadUnauthorized
	}
	if store == nil || store.db == nil {
		return nil, ErrTemplateStoreNotInitialized
	}
	now := time.Now()
	artifact, err := getRootfsArtifactByID(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	secret, err := artifactDownloadSigningKey(artifact)
	if err != nil {
		return nil, err
	}
	claims, err := artifactticket.Verify(secret, refreshToken, now)
	if err != nil || claims.Purpose != artifactticket.PurposeRefresh || claims.ArtifactID != artifactID || claims.NodeID != nodeID {
		return nil, ErrArtifactDownloadUnauthorized
	}
	if err := authorizeArtifactReplica(ctx, artifactID, nodeID); err != nil {
		return nil, err
	}
	if artifact.Status != ArtifactStatusReady {
		return nil, fmt.Errorf("artifact %s is not ready", artifactID)
	}
	if err := ensureArtifactStableLocator(ctx, artifact); err != nil {
		return nil, err
	}
	if claims.Generation != artifact.Generation {
		return nil, ErrArtifactDownloadUnauthorized
	}
	return buildArtifactDownloadTicket(ctx, artifact, nodeID, now)
}

func buildArtifactDownloadTicket(ctx context.Context, artifact *models.RootfsArtifact, nodeID string, now time.Time) (*artifactticket.Ticket, error) {
	if artifact == nil || strings.TrimSpace(artifact.ArtifactID) == "" {
		return nil, errors.New("artifact is nil or has no id")
	}
	ttl, err := artifactDownloadTTL()
	if err != nil {
		return nil, err
	}
	generation := artifact.Generation
	if generation <= 0 {
		generation = 1
	}
	expiresAt := now.Add(ttl)
	ticket := &artifactticket.Ticket{
		Method:     http.MethodGet,
		ExpiresAt:  expiresAt.UTC(),
		ServerTime: now.UTC(),
		ArtifactID: artifact.ArtifactID,
		Generation: generation,
		SizeBytes:  artifact.Ext4SizeBytes,
		SHA256:     artifact.Ext4SHA256,
	}

	if artifactUsesS3(artifact) {
		signedURL, err := presignArtifactGetURLWithTTL(ctx, artifact, ttl)
		if err != nil {
			return nil, fmt.Errorf("presign artifact %s: %w", artifact.ArtifactID, err)
		}
		if strings.TrimSpace(signedURL) == "" {
			return nil, fmt.Errorf("presign artifact %s returned an empty URL", artifact.ArtifactID)
		}
		ticket.Source = artifactticket.SourceS3
		ticket.URL = signedURL
		return ticket, nil
	}

	downloadURL, err := TemplateCenterDirectArtifactDownloadURL(artifact)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrArtifactDownloadUnconfigured, err)
	}
	secret, err := artifactDownloadSigningKey(artifact)
	if err != nil {
		return nil, err
	}
	token, err := artifactticket.Sign(secret, artifactticket.Claims{
		ArtifactID: artifact.ArtifactID,
		NodeID:     strings.TrimSpace(nodeID),
		ExpiresAt:  expiresAt.Unix(),
		Generation: generation,
		Purpose:    artifactticket.PurposeDownload,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrArtifactDownloadUnconfigured, err)
	}
	ticket.Source = artifactticket.SourceTemplateCenter
	ticket.URL = downloadURL
	ticket.Token = token
	ticket.Headers = map[string]string{
		artifactticket.DownloadTokenHeader:  token,
		artifactticket.DownloadNodeIDHeader: strings.TrimSpace(nodeID),
	}
	return ticket, nil
}

// BuildArtifactDownloadRefreshToken mints the long-lived (24h) capability
// that Cubelet stores on the pod and exchanges for a short-lived download
// ticket via IssueArtifactDownloadTicket. It is exported so
// CubeTemplateCenter's test suite can drive the real end-to-end
// issuance+verification path without duplicating ticket-signing logic.
func BuildArtifactDownloadRefreshToken(ctx context.Context, artifact *models.RootfsArtifact, nodeID string, now time.Time) (string, error) {
	if artifact == nil || strings.TrimSpace(artifact.ArtifactID) == "" ||
		strings.TrimSpace(nodeID) == "" {
		return "", ErrArtifactDownloadUnauthorized
	}
	generation := artifact.Generation
	if generation <= 0 {
		generation = 1
	}
	secret, err := artifactDownloadSigningKey(artifact)
	if err != nil {
		return "", err
	}
	return artifactticket.Sign(secret, artifactticket.Claims{
		ArtifactID: artifact.ArtifactID,
		NodeID:     strings.TrimSpace(nodeID),
		ExpiresAt:  now.Add(artifactRefreshTokenTTL).Unix(),
		Generation: generation,
		Purpose:    artifactticket.PurposeRefresh,
	})
}

func artifactDirectDownloadOrigin(ctx context.Context, artifact *models.RootfsArtifact) (string, error) {
	if artifact == nil {
		return "", errors.New("artifact is nil")
	}
	var rawURL string
	if artifactUsesS3(artifact) {
		ttl, err := artifactDownloadTTL()
		if err != nil {
			return "", err
		}
		rawURL, err = presignArtifactGetURLWithTTL(ctx, artifact, ttl)
		if err != nil {
			return "", err
		}
	} else {
		var err error
		rawURL, err = TemplateCenterDirectArtifactDownloadURL(artifact)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrArtifactDownloadUnconfigured, err)
		}
	}
	origin, err := artifactticket.URLOrigin(rawURL)
	if err != nil {
		return "", fmt.Errorf("resolve artifact direct-download origin: %w", err)
	}
	return origin, nil
}

func configuredTemplateCenterDownloadBase() string {
	if baseURL := strings.TrimSpace(os.Getenv(constants.TemplateCenterDownloadAddrEnv)); baseURL != "" {
		return baseURL
	}
	if cfg := config.GetConfig(); cfg != nil {
		return strings.TrimSpace(cfg.TemplateCenterAddr())
	}
	return ""
}

// ensureArtifactStableLocator lazily upgrades pre-migration rows before they
// are distributed. Short-lived credentials never enter these columns.
func ensureArtifactStableLocator(ctx context.Context, artifact *models.RootfsArtifact) error {
	if artifact == nil || strings.TrimSpace(artifact.ArtifactID) == "" {
		return errors.New("artifact is nil or has no id")
	}
	if store == nil || store.db == nil {
		return ErrTemplateStoreNotInitialized
	}
	if artifact.Status != ArtifactStatusReady {
		return fmt.Errorf("artifact %s is not ready", artifact.ArtifactID)
	}
	if strings.TrimSpace(artifact.StorageRef) == "" && artifactUsesS3(artifact) {
		return fmt.Errorf("artifact %s requires explicit migration of its S3 storage locator", artifact.ArtifactID)
	}

	candidate := *artifact
	updates := map[string]any{}
	if candidate.Generation <= 0 {
		candidate.Generation = 1
		updates["generation"] = candidate.Generation
	}
	if strings.TrimSpace(candidate.OwnerTCDownloadBase) == "" {
		if baseURL := configuredTemplateCenterDownloadBase(); baseURL != "" {
			candidate.OwnerTCDownloadBase = baseURL
			updates["owner_tc_download_base"] = baseURL
		}
	}
	if strings.TrimSpace(candidate.StorageRef) == "" {
		if storageRef := inferredArtifactStorageRef(&candidate); storageRef != "" {
			candidate.StorageRef = storageRef
			updates["storage_ref"] = storageRef
		}
	}

	query := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ? AND deleted_at IS NULL AND status = ?", artifact.ArtifactID, artifact.Status)
	if artifact.Generation == 0 {
		query = query.Where("(generation = ? OR generation IS NULL)", artifact.Generation)
	} else {
		query = query.Where("generation = ?", artifact.Generation)
	}
	for _, field := range []struct{ column, value string }{
		{"storage_backend", artifact.StorageBackend},
		{"object_key", artifact.ObjectKey},
		{"storage_ref", artifact.StorageRef},
		{"owner_tc_download_base", artifact.OwnerTCDownloadBase},
		{"artifact_url", artifact.ArtifactURL},
		{"ext4_path", artifact.Ext4Path},
	} {
		if field.value == "" {
			query = query.Where("("+field.column+" = ? OR "+field.column+" IS NULL)", field.value)
		} else {
			query = query.Where(field.column+" = ?", field.value)
		}
	}
	if len(updates) == 0 {
		var count int64
		if err := query.Count(&count).Error; err != nil {
			return fmt.Errorf("check stable locator for artifact %s: %w", artifact.ArtifactID, err)
		}
		if count != 1 {
			return fmt.Errorf("stable locator conflict for artifact %s: READY artifact snapshot changed", artifact.ArtifactID)
		}
		return nil
	}
	result := query.Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("backfill stable locator for artifact %s: %w", artifact.ArtifactID, result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("stable locator conflict for artifact %s: READY artifact snapshot changed", artifact.ArtifactID)
	}
	artifact.Generation = candidate.Generation
	artifact.OwnerTCDownloadBase = candidate.OwnerTCDownloadBase
	artifact.StorageRef = candidate.StorageRef
	return nil
}

func inferredArtifactStorageRef(artifact *models.RootfsArtifact) string {
	if artifact == nil || artifactUsesS3(artifact) {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(artifact.StorageBackend)) {
	case "fs":
		return (&url.URL{Scheme: "file", Path: filepath.Clean(configenv.ResolveArtifactFSRoot())}).String()
	default:
		path := strings.TrimSpace(artifact.Ext4Path)
		if path == "" || !filepath.IsAbs(path) {
			return ""
		}
		return (&url.URL{Scheme: "file", Path: filepath.Clean(path)}).String()
	}
}

var artifactDownloadTTL = func() (time.Duration, error) {
	if cfg := config.GetConfig(); cfg != nil {
		return cfg.Common.ArtifactDownloadDuration()
	}
	return (*config.CommonConf)(nil).ArtifactDownloadDuration()
}

func artifactUsesS3(artifact *models.RootfsArtifact) bool {
	if artifact == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(artifact.StorageBackend)) {
	case "s3":
		return true
	case "fs":
		return false
	}
	// Legacy rows did not record storage_backend. A non-empty artifact_url was
	// only produced after a successful object-store upload.
	return strings.TrimSpace(artifact.ArtifactURL) != ""
}

func templateCenterArtifactURL(baseURL, artifactID string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", errors.New("TemplateCenter download base URL is empty")
	}
	if !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", fmt.Errorf("invalid TemplateCenter download base URL %q", baseURL)
	}
	u.RawQuery = ""
	u.Fragment = ""
	return url.JoinPath(u.String(), "internal", "artifacts", artifactID, "ext4")
}

func artifactDownloadTicketURL(baseURL, artifactID string) (string, error) {
	baseURL = strings.TrimSpace(NormalizeBaseURL(baseURL))
	if baseURL == "" {
		return "", errors.New("CubeMaster download base URL is empty")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("invalid CubeMaster download base URL %q", baseURL)
	}
	u.RawQuery = ""
	u.Fragment = ""
	return url.JoinPath(u.String(), "internal", "template", "artifacts", artifactID, "download-ticket")
}

// VerifyArtifactDownloadToken verifies a node's per-artifact download
// capability against the current replica placement. It is called by
// CubeTemplateCenter's node-facing handler (nodeArtifactDownloadHandler):
// TC shares this package's DB-backed store and node cache (see
// CubeTemplateCenter's InitForTemplateCenter and nodemeta.Init), so it never
// needs an extra HTTP hop back to Master just to check a token. Master itself
// never serves artifact bytes.
func VerifyArtifactDownloadToken(ctx context.Context, artifactID, nodeID, token string) (*models.RootfsArtifact, error) {
	if store == nil || store.db == nil {
		return nil, ErrTemplateStoreNotInitialized
	}
	artifact, err := getRootfsArtifactByID(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	secret, err := artifactDownloadSigningKey(artifact)
	if err != nil {
		return nil, err
	}
	claims, err := artifactticket.Verify(secret, token, time.Now())
	generation := artifact.Generation
	if generation <= 0 {
		generation = 1
	}
	if err != nil || claims.Purpose != artifactticket.PurposeDownload || claims.ArtifactID != artifactID || claims.NodeID != nodeID ||
		claims.Generation != generation || artifact.Status != ArtifactStatusReady {
		return nil, ErrArtifactDownloadUnauthorized
	}
	if err := authorizeArtifactReplica(ctx, artifactID, nodeID); err != nil {
		return nil, err
	}
	return artifact, nil
}

var lookupArtifactDownloadNode = localcache.GetNode

// SetArtifactDownloadStoreForTest wires the package-level store and node
// lookup used by ticket issuance/verification (IssueArtifactDownloadTicket,
// VerifyArtifactDownloadToken) to test doubles and returns a restore
// function. It is exported — not a _test.go helper — specifically so
// CubeTemplateCenter's own test binary (a different Go package that only
// calls these exported functions, never this package's private `store`)
// can exercise the real cross-component download-authorization path
// against a throwaway DB instead of stubbing VerifyArtifactDownloadToken.
// Production code never calls this.
func SetArtifactDownloadStoreForTest(db *gorm.DB, lookupNode func(string) (*node.Node, bool)) func() {
	oldDB := store.db
	oldLookup := lookupArtifactDownloadNode
	store.db = db
	if lookupNode != nil {
		lookupArtifactDownloadNode = lookupNode
	}
	return func() {
		store.db = oldDB
		lookupArtifactDownloadNode = oldLookup
	}
}

func authorizeArtifactReplica(ctx context.Context, artifactID, nodeID string) error {
	if store == nil || store.db == nil {
		return ErrTemplateStoreNotInitialized
	}
	if _, ok := lookupArtifactDownloadNode(nodeID); !ok {
		return ErrArtifactDownloadUnauthorized
	}
	var count int64
	if err := store.db.WithContext(ctx).Model(&models.TemplateReplica{}).
		Where("artifact_id = ? AND node_id = ? AND phase IN ?", artifactID, nodeID,
			[]string{ReplicaPhaseDistributing, ReplicaPhaseDistributed, ReplicaPhaseSnapshotting}).Count(&count).Error; err != nil {
		return fmt.Errorf("check artifact replica authorization: %w", err)
	}
	if count == 0 {
		return ErrArtifactDownloadUnauthorized
	}
	return nil
}

// TemplateCenterArtifactDownloadURL is TC's component-authenticated
// /tc/api/v1/artifacts/:id/ext4 endpoint. It is used only by Master's own
// servability probe (Master -> TC, shared component token); it is never
// handed to a node and Master never relays its bytes to Cubelet.
func TemplateCenterArtifactDownloadURL(artifact *models.RootfsArtifact) (string, error) {
	base := strings.TrimSpace(artifact.OwnerTCDownloadBase)
	if base == "" {
		base = configuredTemplateCenterDownloadBase()
	}
	downloadURL, err := templateCenterArtifactURL(base, artifact.ArtifactID)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(downloadURL)
	u.Path = strings.TrimSuffix(u.Path, "/internal/artifacts/"+artifact.ArtifactID+"/ext4") + "/tc/api/v1/artifacts/" + artifact.ArtifactID + "/ext4"
	return u.String(), nil
}

// TemplateCenterDirectArtifactDownloadURL is the download URL for fs-backed
// artifacts: it points straight at TC's node-facing
// /internal/artifacts/:id/ext4 route, which Cubelet calls with the
// per-node, per-generation download token issued by IssueArtifactDownloadTicket
// (verified locally by TC via VerifyArtifactDownloadToken, no Master hop for
// the file bytes).
func TemplateCenterDirectArtifactDownloadURL(artifact *models.RootfsArtifact) (string, error) {
	if artifact == nil {
		return "", errors.New("artifact is nil")
	}
	base := strings.TrimSpace(artifact.OwnerTCDownloadBase)
	if base == "" {
		base = configuredTemplateCenterDownloadBase()
	}
	return templateCenterArtifactURL(base, artifact.ArtifactID)
}

// artifactDownloadAnnotations builds the ticket-protocol annotations that
// tell Cubelet where to fetch its short-lived download ticket (Master) and
// which origin the resulting file bytes must come from (S3 or the owning
// TemplateCenter). There is no Master-proxied fallback: every artifact is
// downloaded directly from its storage.
func artifactDownloadAnnotations(ctx context.Context, artifact *models.RootfsArtifact, baseURL string) (map[string]string, error) {
	annotations := map[string]string{
		constants.CubeAnnotationRootfsArtifactID:        artifact.ArtifactID,
		constants.CubeAnnotationRootfsArtifactSHA256:    artifact.Ext4SHA256,
		constants.CubeAnnotationRootfsArtifactSizeBytes: fmt.Sprint(artifact.Ext4SizeBytes),
	}
	baseURL = effectiveArtifactDownloadBaseURL(baseURL, artifact)
	ticketURL, err := artifactDownloadTicketURL(baseURL, artifact.ArtifactID)
	if err != nil {
		return nil, err
	}
	// The node fetches the ticket from Master but, for both S3 and fs/TC
	// artifacts, downloads the bytes from a different origin (the object
	// store or the owner TC) — Cubelet pins to the latter, not to Master's
	// ticket-issuing origin. See artifactDirectDownloadOrigin.
	origin, err := artifactDirectDownloadOrigin(ctx, artifact)
	if err != nil {
		return nil, err
	}
	annotations[constants.CubeAnnotationRootfsArtifactTicketURL] = ticketURL
	annotations[constants.CubeAnnotationRootfsArtifactDownloadOrigin] = origin
	return annotations, nil
}
