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
)

// StoreHandler handles store image metadata HTTP requests.
type StoreHandler struct{}

// NewStoreHandler creates a new store handler.
func NewStoreHandler() *StoreHandler {
	return &StoreHandler{}
}

var storeImages = []string{
	"cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest",
	"cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest",
	"ghcr.io/tencentcloud/cubesandbox-base:latest",
}

// ImageMeta is the per-image metadata entry.
type ImageMeta struct {
	Image       string   `json:"image"`
	SizeBytes   uint64   `json:"sizeBytes"`
	SizeMB      float64  `json:"sizeMb"`
	Digest      *string  `json:"digest"`
	DigestShort *string  `json:"digestShort"`
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
func (h *StoreHandler) GetStoreMeta(w http.ResponseWriter, r *http.Request) {
	var mu sync.Mutex
	images := make([]ImageMeta, 0, len(storeImages))
	var wg sync.WaitGroup

	for _, img := range storeImages {
		wg.Add(1)
		go func(image string) {
			defer wg.Done()
			meta := inspectImage(image)
			if meta != nil {
				mu.Lock()
				images = append(images, *meta)
				mu.Unlock()
			}
		}(img)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, StoreMeta{Images: images})
}

// RefreshStoreMeta handles POST /store/refresh.
func (h *StoreHandler) RefreshStoreMeta(w http.ResponseWriter, r *http.Request) {
	var mu sync.Mutex
	images := make([]ImageMeta, 0, len(storeImages))
	var wg sync.WaitGroup

	for _, img := range storeImages {
		wg.Add(1)
		go func(image string) {
			defer wg.Done()
			// Pull (ignore errors — image might not be accessible)
			_ = exec.Command("docker", "pull", "--quiet", image).Run()
			// Inspect regardless (use cached if pull failed)
			meta := inspectImage(image)
			if meta != nil {
				mu.Lock()
				images = append(images, *meta)
				mu.Unlock()
			}
		}(img)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, StoreMeta{Images: images})
}

func inspectImage(image string) *ImageMeta {
	output, err := exec.Command("docker", "image", "inspect", "--format", "{{json .}}", image).Output()
	if err != nil {
		return nil // image not present locally
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

	sizeMB := float64(info.Size) / (1024.0 * 1024.0)
	sizeMB = math.Round(sizeMB*10) / 10 // round to 1 decimal

	// Pick the digest that matches the queried registry (first match wins)
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
