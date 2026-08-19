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
// entries for sandboxID. Best-effort: logs failures and never returns a hard
// error that should abort Resume (Redis is already rewritten).
func InvalidateBackendCache(ctx context.Context, sandboxID, fallbackHostIP string) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return
	}
	urls := listAdminURLsFn(ctx, fallbackHostIP)
	if len(urls) == 0 {
		log.G(ctx).Warnf("cubeproxy: no admin URLs to invalidate backend cache sandbox=%s", sandboxID)
		return
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		okN  int
		errs []string
	)
	for _, u := range urls {
		wg.Add(1)
		url := u
		go func() {
			defer wg.Done()
			if err := doDeleteFn(ctx, url, sandboxID); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("%s: %v", url, err))
				mu.Unlock()
				return
			}
			mu.Lock()
			okN++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if okN == 0 && len(errs) > 0 {
		log.G(ctx).Warnf("cubeproxy: backend_cache delete all failed sandbox=%s errs=%v", sandboxID, errs)
		return
	}
	if len(errs) > 0 {
		log.G(ctx).Warnf("cubeproxy: backend_cache delete partial sandbox=%s ok=%d errs=%v", sandboxID, okN, errs)
		return
	}
	log.G(ctx).Infof("cubeproxy: backend_cache deleted sandbox=%s replicas=%d", sandboxID, okN)
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
