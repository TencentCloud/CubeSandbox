// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodemeta

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
)

// CubeOpsLoader pulls the authoritative node view from CubeOps for
// CubeMaster's localcache. It implements the loader shape registered via
// localcache.RegisterNodeLoader.
type CubeOpsLoader struct {
	baseURL string
	client  *http.Client
	// bootRetries: extra LoadNodes attempts on failure. 0 = single-shot.
	bootRetries int
	bootBackoff time.Duration
}

// NewCubeOpsLoader creates a CubeOps node-view loader. baseURL must include
// scheme and host. Single-shot by default; chain WithBootRetry for retries.
func NewCubeOpsLoader(baseURL string) *CubeOpsLoader {
	return &CubeOpsLoader{
		baseURL: baseURL,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// WithBootRetry retries on failure with exponential backoff. retries is the
// number of extra attempts after the first one.
func (l *CubeOpsLoader) WithBootRetry(retries int, backoff time.Duration) *CubeOpsLoader {
	if retries < 0 {
		retries = 0
	}
	l.bootRetries = retries
	l.bootBackoff = backoff
	return l
}

// LoadNodes fetches the full node view from CubeOps /internal/v1/nodes.
// Retries per WithBootRetry; returns ctx.Err() if cancelled between attempts.
func (l *CubeOpsLoader) LoadNodes(ctx context.Context) ([]*node.Node, error) {
	var lastErr error
	for attempt := 0; attempt <= l.bootRetries; attempt++ {
		if attempt > 0 {
			wait := l.bootBackoff << (attempt - 1)
			log.G(ctx).Warnf("cubeops_loader retry attempt=%d wait=%s last_err=%v", attempt+1, wait, lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		nodes, err := l.loadOnce(ctx)
		if err == nil {
			if attempt > 0 {
				log.G(ctx).Infof("cubeops_loader recovered after %d retries nodes=%d", attempt, len(nodes))
			}
			return nodes, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (l *CubeOpsLoader) loadOnce(ctx context.Context) ([]*node.Node, error) {
	u, err := url.Parse(l.baseURL + "/internal/v1/nodes")
	if err != nil {
		return nil, fmt.Errorf("invalid cubeops url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}

	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cubeops request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("cubeops returned %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read cubeops response: %w", err)
	}

	var nodes []*node.Node
	if err := json.Unmarshal(body, &nodes); err != nil {
		return nil, fmt.Errorf("unmarshal cubeops nodes: %w", err)
	}

	now := time.Now()
	for _, n := range nodes {
		if n != nil {
			n.MetaDataUpdateAt = now
		}
	}

	log.G(ctx).Debugf("cubeops_loader nodes=%d", len(nodes))
	return nodes, nil
}

// cubeopsNodeLoader wraps LoadNodes to match localcache.RegisterNodeLoader.
func cubeopsNodeLoader(loader *CubeOpsLoader) func(context.Context) ([]*node.Node, error) {
	return func(ctx context.Context) ([]*node.Node, error) {
		return loader.LoadNodes(ctx)
	}
}
