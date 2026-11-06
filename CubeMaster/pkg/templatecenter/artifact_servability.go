// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
)

// This file checks the authoritative storage tier before distribution starts.
//
// WHY A NETWORK PROBE, NOT A DISK PROBE
// -------------------------------------
// In the split-process architecture TC builds the ext4. An fs artifact stays
// on TC-managed local/CBS/CFS storage and is served by TC; an S3 artifact is
// served by the object store. CubeMaster's own disk is therefore not evidence
// that a freshly built artifact is missing.
//
// Probes are control-plane requests: TC uses existing component authentication,
// while S3 uses Stat. They must not require a node replica or mint node tickets.
//
// TC is probed with HEAD. S3 is checked through the blobstore Stat API because
// an AWS SigV4 GET URL cannot be reused for HEAD and the control plane must not
// fetch even a byte of the artifact.

// artifactServability is the outcome of probing the download path.
type artifactServability int

const (
	// artifactServabilityUnknown means the probe could not decide: network
	// error, unexpected status, unparseable envelope. Callers must NOT
	// demote the row on Unknown — a transient TC outage must not destroy a
	// healthy artifact.
	artifactServabilityUnknown artifactServability = iota
	// artifactServabilityServable means the download endpoint served bytes.
	artifactServabilityServable
	// artifactServabilityMissing means the serving tier itself answered
	// "not found" — the file is genuinely gone there, so the row should be
	// demoted and rebuilt.
	artifactServabilityMissing
)

// artifactServedByRemoteTier reports whether this process delegates artifact
// file serving to the standalone CubeTemplateCenter tier: CubeMaster always
// has CUBE_TEMPLATE_CENTER_ADDR configured (builds are forwarded there), and
// TC itself never does (routes.go: a proxied build submit would otherwise
// forward the job back to TC itself). Indirected for tests.
var artifactServedByRemoteTier = func() bool {
	cfg := config.GetConfig()
	return cfg != nil && cfg.TemplateCenterAddr() != ""
}

// artifactServabilityEnvelope mirrors the ret envelope of every cube API
// response — just enough to read the business code out of a probe response.
type artifactServabilityEnvelope struct {
	Ret struct {
		Code int    `json:"ret_code"`
		Msg  string `json:"ret_msg"`
	} `json:"ret"`
}

var artifactServabilityHTTPClient = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// getArtifactDownloadRange is the single network seam, indirected so tests
// can substitute a fake serving tier.
var getArtifactDownloadRange = func(ctx context.Context, rawURL string, requestHeader http.Header) (status int, header http.Header, body []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil) // NOCC:Request Without TLS(下载地址由部署侧下发，集群内明文属预期)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header = requestHeader.Clone()
	resp, err := artifactServabilityHTTPClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	// HEAD carries no artifact bytes. Keep a bounded read only for defensive
	// compatibility with a non-compliant upstream.
	body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, body, nil
}

// classifyArtifactServability is the pure decision half of the probe, kept
// I/O-free so every branch is directly testable.
func classifyArtifactServability(status int, header http.Header, body []byte) (artifactServability, string) {
	// A served artifact normally returns 200 to HEAD with identity/length
	// headers. Keep 206 accepted for compatibility with ranged probe stubs and
	// older serving tiers.
	if status == http.StatusPartialContent {
		return artifactServabilityServable, "206 partial content"
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		// Defensive: the cube API contract is 200+envelope, but a plain HTTP
		// server (or a future handler change) may use real statuses.
		return artifactServabilityMissing, fmt.Sprintf("http %d", status)
	}
	if status == http.StatusOK {
		if header.Get("X-Cube-Artifact-Id") != "" || header.Get("ETag") != "" || header.Get("Content-Length") != "" {
			return artifactServabilityServable, "200 with artifact headers"
		}
		// The error envelope: the file path failed and the handler wrote
		// ret_code in the body.
		env := artifactServabilityEnvelope{}
		if err := json.Unmarshal(body, &env); err != nil {
			return artifactServabilityUnknown, fmt.Sprintf("http 200 with unparseable body (%.120s)", body)
		}
		if env.Ret.Code == int(errorcode.ErrorCode_NotFound) {
			return artifactServabilityMissing, fmt.Sprintf("ret_code=%d ret_msg=%s", env.Ret.Code, env.Ret.Msg)
		}
		return artifactServabilityUnknown, fmt.Sprintf("ret_code=%d ret_msg=%s", env.Ret.Code, env.Ret.Msg)
	}
	return artifactServabilityUnknown, fmt.Sprintf("http status %d", status)
}

