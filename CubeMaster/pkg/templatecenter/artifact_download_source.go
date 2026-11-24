// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/pkgs/artifactsource"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
)

// Keep the existing names used by this package's callers, but derive their
// wire values from the shared artifactsource contract consumed by Cubelet.
const (
	ArtifactSourceS3     = string(artifactsource.S3)
	ArtifactSourceTC     = string(artifactsource.TemplateCenter)
	ArtifactSourceMaster = string(artifactsource.CubeMaster)
	ArtifactSourceLegacy = string(artifactsource.Legacy)
)

type ArtifactDownloadSource struct {
	URL       string
	Token     string
	Kind      string
	ExpiresAt int64
}

var (
	tcAddrWarnOnce                   sync.Once
	artifactCapabilitySecretWarnOnce sync.Once
	artifactDirectDisabledWarnOnce   sync.Once
)

// artifactTCSourceBaseURL resolves the TC URL handed to Cubelets. Only an
// address that is already routable is used; anything loopback or wildcard
// returns empty so source selection falls back to the Master route.
//
// Deliberately NOT the Master reachability policy: rewriting a loopback
// address to the shared node IP assumes the service also listens on that
// interface. That holds for CubeMaster (one-click binds 0.0.0.0 by default)
// but not for TC, which one-click binds to 127.0.0.1 because only the
// co-located Master calls it. Rewriting TC's address would hand every Cubelet
// a nodeIP:8090 URL with no listener behind it, so a deployment running on
// local disk (no object store) would fail every template creation.
var artifactTCSourceBaseURL = func() string {
	// TemplateCenterNodeAddr checks the environment before dereferencing its
	// receiver, so this also honors CUBE_TEMPLATE_CENTER_NODE_ADDR before the
	// global config has completed initialization.
	addr := strings.TrimSpace(config.GetConfig().TemplateCenterNodeAddr())
	if addr == "" {
		return ""
	}
	if ExternallyUsableBaseURL(addr) {
		return NormalizeBaseURL(addr)
	}
	tcAddrWarnOnce.Do(func() {
		log.G(context.Background()).Warnf(
			"%s=%q is wildcard/loopback, so it is not reachable from other nodes; direct-TC downloads fall back to the Master path. Set it to a routable address to enable them",
			config.EnvTemplateCenterNodeAddr, addr)
	})
	return ""
}

// IssueArtifactDownloadSource uses the configured TTL and selects an available
// direct source from the artifact storage metadata and configured endpoints.
func IssueArtifactDownloadSource(ctx context.Context, artifact *models.RootfsArtifact, masterBaseURL string, now time.Time) (*ArtifactDownloadSource, error) {
	ttl := config.DefaultArtifactDownloadCapabilityTTL
	if cfg := config.GetConfig(); cfg != nil && cfg.Common != nil && cfg.Common.ArtifactDownloadCapabilityTTL > 0 {
		ttl = cfg.Common.ArtifactDownloadCapabilityTTL
	}
	return issueArtifactDownloadSourceWithOptions(ctx, artifact, masterBaseURL, ttl, now)
}

// IssueArtifactDownloadSourceWithTTL chooses a source with an explicit TTL.
func IssueArtifactDownloadSourceWithTTL(ctx context.Context, artifact *models.RootfsArtifact, masterBaseURL string, ttl time.Duration, now time.Time) (*ArtifactDownloadSource, error) {
	return issueArtifactDownloadSourceWithOptions(ctx, artifact, masterBaseURL, ttl, now)
}

