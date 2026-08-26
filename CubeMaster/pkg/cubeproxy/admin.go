// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package cubeproxy talks to CubeProxy admin endpoints (routing-cache purge).
package cubeproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/rediskey"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
)

const (
	backendCacheDeletePath = "/admin/backend_cache/delete"
	defaultAdminTimeout    = 3 * time.Second
	// purgeAttempts covers a replica reloading or a blipped connection.
	// Past that the endpoint is not going to answer and waiting only
	// delays telling the caller.
	purgeAttempts = 3
	purgeBackoff  = 500 * time.Millisecond
)

// Endpoint mirrors CubeProxy's registry Hash value / CLM discovery.Endpoint.
type Endpoint struct {
	ProxyID  string `json:"proxy_id"`
	AdminURL string `json:"admin_url"`
	NodeIP   string `json:"node_ip,omitempty"`
}

var (
	httpClient = &http.Client{Timeout: defaultAdminTimeout}
	// listAdminURLsFn is overridable in tests.
	listAdminURLsFn = listAdminURLs
	doDeleteFn      = postBackendCacheDelete
)

// InvalidateBackendCache asks every live CubeProxy to drop local_cache routing
// entries for sandboxID, retrying the replicas that refuse.
//
// The error is not advisory. A cache hit renews the entry's TTL, so a mapping
// we fail to purge never expires on its own: every later request keeps landing
// on the pre-resume backend and the sandbox is unreachable, not slower. One
// replica left holding a stale entry is enough, so a partial purge is a
// failure too.
func InvalidateBackendCache(ctx context.Context, sandboxID, fallbackHostIP string) error {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil
	}
	urls := listAdminURLsFn(ctx, fallbackHostIP)
	if len(urls) == 0 {
		return fmt.Errorf("no CubeProxy admin endpoint known for sandbox %s", sandboxID)
	}

	pending := urls
	var reasons []string
	for attempt := 1; attempt <= purgeAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(purgeBackoff):
			}
		}
		pending, reasons = purgeOnce(ctx, pending, sandboxID)
		if len(pending) == 0 {
			log.G(ctx).Infof("cubeproxy: backend_cache deleted sandbox=%s replicas=%d attempt=%d",
				sandboxID, len(urls), attempt)
			return nil
		}
	}
	return fmt.Errorf("%d of %d CubeProxy replica(s) still hold sandbox %s: %s",
		len(pending), len(urls), sandboxID, strings.Join(reasons, "; "))
}

// purgeOnce broadcasts to every url and returns the ones still to convince,
// alongside why each refused.
func purgeOnce(ctx context.Context, urls []string, sandboxID string) (pending, reasons []string) {
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for _, u := range urls {
		wg.Add(1)
		url := u
		go func() {
			defer wg.Done()
			err := doDeleteFn(ctx, url, sandboxID)
			if err == nil {
				return
			}
			mu.Lock()
			pending = append(pending, url)
			reasons = append(reasons, fmt.Sprintf("%s: %v", url, err))
			mu.Unlock()
		}()
	}
	wg.Wait()
	return pending, reasons
}

func listAdminURLs(ctx context.Context, fallbackHostIP string) []string {
	cfg := config.GetConfig()
	var conf *config.CubeProxyConf
	if cfg != nil {
		conf = cfg.CubeProxyConf
	}
	if conf != nil {
		if static := normalizeAdminURLs(conf.AdminURLs); len(static) > 0 {
			return static
		}
	}

	urls := listAdminURLsFromRegistry(ctx, conf)
	if len(urls) > 0 {
		return urls
	}

	host := strings.TrimSpace(fallbackHostIP)
	if host == "" {
		return nil
	}
	port := 8082
	if conf != nil && conf.AdminPort > 0 {
		port = conf.AdminPort
	}
	return []string{fmt.Sprintf("http://%s:%d", host, port)}
}

func normalizeAdminURLs(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, u := range in {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" {
			continue
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			u = "http://" + u
		}
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

func listAdminURLsFromRegistry(ctx context.Context, conf *config.CubeProxyConf) []string {
	conn := wrapredis.GetRedis()
	if conn == nil {
		return nil
	}
	values, err := redis.StringMap(conn.Do("HGETALL", rediskey.CubeProxyRegistry()))
	if err != nil || len(values) == 0 {
		return nil
	}

	ttlMs := int64(15000)
	if conf != nil && conf.HeartbeatTTLMs > 0 {
		ttlMs = conf.HeartbeatTTLMs
	}
	live := liveProxyIDs(ctx, conn, ttlMs)

	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for proxyID, raw := range values {
		if len(live) > 0 {
			if _, ok := live[proxyID]; !ok {
				continue
			}
		}
		var ep Endpoint
		if err := json.Unmarshal([]byte(raw), &ep); err != nil {
			log.G(ctx).Warnf("cubeproxy: bad registry entry id=%s: %v", proxyID, err)
			continue
		}
		u := strings.TrimRight(strings.TrimSpace(ep.AdminURL), "/")
		if u == "" {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

func liveProxyIDs(ctx context.Context, conn *wrapredis.RedisWrap, ttlMs int64) map[string]struct{} {
	if ttlMs <= 0 {
		return nil
	}
	nowMs := time.Now().UnixMilli()
	minScore := nowMs - ttlMs
	members, err := redis.Strings(conn.Do("ZRANGEBYSCORE", rediskey.CubeProxyHeartbeat(), minScore, "+inf"))
	if err != nil {
		log.G(ctx).Debugf("cubeproxy: heartbeat read failed: %v", err)
		return nil
	}
	live := make(map[string]struct{}, len(members))
	for _, id := range members {
		live[id] = struct{}{}
	}
	return live
}

func postBackendCacheDelete(ctx context.Context, adminURL, sandboxID string) error {
	body, err := json.Marshal(map[string]string{"sandbox_id": sandboxID})
	if err != nil {
		return err
	}
	url := strings.TrimRight(adminURL, "/") + backendCacheDeletePath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg := config.GetConfig(); cfg != nil && cfg.CubeProxyConf != nil {
		if tok := strings.TrimSpace(cfg.CubeProxyConf.AdminToken); tok != "" {
			req.Header.Set("X-Cube-Admin-Token", tok)
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusNotFound {
		// The replica answered, it just has no such route. Say so: a
		// CubeProxy predating the admin endpoint looks exactly like a
		// healthy one until a resumed sandbox turns out unreachable.
		return fmt.Errorf("no %s route; this CubeProxy is too old to purge its routing cache",
			backendCacheDeletePath)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
