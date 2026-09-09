// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

type templateLocalitySnapshot struct {
	ReadyReplicas []ReplicaStatus
}

type templateFetchCall struct {
	done chan struct{}
	val  interface{}
	err  error
}

type templateFetchGroup struct {
	mu    sync.Mutex
	calls map[string]*templateFetchCall
}

type templateLockGroup struct {
	locks sync.Map
}

var (
	templateDefinitionCache    = cache.New(templateCacheTTL(), templateCacheTTL())
	templateLocalityReadyCache = cache.New(templateCacheTTL(), templateCacheTTL())
	// templateKindCache caches the template kind ("snapshot"|"app_snapshot"|...)
	// keyed by templateID. The kind is derived from a single column in
	// t_cube_template_definition, so its only mutation source is the same
	// definition write paths that already call invalidateTemplateCaches.
	templateKindCache = cache.New(templateCacheTTL(), templateCacheTTL())
	// templateListCache caches the full ListTemplates result under a single
	// key. Template metadata changes infrequently (minutes to hours between
	// writes), but ListTemplates is polled aggressively by the console/UI, so
	// a short TTL plus write-path invalidation keeps DB load low without
	// noticeable staleness.
	templateListCache = cache.New(templateQueryCacheTTL(), templateQueryCacheTTL())
	// templateInfoCache caches GetTemplateInfo results keyed by templateID.
	templateInfoCache         = cache.New(templateQueryCacheTTL(), templateQueryCacheTTL())
	templateRequestFetchGroup = &templateFetchGroup{calls: make(map[string]*templateFetchCall)}
	templateRequestLockGroup  = &templateLockGroup{}
)

const templateListCacheKey = "all"

// templateQueryCacheTTL is the TTL for the ListTemplates / GetTemplateInfo
// query caches. Short on purpose: template metadata updates are rare but the
// console polls the list endpoint frequently, so 1 minute cuts DB load by
// ~95% while staying effectively fresh for operators.
func templateQueryCacheTTL() time.Duration {
	return 1 * time.Minute
}

// templateCacheTTL returns the configured template cache TTL, falling back to
// the historical 6-hour default when unset or the config is not loaded yet.
func templateCacheTTL() time.Duration {
	if cfg := config.GetConfig(); cfg != nil && cfg.Common != nil && cfg.Common.TemplateCacheTTL > 0 {
		return cfg.Common.TemplateCacheTTL
	}
	return 360 * time.Minute
}

func (g *templateLockGroup) get(templateID string) *sync.RWMutex {
	if templateID == "" {
		return nil
	}
	if v, ok := g.locks.Load(templateID); ok {
		lock, _ := v.(*sync.RWMutex)
		if lock != nil {
			return lock
		}
	}
	lock := &sync.RWMutex{}
	actual, _ := g.locks.LoadOrStore(templateID, lock)
	lock, _ = actual.(*sync.RWMutex)
	return lock
}

func withTemplateReadLock(templateID string, fn func() error) error {
	lock := templateRequestLockGroup.get(templateID)
	if lock == nil {
		return fn()
	}
	lock.RLock()
	defer lock.RUnlock()
	return fn()
}

func withTemplateWriteLock(templateID string, fn func() error) error {
	lock := templateRequestLockGroup.get(templateID)
	if lock == nil {
		return fn()
	}
	lock.Lock()
	defer lock.Unlock()
	return fn()
}

func (g *templateFetchGroup) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
	g.mu.Lock()
	if call, ok := g.calls[key]; ok {
		g.mu.Unlock()
		<-call.done
		return call.val, call.err
	}
	call := &templateFetchCall{done: make(chan struct{})}
	g.calls[key] = call
	g.mu.Unlock()

	call.val, call.err = fn()
	close(call.done)

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	return call.val, call.err
}

func getCachedTemplateRequest(templateID string) (*sandboxtypes.CreateCubeSandboxReq, bool, error) {
	v, ok := templateDefinitionCache.Get(templateID)
	if !ok {
		return nil, false, nil
	}
	req, ok := v.(*sandboxtypes.CreateCubeSandboxReq)
	if !ok || req == nil {
		templateDefinitionCache.Delete(templateID)
		return nil, false, nil
	}
	cloned, err := cloneCreateRequest(req)
	if err != nil {
		templateDefinitionCache.Delete(templateID)
		return nil, false, err
	}
	return cloned, true, nil
}