const (
	// servabilityProbeTTL bounds how long a positive servability answer is
	// reused. The preflight runs once per artifact per distribution/redo, and a
	// batch distribution repeats it for every target, so without this the
	// download endpoint sees one ranged GET per artifact per operation.
	servabilityProbeTTL = 30 * time.Second
	// servabilityProbeCacheMax bounds the map: artifact ids are bounded by
	// live templates, but a long-lived master must not grow it without limit.
	servabilityProbeCacheMax = 4096
)

var (
	servabilityProbeMu    sync.Mutex
	servabilityProbeCache = map[string]artifactServabilityProbe{}
)

type artifactServabilityProbe struct {
	verdict artifactServability
	reason  string
	at      time.Time
}

func artifactServabilityCacheKey(artifact *models.RootfsArtifact) string {
	locator := []string{artifact.ArtifactID, fmt.Sprint(artifact.Generation), artifact.StorageBackend,
		artifact.StorageRef, artifact.ObjectKey, artifact.OwnerTCDownloadBase, artifact.Ext4Path,
		artifact.ArtifactURL, artifact.Ext4SHA256, artifact.MasterNodeIP}
	encoded, _ := json.Marshal(locator)
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

var artifactServabilityS3StatFn = statArtifactObjectInS3

// probeServabilityOnce reuses a recent positive answer, then probes.
//
// Only artifactServabilityServable is cached. Caching Missing would keep
// handing out an artifact the serving tier has already lost, and caching
// Unknown would extend a transient outage by the TTL -- both are exactly the
// failure modes the probe exists to catch.
func probeServabilityOnce(ctx context.Context, artifact *models.RootfsArtifact) (artifactServability, string) {
	if artifact == nil {
		return artifactServabilityUnknown, "nil artifact"
	}
	now := time.Now()
	cacheKey := artifactServabilityCacheKey(artifact)
	servabilityProbeMu.Lock()
	cached, ok := servabilityProbeCache[cacheKey]
	servabilityProbeMu.Unlock()
	if ok && now.Sub(cached.at) < servabilityProbeTTL {
		return cached.verdict, cached.reason
	}

	verdict, reason := probeArtifactServability(ctx, artifact)
	if verdict == artifactServabilityServable {
		servabilityProbeMu.Lock()
		if len(servabilityProbeCache) >= servabilityProbeCacheMax {
			// Cheap full reset rather than an LRU: entries expire on their own
			// within servabilityProbeTTL, so dropping all of them only costs
			// one extra probe per live artifact.
			servabilityProbeCache = map[string]artifactServabilityProbe{}
		}
		servabilityProbeCache[cacheKey] = artifactServabilityProbe{verdict: verdict, reason: reason, at: now}
		servabilityProbeMu.Unlock()
	}
	return verdict, reason
}

// probeArtifactServability issues the same component-authenticated request
// Master itself would use to check artifact state on TC, and classifies the
// answer. It is a liveness/placement probe, not a replay of Cubelet's own
// download request: Cubelet downloads fs/TC artifacts directly from TC's
// node-facing, per-node-token endpoint (see TemplateCenterDirectArtifactDownloadURL),
// never through this component-token endpoint.
func probeArtifactServability(ctx context.Context, artifact *models.RootfsArtifact) (artifactServability, string) {
	if artifact == nil {
		return artifactServabilityUnknown, "nil artifact"
	}
	if artifactUsesS3(artifact) {
		exists, statErr := artifactServabilityS3StatFn(ctx, artifact)
		if statErr != nil {
			return artifactServabilityUnknown, fmt.Sprintf("stat object store artifact: %v", statErr)
		}
		if !exists {
			return artifactServabilityMissing, "object store reports not found"
		}
		return artifactServabilityServable, "object store stat succeeded"
	}
	endpoint, err := TemplateCenterArtifactDownloadURL(artifact)
	if err != nil {
		return artifactServabilityUnknown, "invalid TC artifact endpoint"
	}
	callbackToken := strings.TrimSpace(os.Getenv(constants.TemplateCallbackTokenEnv))
	if callbackToken == "" {
		return artifactServabilityUnknown, "TC component authentication is not configured"
	}
	requestHeader := make(http.Header)
	requestHeader.Set(constants.TemplateCallbackTokenHeader, callbackToken)
	status, header, body, err := getArtifactDownloadRange(ctx, endpoint, requestHeader)
	if err != nil {
		return artifactServabilityUnknown, fmt.Sprintf("head direct artifact endpoint: %v", sanitizeArtifactProbeError(err))
	}
	return classifyArtifactServability(status, header, body)
}

func sanitizeArtifactProbeError(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return errors.New("artifact endpoint request failed")
}

// demoteUnservableRootfsArtifact flips a READY row whose data the serving
// tier reports missing to FAILED so the next create rebuilds it. This is the
// remote-tier counterpart of demoteMissingRootfsArtifact: the "not found"
// answer came directly from the tier that owns the bytes, so it is
// authoritative regardless of which pod ran the probe.
var demoteUnservableRootfsArtifact = func(ctx context.Context, artifact *models.RootfsArtifact, reason string) (bool, error) {
	if artifact == nil {
		return false, errors.New("nil artifact probe snapshot")
	}
	log.G(ctx).Errorf("rootfs artifact %s: download path reports missing (%s); demoting to %s so the next create rebuilds it",
		artifact.ArtifactID, reason, ArtifactStatusFailed)
	query := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ? AND status = ? AND COALESCE(generation, 0) = ?", artifact.ArtifactID, ArtifactStatusReady, artifact.Generation)
	for column, value := range map[string]string{
		"storage_backend": artifact.StorageBackend, "storage_ref": artifact.StorageRef,
		"object_key": artifact.ObjectKey, "owner_tc_download_base": artifact.OwnerTCDownloadBase,
		"ext4_path": artifact.Ext4Path, "artifact_url": artifact.ArtifactURL,
		"ext4_sha256": artifact.Ext4SHA256, "master_node_ip": artifact.MasterNodeIP,
	} {
		query = query.Where("COALESCE("+column+", '') = ?", value)
	}
	result := query.Updates(map[string]any{
		"status":     ArtifactStatusFailed,
		"last_error": fmt.Sprintf("download endpoint reports the artifact missing (%s); artifact must be rebuilt", reason),
		"updated_at": time.Now(),
	})
	if result.Error != nil {
		log.G(ctx).Warnf("rootfs artifact %s generation %d: demote to %s fail: %v",
			artifact.ArtifactID, artifact.Generation, ArtifactStatusFailed, result.Error)
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		log.G(ctx).Infof("rootfs artifact %s generation %d: demote skipped because status, generation or locator changed while the probe was in flight",
			artifact.ArtifactID, artifact.Generation)
		return false, nil
	}
	return true, nil
}

var ErrArtifactLocatorChanged = errors.New("artifact locator changed while checking servability; retry with the current locator")

// artifactUnservableError renders the demote outcome for callers.
func artifactUnservableError(artifact *models.RootfsArtifact, reason string) error {
	return fmt.Errorf(
		"rootfs artifact %s is READY in the database but the download endpoint reports it missing (%s); "+
			"the file is gone on the serving tier (CubeTemplateCenter restarted without a persistent volume?) — "+
			"that generation was demoted if it was still current, so the next create can rebuild it",
		artifact.ArtifactID, reason)
}

// artifactServabilityUnknownError renders the "could not decide" outcome. The
// row is deliberately left untouched: the artifact may be perfectly healthy
// behind a transient network/TC failure.
func artifactServabilityUnknownError(artifact *models.RootfsArtifact, reason string) error {
	return fmt.Errorf(
		"cannot verify rootfs artifact %s is servable at %q: %s; "+
			"leaving the row untouched (the serving tier may be unreachable) — retry or redo",
		artifact.ArtifactID, effectiveArtifactDownloadBaseURL("", artifact), reason)
}

// verifyArtifactServability is the distribution/redo preflight: the artifact
// row must be backed by data the node-facing download path can actually serve.
//
//   - Remote-TC topology with a resolvable download base URL: mint the same
//     short-lived capability as distribution and probe its TC/S3 destination.
//   - Otherwise (no remote tier, or a legacy row predating the download
//     base URL column) this process IS assumed to be the serving tier and
//     the classic node-local probe runs.
//
// WHY OWNERSHIP MUST NOT GATE THE REMOTE PROBE
// ---------------------------------------------
// artifactOwnershipOf classifies a row as Local whenever its recorded
// MasterNodeIP matches THIS host's own identity. That is true far more often
// than it looks: MasterNodeIP is populated from the inbound request's Host
// header (requestBaseURL), so in a single-node / one-click deployment where
// clients simply call this same CubeMaster's own address, ownership resolves
// to Local -- even though CUBE_TEMPLATE_CENTER_ADDR is configured and the
// build was forwarded to the standalone CubeTemplateCenter tier. Once
// artifactServedByRemoteTier() is true, TC is the ONLY thing that ever
// builds a fresh ext4 (see the file header comment), so "Local" here just
// means "this host answered the HTTP request", not "this host holds the
// file". Gating on ownership made the node-local disk probe run in exactly
// that case, which used to accidentally work only because CubeMaster and TC
// shared one physical artifact-store directory; once they use independent
// directories the stat always misses and every distribution/redo wrongly
// demotes a perfectly healthy artifact to FAILED. This mirrors the fix in
// rootfsArtifactReuseVerdict (artifact_presence.go), which checks
// artifactServedByRemoteTier() alone for the same reason.
//
// The one case that must still fall through to the node-local probe is a
// legacy row from before the download-base-url column existed
// (MasterNodeIP == ""): those may genuinely have their ext4 sitting on this
// node's disk from before this CubeMaster started delegating builds to TC.
func verifyArtifactServability(ctx context.Context, artifact *models.RootfsArtifact) error {
	if artifact == nil {
		return fmt.Errorf("verifyArtifactServability: artifact is nil")
	}
	if artifactServedByRemoteTier() && strings.TrimSpace(effectiveArtifactDownloadBaseURL("", artifact)) != "" {
		switch verdict, reason := probeServabilityOnce(ctx, artifact); verdict {
		case artifactServabilityServable:
			return nil
		case artifactServabilityMissing:
			demoted, err := demoteUnservableRootfsArtifact(ctx, artifact, reason)
			if err != nil {
				return fmt.Errorf("demote missing rootfs artifact: %w", err)
			}
			if !demoted {
				return ErrArtifactLocatorChanged
			}
			return artifactUnservableError(artifact, reason)
		default:
			return artifactServabilityUnknownError(artifact, reason)
		}
	}
	if verdict := resolveMissingArtifact(ctx, artifact); verdict != artifactMissingVerdictNone {
		return missingArtifactError(artifact, verdict)
	}
	return nil
}
