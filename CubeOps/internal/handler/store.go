// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"encoding/json"
	"math"
	"net/http"
	"os/exec"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
)

// StoreHandler handles store image metadata HTTP requests.
type StoreHandler struct{}

// NewStoreHandler creates a new store handler.
func NewStoreHandler() *StoreHandler { return &StoreHandler{} }

// Register installs the store routes on the given router group.
func (h *StoreHandler) Register(r *gin.RouterGroup) {
	r.GET("/store/meta", h.GetStoreMeta)
	r.POST("/store/refresh", h.RefreshStoreMeta)
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

type dockerInspectResult struct {
	ID          string   `json:"Id"`
	Size        uint64   `json:"Size"`
	RepoDigests []string `json:"RepoDigests"`
}

// GetStoreMeta handles GET /store/meta.
func (h *StoreHandler) GetStoreMeta(c *gin.Context) {
	httputil.WriteJSON(c, http.StatusOK, StoreMeta{Images: inspectAll(storeImages, false)})
}

// RefreshStoreMeta handles POST /store/refresh.
func (h *StoreHandler) RefreshStoreMeta(c *gin.Context) {
	httputil.WriteJSON(c, http.StatusOK, StoreMeta{Images: inspectAll(storeImages, true)})
}

// inspectAll concurrently inspects each image and returns the metadata
// slice. When pull is true, it also tries to docker pull each image first
// (best-effort — pull failures fall back to whatever the local cache has).
func inspectAll(images []string, pull bool) []ImageMeta {
	var mu sync.Mutex
	out := make([]ImageMeta, 0, len(images))
	var wg sync.WaitGroup
	for _, img := range images {
		wg.Add(1)
		go func(image string) {
			defer wg.Done()
			if pull {
				_ = exec.Command("docker", "pull", "--quiet", image).Run()
			}
			meta := inspectImage(image)
			if meta == nil {
				return
			}
			mu.Lock()
			out = append(out, *meta)
			mu.Unlock()
		}(img)
	}
	wg.Wait()
	return out
}

// inspectImage returns the image metadata by shelling out to docker. The
// result may be nil when the image is not present locally and pull failed.
func inspectImage(image string) *ImageMeta {
	output, err := exec.Command("docker", "image", "inspect", "--format", "{{json .}}", image).Output()
	if err != nil {
		return nil
	}

	raw := strings.TrimSpace(string(output))
	// docker inspect may return a JSON array; unwrap the single element
	if strings.HasPrefix(raw, "[") {
		raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]"))
	}

	var info dockerInspectResult
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return nil
	}

	// Round size to 1 decimal place so the response is stable and readable.
	// math.Round returns float64 so we apply the standard "round-half-away-from-zero"
	// rounding by multiplying, rounding, then dividing.
	sizeMB := float64(info.Size) / (1024.0 * 1024.0)
	sizeMB = math.Round(sizeMB*10) / 10

	// Pick the digest that matches the queried registry (first match wins);
	// fall back to the first available digest if no match is found.
	registry := ""
	if parts := strings.SplitN(image, "/", 2); len(parts) > 0 {
		registry = parts[0]
	}
	var digest *string
	for _, d := range info.RepoDigests {
		if strings.HasPrefix(d, registry) {
			dCopy := d
			digest = &dCopy
			break
		}
	}
	if digest == nil && len(info.RepoDigests) > 0 {
		dCopy := info.RepoDigests[0]
		digest = &dCopy
	}

	var digestShort *string
	if digest != nil {
		if parts := strings.SplitN(*digest, "@", 2); len(parts) > 1 {
			dsCopy := parts[1]
			digestShort = &dsCopy
		}
	}

	return &ImageMeta{
		Image:       image,
		SizeBytes:   info.Size,
		SizeMB:      sizeMB,
		Digest:      digest,
		DigestShort: digestShort,
	}
}