func setTemplateRequestCache(templateID string, req *sandboxtypes.CreateCubeSandboxReq) error {
	if templateID == "" || req == nil {
		return nil
	}
	cloned, err := cloneCreateRequest(req)
	if err != nil {
		return err
	}
	templateDefinitionCache.Set(templateID, cloned, templateCacheTTL())
	return nil
}

func getCachedTemplateLocality(templateID string) ([]ReplicaStatus, bool) {
	v, ok := templateLocalityReadyCache.Get(templateID)
	if !ok {
		return nil, false
	}
	snapshot, ok := v.(*templateLocalitySnapshot)
	if !ok || snapshot == nil {
		templateLocalityReadyCache.Delete(templateID)
		return nil, false
	}
	out := make([]ReplicaStatus, len(snapshot.ReadyReplicas))
	copy(out, snapshot.ReadyReplicas)
	return out, true
}

func setTemplateLocalityCache(templateID string, replicas []ReplicaStatus) {
	if templateID == "" {
		return
	}
	ready := make([]ReplicaStatus, 0, len(replicas))
	for _, replica := range replicas {
		if !isReplicaSchedulable(replica) {
			continue
		}
		ready = append(ready, replica)
	}
	templateLocalityReadyCache.Set(templateID, &templateLocalitySnapshot{ReadyReplicas: ready}, templateCacheTTL())
}

func evictReplicaFromLocalityCache(templateID, nodeID string) {
	if templateID == "" || nodeID == "" {
		return
	}
	replicas, ok := getCachedTemplateLocality(templateID)
	if !ok {
		return
	}
	next := make([]ReplicaStatus, 0, len(replicas))
	for _, replica := range replicas {
		if replica.NodeID == nodeID {
			continue
		}
		next = append(next, replica)
	}
	if len(next) == 0 {
		templateLocalityReadyCache.Delete(templateID)
		return
	}
	templateLocalityReadyCache.Set(templateID, &templateLocalitySnapshot{ReadyReplicas: next}, templateCacheTTL())
}

func invalidateTemplateCaches(templateID string) {
	if templateID == "" {
		return
	}
	templateDefinitionCache.Delete(templateID)
	templateLocalityReadyCache.Delete(templateID)
	templateKindCache.Delete(templateID)
	templateInfoCache.Delete(templateID)
	// Any single-template write can change the aggregate list (status, replica
	// counts), so the list cache is dropped alongside the per-template one.
	templateListCache.Delete(templateListCacheKey)
	localcache.InvalidateImageState(templateID)
}

// invalidateTemplateListCache drops the aggregate list cache only. Used by
// write paths that do not have a specific templateID in scope but still
// mutate the list (e.g. GC deleting orphaned definitions).
func invalidateTemplateListCache() {
	templateListCache.Delete(templateListCacheKey)
}

// cloneTemplateInfo deep-copies a TemplateInfo so cached entries cannot be
// mutated by callers (the Replicas slice is shared memory otherwise).
func cloneTemplateInfo(in *TemplateInfo) *TemplateInfo {
	if in == nil {
		return nil
	}
	out := *in
	if in.Replicas != nil {
		out.Replicas = make([]ReplicaStatus, len(in.Replicas))
		copy(out.Replicas, in.Replicas)
	}
	return &out
}

func getCachedTemplateList() ([]TemplateInfo, bool) {
	v, ok := templateListCache.Get(templateListCacheKey)
	if !ok {
		return nil, false
	}
	infos, ok := v.([]TemplateInfo)
	if !ok {
		templateListCache.Delete(templateListCacheKey)
		return nil, false
	}
	out := make([]TemplateInfo, len(infos))
	copy(out, infos)
	return out, true
}

func setTemplateListCache(infos []TemplateInfo) {
	copied := make([]TemplateInfo, len(infos))
	copy(copied, infos)
	templateListCache.Set(templateListCacheKey, copied, templateQueryCacheTTL())
}

func getCachedTemplateInfo(templateID string) (*TemplateInfo, bool) {
	v, ok := templateInfoCache.Get(templateID)
	if !ok {
		return nil, false
	}
	info, ok := v.(*TemplateInfo)
	if !ok || info == nil {
		templateInfoCache.Delete(templateID)
		return nil, false
	}
	return cloneTemplateInfo(info), true
}