func issueArtifactDownloadSourceWithOptions(ctx context.Context, artifact *models.RootfsArtifact, masterBaseURL string, ttl time.Duration, now time.Time) (*ArtifactDownloadSource, error) {
	if artifact == nil || strings.TrimSpace(artifact.ArtifactID) == "" {
		return nil, fmt.Errorf("artifact is nil or has no id")
	}
	if ttl <= 0 || ttl > config.MaxArtifactDownloadCapabilityTTL {
		return nil, fmt.Errorf("artifact download TTL must be in (0, %s]", config.MaxArtifactDownloadCapabilityTTL)
	}
	// Rollout kill switch. The legacy proxied URL is always a valid source, so
	// an operator can disable every direct endpoint at once and lose only the
	// bandwidth saving, not the ability to create templates.
	if !config.GetConfig().ArtifactDownloadDirectEnabled() {
		artifactDirectDisabledWarnOnce.Do(func() {
			log.G(ctx).Warnf(
				"direct artifact distribution is disabled (%s / common.artifact_download_direct_enabled); every Cubelet will download through CubeMaster",
				config.EnvArtifactDownloadDirect)
		})
		return &ArtifactDownloadSource{
			URL:   buildDownloadURL(strings.TrimRight(strings.TrimSpace(masterBaseURL), "/"), artifact.ArtifactID, artifact.DownloadToken),
			Token: artifact.DownloadToken,
			Kind:  ArtifactSourceLegacy,
		}, nil
	}
	if ArtifactUsesObjectStore(artifact) {
		if signedURL, err := presignArtifactGetURLWithTTL(ctx, artifact, ttl); err == nil && signedURL != "" {
			return &ArtifactDownloadSource{
				URL:       signedURL,
				Kind:      ArtifactSourceS3,
				ExpiresAt: now.Add(ttl).Unix(),
			}, nil
		}
	}

	baseURL := strings.TrimRight(strings.TrimSpace(masterBaseURL), "/")
	kind := ArtifactSourceMaster
	if directTCArtifactSource(artifact) {
		if tcURL := strings.TrimRight(strings.TrimSpace(artifactTCSourceBaseURL()), "/"); tcURL != "" {
			baseURL = tcURL
			kind = ArtifactSourceTC
		}
	}
	if baseURL == "" {
		return nil, fmt.Errorf("artifact download source base URL is empty")
	}
	endpoint, err := artifactDownloadEndpoint(baseURL, artifact.ArtifactID)
	if err != nil {
		return nil, err
	}
	token, err := NewArtifactDownloadCapabilityWithTTL(artifact.ArtifactID, ttl, now)
	if err != nil {
		// Signing failures are currently caused by an unset shared secret.
		// Preserve the legacy rolling-upgrade path, but make the security
		// downgrade visible without logging once per target node.
		artifactCapabilitySecretWarnOnce.Do(func() {
			log.G(ctx).Errorf(
				"artifact capability signing failed (%v); falling back to the long-lived legacy token; set %s on CubeMaster to enable short-lived capabilities",
				err, constants.TemplateCallbackTokenEnv)
		})
		legacyBaseURL := strings.TrimRight(strings.TrimSpace(masterBaseURL), "/")
		if legacyBaseURL == "" {
			legacyBaseURL = baseURL
		}
		return &ArtifactDownloadSource{
			URL:   buildDownloadURL(legacyBaseURL, artifact.ArtifactID, artifact.DownloadToken),
			Token: artifact.DownloadToken,
			Kind:  ArtifactSourceLegacy,
		}, nil
	}
	return &ArtifactDownloadSource{
		URL:       endpoint,
		Token:     token,
		Kind:      kind,
		ExpiresAt: now.Add(ttl).Unix(),
	}, nil
}

func setArtifactDownloadAnnotations(annotations map[string]string, legacyURL, legacyToken string, source *ArtifactDownloadSource) {
	annotations[constants.CubeAnnotationRootfsArtifactURL] = legacyURL
	annotations[constants.CubeAnnotationRootfsArtifactToken] = legacyToken
	delete(annotations, constants.CubeAnnotationRootfsArtifactDirectURL)
	delete(annotations, constants.CubeAnnotationRootfsArtifactDirectToken)
	delete(annotations, constants.CubeAnnotationRootfsArtifactSource)
	delete(annotations, constants.CubeAnnotationRootfsArtifactExpiresAt)
	if source == nil || source.Kind == ArtifactSourceLegacy {
		return
	}
	annotations[constants.CubeAnnotationRootfsArtifactDirectURL] = source.URL
	if source.Token != "" {
		annotations[constants.CubeAnnotationRootfsArtifactDirectToken] = source.Token
	}
	annotations[constants.CubeAnnotationRootfsArtifactSource] = source.Kind
	annotations[constants.CubeAnnotationRootfsArtifactExpiresAt] = strconv.FormatInt(source.ExpiresAt, 10)
}

func directTCArtifactSource(artifact *models.RootfsArtifact) bool {
	if artifact == nil {
		return false
	}
	if artifact.StorageBackend == "fs" || blobstore.IsObjectLocator(strings.TrimSpace(artifact.ArtifactURL)) {
		return true
	}
	return artifactServedByRemoteTier()
}

func artifactDownloadEndpoint(baseURL, artifactID string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid artifact source base URL %q", baseURL)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/cube/template/artifact/direct-download"
	query := u.Query()
	query.Set("artifact_id", artifactID)
	query.Del("token")
	u.RawQuery = query.Encode()
	return u.String(), nil
}
