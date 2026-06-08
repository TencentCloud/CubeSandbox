// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package proxypush is the HTTP client the sidecar uses to push lifecycle
// metadata + state to one or more CubeProxy admin endpoints, and to pull the
// per-request last_active timestamps back. The protocol is documented in
// CubeProxy/lua/admin_phase.lua — this file is the canonical Go peer.
package proxypush

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/tencentcloud/CubeSandbox/CubeProxy/sidecar/internal/lifecycle"
)

// Client fans pushes out to every configured CubeProxy admin URL and merges
// pulls (last_active) by max(timestamp).
type Client struct {
	endpoints []string
	token     string
	httpc     *http.Client
	log       *zap.Logger
}

func New(endpoints []string, token string, timeout time.Duration, log *zap.Logger) *Client {
	return &Client{
		endpoints: endpoints,
		token:     token,
		httpc:     &http.Client{Timeout: timeout},
		log:       log,
	}
}

// LastActiveResponse mirrors the JSON body returned by GET /admin/last_active.
type LastActiveResponse struct {
	Now     int64            `json:"now"`
	Since   int64            `json:"since"`
	Count   int              `json:"count"`
	Entries map[string]int64 `json:"entries"`
}

// UpsertMeta pushes one sandbox's metadata to every CubeProxy.
func (c *Client) UpsertMeta(ctx context.Context, meta lifecycle.SandboxLifecycleMeta) error {
	body, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	return c.broadcast(ctx, http.MethodPost, "/admin/meta/upsert", body)
}

// DeleteMeta drops a sandbox from every CubeProxy.
func (c *Client) DeleteMeta(ctx context.Context, sandboxID string) error {
	body, err := json.Marshal(map[string]string{"sandbox_id": sandboxID})
	if err != nil {
		return fmt.Errorf("marshal delete: %w", err)
	}
	return c.broadcast(ctx, http.MethodPost, "/admin/meta/delete", body)
}

// SetState pushes a state transition to every CubeProxy.
// state must be one of "running" | "pausing" | "paused".
func (c *Client) SetState(ctx context.Context, sandboxID, state string) error {
	body, err := json.Marshal(map[string]string{
		"sandbox_id": sandboxID,
		"state":      state,
	})
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	return c.broadcast(ctx, http.MethodPost, "/admin/state", body)
}

// PullLastActive queries every CubeProxy for entries newer than `since` and
// merges them by max(ts). Returns merged entries plus the *minimum* `now`
// across responses (callers use that as the next `since`, ensuring no gap).
//
// Endpoint failures are logged at warn and skipped: a single CubeProxy being
// unreachable shouldn't blind the sweeper to entries on the others.
func (c *Client) PullLastActive(ctx context.Context, since int64) (map[string]int64, int64, error) {
	merged := make(map[string]int64)
	minNow := int64(0)
	first := true
	var (
		successes int
		lastErr   error
	)

	for _, url := range c.endpoints {
		path := "/admin/last_active?since=" + strconv.FormatInt(since, 10)
		raw, err := c.do(ctx, http.MethodGet, url, path, nil)
		if err != nil {
			c.log.Warn("pull last_active failed", zap.String("url", url), zap.Error(err))
			lastErr = err
			continue
		}
		var resp LastActiveResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			c.log.Warn("pull last_active: bad json", zap.String("url", url), zap.Error(err))
			lastErr = err
			continue
		}
		for sid, ts := range resp.Entries {
			if cur, ok := merged[sid]; !ok || ts > cur {
				merged[sid] = ts
			}
		}
		if first || resp.Now < minNow {
			minNow = resp.Now
			first = false
		}
		successes++
	}

	if successes == 0 {
		if lastErr == nil {
			lastErr = errors.New("no admin endpoints succeeded")
		}
		return nil, 0, lastErr
	}
	return merged, minNow, nil
}

// broadcast fans out a write to every endpoint. Returns an error only when
// every endpoint failed; partial success returns nil but logs the failures
// (CubeProxy is the consumer and will eventually reconverge from the next
// stream replay).
func (c *Client) broadcast(ctx context.Context, method, path string, body []byte) error {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []error
		ok       int
	)

	for _, url := range c.endpoints {
		wg.Add(1)
		url := url
		go func() {
			defer wg.Done()
			if _, err := c.do(ctx, method, url, path, body); err != nil {
				c.log.Warn("admin push failed",
					zap.String("url", url),
					zap.String("path", path),
					zap.Error(err))
				mu.Lock()
				failures = append(failures, fmt.Errorf("%s: %w", url, err))
				mu.Unlock()
				return
			}
			mu.Lock()
			ok++
			mu.Unlock()
		}()
	}
	wg.Wait()

	if ok == 0 && len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, base, path string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("X-Cube-Admin-Token", c.token)
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("status=%d body=%q", resp.StatusCode, respBody)
	}
	return respBody, nil
}
