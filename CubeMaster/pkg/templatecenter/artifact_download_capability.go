// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

const ArtifactDownloadCapabilityTTL = config.DefaultArtifactDownloadCapabilityTTL

const artifactDownloadCapabilityDomain = "cube-template-artifact-download/v1"

// artifactCapabilityOnlyKey marks a request that arrived on a route mounted
// OUTSIDE the user API-key middleware.
type artifactCapabilityOnlyKey struct{}

// WithCapabilityOnlyArtifactAuth marks ctx as belonging to an unauthenticated
// route, where the long-lived per-artifact DownloadToken must NOT be accepted.
//
// That token is effectively a bearer password with no expiry: it is stored in
// plaintext on the artifact row, handed to every Cubelet that ever pulled the
// artifact, and only rotates on re-finalize. Behind the API-key middleware
// that is tolerable, because the caller is already authenticated. On the
// direct-download route there is no second factor, so a single leaked token
// would grant permanent access to the artifact's bytes. Short-lived
// artifact-bound capabilities are the only credential accepted there.
func WithCapabilityOnlyArtifactAuth(ctx context.Context) context.Context {
	return context.WithValue(ctx, artifactCapabilityOnlyKey{}, true)
}

// CapabilityOnlyArtifactAuth reports whether ctx was marked by
// WithCapabilityOnlyArtifactAuth.
func CapabilityOnlyArtifactAuth(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	only, _ := ctx.Value(artifactCapabilityOnlyKey{}).(bool)
	return only
}

// NewArtifactDownloadCapability creates an artifact-bound token using the
// one-hour compatibility default. Production issuance should use the TTL from
// CubeMaster's runtime configuration.
func NewArtifactDownloadCapability(artifactID string, now time.Time) (string, error) {
	return NewArtifactDownloadCapabilityWithTTL(artifactID, ArtifactDownloadCapabilityTTL, now)
}

// NewArtifactDownloadCapabilityWithTTL signs an artifact-bound token with an
// explicit expiry. CubeMaster and CubeTemplateCenter share the callback secret,
// but the domain separator prevents reuse as a callback credential.
func NewArtifactDownloadCapabilityWithTTL(artifactID string, ttl time.Duration, now time.Time) (string, error) {
	if ttl <= 0 || ttl > config.MaxArtifactDownloadCapabilityTTL {
		return "", fmt.Errorf("artifact download capability TTL must be in (0, %s]", config.MaxArtifactDownloadCapabilityTTL)
	}
	secret := strings.TrimSpace(os.Getenv(constants.TemplateCallbackTokenEnv))
	if secret == "" {
		return "", fmt.Errorf("%s is not configured", constants.TemplateCallbackTokenEnv)
	}
	if artifactID == "" {
		return "", fmt.Errorf("artifact id is empty")
	}
	expiresAt := now.Add(ttl).Unix()
	message := artifactDownloadCapabilityMessage(artifactID, expiresAt)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(message)
	return fmt.Sprintf("v1.%d.%s", expiresAt, base64.RawURLEncoding.EncodeToString(mac.Sum(nil))), nil
}

// ValidArtifactDownloadCapability validates the token, artifact binding, and
// short server-enforced TTL. The request cannot extend the expiry encoded by
// CubeMaster or use a token minted for a different artifact.
func ValidArtifactDownloadCapability(artifactID, token string, now time.Time) bool {
	secret := strings.TrimSpace(os.Getenv(constants.TemplateCallbackTokenEnv))
	if secret == "" || artifactID == "" {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return false
	}
	expiresAt, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	nowUnix := now.Unix()
	if expiresAt <= nowUnix || expiresAt > now.Add(config.MaxArtifactDownloadCapabilityTTL+time.Second).Unix() {
		return false
	}
	provided, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(artifactDownloadCapabilityMessage(artifactID, expiresAt))
	return subtle.ConstantTimeCompare(provided, mac.Sum(nil)) == 1
}

func artifactDownloadCapabilityMessage(artifactID string, expiresAt int64) []byte {
	return []byte(artifactDownloadCapabilityDomain + "\n" + artifactID + "\n" + strconv.FormatInt(expiresAt, 10))
}
