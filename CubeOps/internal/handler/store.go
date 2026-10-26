// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
)

// StoreHandler handles store image metadata HTTP requests.
type StoreHandler struct {
	registryClient RegistryClient
}

// NewStoreHandler creates a new store handler.
func NewStoreHandler(registryClient RegistryClient) *StoreHandler {
	return &StoreHandler{registryClient: registryClient}
}

// Register installs the store routes on the given router group.
func (h *StoreHandler) Register(r *gin.RouterGroup) {
	r.GET("/store/meta", h.GetStoreMeta)
	r.POST("/store/refresh", StoreRefreshRateLimit(), h.RefreshStoreMeta)
}

var storeImages = []string{
	"cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest",
	"cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest",
	"ghcr.io/tencentcloud/cubesandbox-base:latest",
}

// ImageMeta is the per-image metadata entry.
type ImageMeta struct {
	Image       string  `json:"image"`
	SizeBytes   uint64  `json:"sizeBytes"`
	SizeMB      float64 `json:"sizeMb"`
	Digest      *string `json:"digest"`
	DigestShort *string `json:"digestShort"`
}

// StoreMeta is the response for GET /store/meta.
type StoreMeta struct {
	Images []ImageMeta `json:"images"`
}

// GetStoreMeta handles GET /store/meta.
//
// Returns cached metadata for the curated image set without contacting
// any registry. Images with no prior refresh are still listed so the
// frontend can render them.
func (h *StoreHandler) GetStoreMeta(c *gin.Context) {
	httputil.WriteJSON(c, http.StatusOK, StoreMeta{Images: h.inspectAll(c.Request.Context(), false)})
}

// RefreshStoreMeta handles POST /store/refresh.
//
// Resolves each curated image's latest manifest digest from upstream
// registries via go-containerregistry. Only the manifest and config
// blob are fetched (no layers, no docker daemon).
func (h *StoreHandler) RefreshStoreMeta(c *gin.Context) {
	images := h.inspectAll(c.Request.Context(), true)
	httputil.WriteJSON(c, http.StatusOK, StoreMeta{Images: images})
}

// inspectAll resolves metadata for every curated image in parallel
// and returns the results in storeImages order. When refresh is true
// it queries the upstream registry; otherwise it returns the cached
// digest from a previous refresh.
func (h *StoreHandler) inspectAll(ctx context.Context, refresh bool) []ImageMeta {
	out := make([]ImageMeta, len(storeImages))
	var wg sync.WaitGroup
	for i, img := range storeImages {
		wg.Add(1)
		go func(i int, image string) {
			defer wg.Done()

			var meta *ImageMeta
			if refresh {
				meta = h.registryClient.FetchLatest(ctx, image)
			} else {
				meta = h.registryClient.Cached(image)
			}
			if meta == nil {
				// Even when we cannot resolve a digest we still report
				// the image so the frontend can render the store entry.
				meta = &ImageMeta{Image: image}
			}
			out[i] = *meta
		}(i, img)
	}
	wg.Wait()
	return out
}

// ── Registry client ────────────────────────────────────────────────────────

// RegistryClient resolves image metadata from an OCI/Docker registry.
type RegistryClient interface {
	// FetchLatest queries the registry for the current manifest digest
	// of the given image reference. Returns nil if the image cannot be
	// resolved.
	FetchLatest(ctx context.Context, ref string) *ImageMeta
	// Cached returns the most recently fetched metadata for ref, or nil
	// if FetchLatest has not been called successfully for it yet.
	Cached(ref string) *ImageMeta
}

// defaultRegistryClient is the client used by the handler in production.
var defaultRegistryClient RegistryClient = newGCRRegistryClient()

// DefaultRegistryClient returns the package-level registry client used by
// production code. Tests should construct their own fake client and
// pass it to NewStoreHandler.
func DefaultRegistryClient() RegistryClient { return defaultRegistryClient }

// gcrRegistryClient uses google/go-containerregistry to resolve image
// metadata. It keeps an in-memory cache of the most recent result per
// image so GET /store/meta does not hit the network.
type gcrRegistryClient struct {
	cache   sync.Map // ref -> ImageMeta
	authOpt remote.Option
}

func newGCRRegistryClient() *gcrRegistryClient {
	// Store images are public; anonymous access avoids the per-call
	// filesystem I/O of authn.DefaultKeychain.
	return &gcrRegistryClient{
		authOpt: remote.WithAuth(authn.Anonymous),
	}
}

// FetchLatest resolves ref via go-containerregistry, picking the
// linux/amd64 manifest for multi-arch indexes, and caches the result.
// Errors return nil so the handler can emit a placeholder entry;
// registry failures are logged at warn level.
func (c *gcrRegistryClient) FetchLatest(ctx context.Context, ref string) *ImageMeta {
	parsedRef, err := name.ParseReference(ref)
	if err != nil {
		logging.G(ctx).Debugf("store: failed to parse image ref %q: %v", ref, err)
		return nil
	}

	img, err := remote.Image(parsedRef,
		remote.WithContext(ctx),
		c.authOpt,
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}),
	)
	if err != nil {
		logging.G(ctx).Warnf("store: failed to resolve image %q: %v", ref, err)
		return nil
	}

	manifest, err := img.Manifest()
	if err != nil {
		logging.G(ctx).Warnf("store: failed to read manifest for %q: %v", ref, err)
		return nil
	}

	var totalSize int64
	for _, layer := range manifest.Layers {
		totalSize += layer.Size
	}

	digest, err := img.Digest()
	if err != nil {
		logging.G(ctx).Warnf("store: failed to read digest for %q: %v", ref, err)
		return nil
	}

	// Build the canonical "repo@sha256:..." form to match the old
	// docker-inspect RepoDigests output and the CubeMaster
	// imageDigestFromReference convention.
	repoName := parsedRef.Context().Name()
	// Normalise docker.io canonical name: go-containerregistry may
	// return "index.docker.io/<repo>"; we prefer "docker.io/<repo>".
	repoName = strings.Replace(repoName, "index.docker.io/", "docker.io/", 1)
	fullDigest := fmt.Sprintf("%s@%s", repoName, digest.String())
	shortDigest := digest.String()

	sizeMB := float64(totalSize) / (1024.0 * 1024.0)
	sizeMB = math.Round(sizeMB*10) / 10

	meta := &ImageMeta{
		Image:       ref,
		SizeBytes:   uint64(totalSize),
		SizeMB:      sizeMB,
		Digest:      &fullDigest,
		DigestShort: &shortDigest,
	}
	c.cache.Store(ref, *meta)
	return meta
}

func (c *gcrRegistryClient) Cached(ref string) *ImageMeta {
	if v, ok := c.cache.Load(ref); ok {
		m := v.(ImageMeta)
		return &m
	}
	return nil
}
