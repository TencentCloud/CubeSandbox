// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package content

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ErrVolumeNotFound means the control plane no longer has the volume, for
// example because it was deleted after the token was issued.
var ErrVolumeNotFound = errors.New("volume not found")

// VolumeChecker answers whether a volume still exists. It runs before any
// plugin Attach, so a token for a deleted volume can never make a plugin mount
// or recreate it. A nil error means the volume exists, ErrVolumeNotFound means
// it does not, and any other error means the answer is unknown (callers must
// fail closed).
type VolumeChecker interface {
	Exists(ctx context.Context, volumeID string) error
}

const (
	// foundTTL bounds how long a "volume exists" answer is trusted. It is also
	// how long a deleted volume stays reachable with an already-issued token.
	foundTTL = 30 * time.Second
	// missingTTL only absorbs bursts of requests for a deleted volume.
	missingTTL = 5 * time.Second

	maxCacheEntries = 4096
	maxMasterBody   = 64 << 10

	// retCodeNotFound is CubeMaster's errorcode.ErrorCode_NotFound.
	retCodeNotFound = 130404
)

var volumeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// MasterChecker asks CubeMaster (GET /cube/volume/{id}) whether a volume still
// exists, and caches the answer briefly so the data plane does not call the
// control plane on every request. Errors are never cached.
type MasterChecker struct {
	// Base returns the CubeMaster HTTP base URL. It is a function so it is
	// resolved when needed, after Cubelet's configuration has been loaded.
	Base func() string
	HTTP *http.Client
	Now  func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
	group singleflight.Group
}

type cacheEntry struct {
	found   bool
	expires time.Time
}

// NewMasterChecker builds a checker with a bounded request timeout.
func NewMasterChecker(base func() string) *MasterChecker {
	return &MasterChecker{Base: base, HTTP: &http.Client{Timeout: 3 * time.Second}, Now: time.Now}
}

// Only the two fields needed are decoded; the response also carries a freshly
// minted content token, which must never be read or kept here.
type masterVolumeResponse struct {
	Ret struct {
		RetCode int    `json:"ret_code"`
		RetMsg  string `json:"ret_msg"`
	} `json:"ret"`
	Volume *struct {
		VolumeID string `json:"volumeID"`
	} `json:"volume"`
}

func (c *MasterChecker) Exists(ctx context.Context, volumeID string) error {
	if !volumeIDRe.MatchString(volumeID) {
		return ErrVolumeNotFound
	}
	if found, ok := c.cached(volumeID); ok {
		return foundErr(found)
	}
	// Collapse concurrent misses for the same volume into one CubeMaster call.
	v, err, _ := c.group.Do(volumeID, func() (any, error) {
		// A caller's cancellation must not poison the shared answer.
		return c.fetch(context.WithoutCancel(ctx), volumeID)
	})
	if err != nil {
		return err
	}
	return foundErr(v.(bool))
}

func foundErr(found bool) error {
	if found {
		return nil
	}
	return ErrVolumeNotFound
}

func (c *MasterChecker) cached(volumeID string) (found, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, hit := c.cache[volumeID]
	if !hit || !c.Now().Before(e.expires) {
		return false, false
	}
	return e.found, true
}

func (c *MasterChecker) store(volumeID string, found bool) {
	ttl := missingTTL
	if found {
		ttl = foundTTL
	}
	now := c.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = make(map[string]cacheEntry)
	}
	if len(c.cache) >= maxCacheEntries {
		for k, e := range c.cache {
			if !now.Before(e.expires) {
				delete(c.cache, k)
			}
		}
		if len(c.cache) >= maxCacheEntries {
			clear(c.cache)
		}
	}
	c.cache[volumeID] = cacheEntry{found: found, expires: now.Add(ttl)}
}

func (c *MasterChecker) fetch(ctx context.Context, volumeID string) (bool, error) {
	base := strings.TrimRight(strings.TrimSpace(c.Base()), "/")
	if base == "" {
		return false, errors.New("cubemaster address is not configured")
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/cube/volume/"+url.PathEscape(volumeID), nil)
	if err != nil {
		return false, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMasterBody))
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("cubemaster volume lookup: http %d", resp.StatusCode)
	}
	var out masterVolumeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return false, fmt.Errorf("cubemaster volume lookup: decode: %w", err)
	}
	switch {
	case out.Ret.RetCode == retCodeNotFound:
		c.store(volumeID, false)
		return false, nil
	case out.Ret.RetCode != 0:
		return false, fmt.Errorf("cubemaster volume lookup: ret_code %d", out.Ret.RetCode)
	case out.Volume == nil || out.Volume.VolumeID != volumeID:
		return false, errors.New("cubemaster volume lookup: unexpected volume in response")
	}
	c.store(volumeID, true)
	return true, nil
}