func setTemplateInfoCache(templateID string, info *TemplateInfo) {
	if templateID == "" || info == nil {
		return
	}
	templateInfoCache.Set(templateID, cloneTemplateInfo(info), templateQueryCacheTTL())
}

// startTemplateQueryCacheRefresh periodically re-warms the template list and
// per-template info caches from the DB. Write paths already invalidate
// actively; this goroutine is the passive backstop so a missed invalidation
// (or an out-of-band DB change, e.g. another Master replica's write that this
// process did not observe) cannot leave stale data cached indefinitely.
//
// Runs at half the cache TTL so at most one refresh interval of staleness is
// possible even if every write-path invalidation is somehow skipped.
func startTemplateQueryCacheRefresh(ctx context.Context) {
	go func() {
		interval := templateQueryCacheTTL() / 2
		if interval < 5*time.Second {
			interval = 5 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshTemplateQueryCaches(ctx)
			}
		}
	}()
}

// refreshTemplateQueryCaches re-loads the list and re-warms every cached
// templateID's info entry. Errors are logged but never fatal — the next tick
// retries, and reads fall through to the DB on a cache miss anyway.
func refreshTemplateQueryCaches(ctx context.Context) {
	if !isReady() {
		return
	}
	// Re-load the full list by calling the underlying query directly (bypass
	// the cache read in ListTemplates to avoid a self-hit), then re-cache it.
	infos, err := listTemplatesFromDB(ctx)
	if err != nil {
		log.G(ctx).Warnf("template query cache refresh: list templates fail: %v", err)
		return
	}
	setTemplateListCache(infos)
	// Re-warm per-template entries that are currently cached, so their TTL
	// restarts and their content is up to date.
	for _, info := range infos {
		if _, ok := templateInfoCache.Get(info.TemplateID); !ok {
			continue
		}
		full, err := getTemplateInfoFromDB(ctx, info.TemplateID)
		if err != nil {
			// Template may have been deleted by a peer replica; drop the stale entry.
			templateInfoCache.Delete(info.TemplateID)
			continue
		}
		setTemplateInfoCache(info.TemplateID, full)
	}
}

// getCachedTemplateKind returns the cached kind for a templateID.
// The second return value reports whether the entry was present.
func getCachedTemplateKind(templateID string) (string, bool) {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return "", false
	}
	v, ok := templateKindCache.Get(templateID)
	if !ok {
		return "", false
	}
	kind, ok := v.(string)
	if !ok {
		templateKindCache.Delete(templateID)
		return "", false
	}
	return kind, true
}

func setTemplateKindCache(templateID, kind string) {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return
	}
	templateKindCache.Set(templateID, strings.TrimSpace(kind), templateCacheTTL())
}

func registerReadyTemplateReplicas(templateID string, replicas []ReplicaStatus) {
	for _, replica := range replicas {
		if !isReplicaSchedulable(replica) || replica.NodeID == "" {
			continue
		}
		localcache.RegisterTemplateReplica(templateID, replica.NodeID, 1)
	}
}

func reportTemplateMetric(ctx context.Context, callee, endpoint, calleeAction string, cost time.Duration, code int64) {
	log.ReportExt(ctx, callee, endpoint, "Create", calleeAction, cost, code)
}

func reportTemplateCacheMetric(ctx context.Context, calleeAction string, cost time.Duration) {
	reportTemplateMetric(ctx, constants.CubeMasterTemplateID, constants.CubeMasterTemplateID, calleeAction, cost, 0)
}

func ReportResolveMetric(ctx context.Context, cost time.Duration) {
	reportTemplateMetric(ctx, constants.CubeMasterTemplateID, constants.CubeMasterTemplateID, constants.ActionTemplateResolve, cost, 0)
}

// ReportResolveStageMetric emits a per-stage trace for the four sub-phases of
// dealCubeboxCreateReqWithTemplateCenter (request / locality / kind / bind).
// It re-uses the same Callee/Action shape as ReportResolveMetric so the
// existing log.ReportExt sink handles it without additional config.
func ReportResolveStageMetric(ctx context.Context, action string, cost time.Duration) {
	reportTemplateMetric(ctx, constants.CubeMasterTemplateID, constants.CubeMasterTemplateID, action, cost, 0)
}
