// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
)

// This file decides "will the download URL distribution is about to hand to
// cubelets actually serve the artifact?" for the standalone-CubeTemplateCenter
// topology.
//
// WHY A NETWORK PROBE, NOT A DISK PROBE
// -------------------------------------
// In the split-process architecture the ext4 file ALWAYS lives in the
// CubeTemplateCenter tier: TC builds it, and the download route
// (/cube/template/artifact/download) is reverse-proxied from CubeMaster to TC
// (see httpservice/cube/routes.go). CubeMaster's own disk never holds a
// freshly built artifact, so the node-local probe in artifact_presence.go
// (stat the row's ext4_path on THIS process's disk) can only ever report
// "missing" or "foreign" on CubeMaster. In a Kubernetes deployment the
// recorded download base URL is the master Service DNS name (it is derived
// from the request Host header), which matches no local host identity — so
// every distribution died with artifactNotServedHereError even though the
// file was perfectly healthy on the TC pod.
//
// The check that DOES mean something on CubeMaster is end-to-end: issue the
// same request a cubelet would issue (a ranged GET against the recorded
// download URL) and classify the answer. This validates the whole chain —
// master service -> proxy -> TC -> file — instead of one process's disk.
//
// The probe is a GET with "Range: bytes=0-0" rather than a HEAD because the
// API envelope contract (common.WriteAPI) reports errors as HTTP 200 with a
// JSON body, and a HEAD response strips that body; a ranged GET keeps both
// the success signal (206, or 200 with the X-Cube-Artifact-Id header the
// download handler sets) and the failure envelope parseable.

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
	Timeout: 15 * time.Second,
}

// getArtifactDownloadRange is the single network seam, indirected so tests
// can substitute a fake serving tier.
var getArtifactDownloadRange = func(ctx context.Context, rawURL string) (status int, header http.Header, body []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil) // NOCC:Request Without TLS(下载地址由部署侧下发，集群内明文属预期)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := artifactServabilityHTTPClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	// Bound the read: the success path honours Range (one byte) and the error
	// path is a small JSON envelope, but a misbehaving server must not make
	// this probe stream a whole rootfs into memory.
	body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, body, nil
}

// classifyArtifactServability is the pure decision half of the probe, kept
// I/O-free so every branch is directly testable.
func classifyArtifactServability(status int, header http.Header, body []byte) (artifactServability, string) {
	// The download handler honours Range: a served artifact comes back 206
	// (or 200 if some middlebox stripped the Range) with the marker headers
	// openTemplateArtifactForDownload sets.
	if status == http.StatusPartialContent {
		return artifactServabilityServable, "206 partial content"
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		// Defensive: the cube API contract is 200+envelope, but a plain HTTP
		// server (or a future handler change) may use real statuses.
		return artifactServabilityMissing, fmt.Sprintf("http %d", status)
	}
	if status == http.StatusOK {
		if header.Get("X-Cube-Artifact-Id") != "" {
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
	servabilityProbeMu.Lock()
	cached, ok := servabilityProbeCache[artifact.ArtifactID]
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
		servabilityProbeCache[artifact.ArtifactID] = artifactServabilityProbe{verdict: verdict, reason: reason, at: now}
		servabilityProbeMu.Unlock()
	}
	return verdict, reason
}

// probeArtifactServability issues the same request a cubelet would issue for
// this artifact and classifies the answer.
func probeArtifactServability(ctx context.Context, artifact *models.RootfsArtifact) (artifactServability, string) {
	if artifact == nil {
		return artifactServabilityUnknown, "nil artifact"
	}
	rawURL := buildDownloadURL(effectiveArtifactDownloadBaseURL("", artifact), artifact.ArtifactID, artifact.DownloadToken)
	status, header, body, err := getArtifactDownloadRange(ctx, rawURL)
	if err != nil {
		return artifactServabilityUnknown, fmt.Sprintf("get %q: %v", rawURL, err)
	}
	return classifyArtifactServability(status, header, body)
}

// demoteUnservableRootfsArtifact flips a READY row whose data the serving
// tier reports missing to FAILED so the next create rebuilds it. This is the
// remote-tier counterpart of demoteMissingRootfsArtifact: the "not found"
// answer came from the tier that actually serves the file (reached through
// the proxied download route), so it is authoritative regardless of which
// pod this probe ran on.
var demoteUnservableRootfsArtifact = func(ctx context.Context, artifactID, reason string) {
	log.G(ctx).Errorf("rootfs artifact %s: download path reports missing (%s); demoting to %s so the next create rebuilds it",
		artifactID, reason, ArtifactStatusFailed)
	// Read the current status and demote with a CAS rather than a blind update:
	// the row may have been claimed for deletion (or rebuilt) between the probe
	// and this write, and overwriting it would either resurrect a deleted
	// artifact or discard a fresh rebuild. Losing the demotion only costs
	// another failed attempt that retries this same probe, so it must not fail
	// the caller.
	artifact, err := getRootfsArtifactByID(ctx, artifactID)
	if err != nil {
		log.G(ctx).Warnf("rootfs artifact %s: demote to %s skipped, cannot re-read row: %v", artifactID, ArtifactStatusFailed, err)
		return
	}
	ok, err := updateRootfsArtifactIfStatus(ctx, artifactID, artifact.Status, map[string]any{
		"status":     ArtifactStatusFailed,
		"last_error": fmt.Sprintf("download endpoint reports the artifact missing (%s); artifact must be rebuilt", reason),
	})
	if err != nil {
		log.G(ctx).Warnf("rootfs artifact %s: demote to %s fail: %v", artifactID, ArtifactStatusFailed, err)
		return
	}
	if !ok {
		log.G(ctx).Infof("rootfs artifact %s: demote skipped, status changed to %s while the probe was in flight",
			artifactID, artifact.Status)
	}
}

// artifactUnservableError renders the demote outcome for callers.
func artifactUnservableError(artifact *models.RootfsArtifact, reason string) error {
	return fmt.Errorf(
		"rootfs artifact %s is READY in the database but the download endpoint reports it missing (%s); "+
			"the file is gone on the serving tier (CubeTemplateCenter restarted without a persistent volume?) — "+
			"the row has been demoted so the next create rebuilds it",
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
//   - Remote-TC topology with a resolvable download base URL: probe the
//     CubeMaster download endpoint end-to-end, regardless of whether the
//     artifact data ultimately lives on TC-local disk or in S3/MinIO. The
//     endpoint is what Cubelets dial after the K8s multi-node fix, so this is
//     the only probe that matches reality.
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
			demoteUnservableRootfsArtifact(ctx, artifact.ArtifactID, reason)
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
