// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

const (
	templateDefinitionCacheTTL = 360 * time.Minute
	templateLocalityCacheTTL   = 360 * time.Minute
	// Per-process only; multi-replica deployments may serve stale hits until TTL
	// or until this replica handles a mutating path (see invalidateTemplateDisplayNameCache).
	templateDisplayNameCacheTTL            = 300 * time.Second
	templateDisplayNameCacheMaxLen         = 4096
	templateDisplayNameNotFoundCacheTTL    = 30 * time.Second
	templateDisplayNameNotFoundCacheMaxLen = 1024
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
	templateDefinitionCache    = cache.New(templateDefinitionCacheTTL, templateDefinitionCacheTTL)
	templateLocalityReadyCache = cache.New(templateLocalityCacheTTL, templateLocalityCacheTTL)
	// templateKindCache caches the template kind ("snapshot"|"app_snapshot"|...)
	// keyed by templateID. The kind is derived from a single column in
	// t_cube_template_definition, so its only mutation source is the same
	// definition write paths that already call invalidateTemplateCaches.
	templateKindCache                = cache.New(templateDefinitionCacheTTL, templateDefinitionCacheTTL)
	templateDisplayNameCache         = cache.New(templateDisplayNameCacheTTL, templateDisplayNameCacheTTL)
	templateDisplayNameNotFoundCache = cache.New(templateDisplayNameNotFoundCacheTTL, templateDisplayNameNotFoundCacheTTL)
	templateRequestFetchGroup        = &templateFetchGroup{calls: make(map[string]*templateFetchCall)}
	templateDisplayNameFetchGroup    = &templateFetchGroup{calls: make(map[string]*templateFetchCall)}
	templateRequestLockGroup         = &templateLockGroup{}
	displayNamePositiveFIFO          []string
	displayNameNotFoundFIFO          []string
	displayNameCacheFIFOMu           sync.Mutex
)

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

func (g *templateLockGroup) delete(key string) {
	if key == "" {
		return
	}
	g.locks.Delete(key)
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

	defer func() {
		if r := recover(); r != nil {
			call.err = fmt.Errorf("template fetch panicked: %v", r)
		}
		close(call.done)
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
	}()

	call.val, call.err = fn()
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
	templateDefinitionCache.Set(templateID, cloned, templateDefinitionCacheTTL)
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
	templateLocalityReadyCache.Set(templateID, &templateLocalitySnapshot{ReadyReplicas: ready}, templateLocalityCacheTTL)
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
	templateLocalityReadyCache.Set(templateID, &templateLocalitySnapshot{ReadyReplicas: next}, templateLocalityCacheTTL)
}

func invalidateTemplateCaches(templateID string) {
	if templateID == "" {
		return
	}
	templateDefinitionCache.Delete(templateID)
	templateLocalityReadyCache.Delete(templateID)
	templateKindCache.Delete(templateID)
	localcache.InvalidateImageState(templateID)
}

func displayNameCacheKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func getCachedTemplateIDByDisplayName(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	if _, ok := templateDisplayNameNotFoundCache.Get(key); ok {
		return "", false
	}
	v, ok := templateDisplayNameCache.Get(key)
	if !ok {
		return "", false
	}
	templateID, ok := v.(string)
	if !ok || strings.TrimSpace(templateID) == "" {
		templateDisplayNameCache.Delete(key)
		return "", false
	}
	return templateID, true
}

func isDisplayNameNotFoundCached(key string) bool {
	if key == "" {
		return false
	}
	_, ok := templateDisplayNameNotFoundCache.Get(key)
	return ok
}

func setTemplateDisplayNameNotFoundCache(key string) {
	key = displayNameCacheKey(key)
	if key == "" {
		return
	}
	_, alreadyCached := templateDisplayNameNotFoundCache.Get(key)
	if !alreadyCached && templateDisplayNameNotFoundCache.ItemCount() >= templateDisplayNameNotFoundCacheMaxLen {
		evictOneDisplayNameNotFoundCacheEntry()
	}
	templateDisplayNameNotFoundCache.Set(key, true, templateDisplayNameNotFoundCacheTTL)
	if !alreadyCached {
		appendDisplayNameNotFoundFIFO(key)
	}
}

func setTemplateDisplayNameCache(key, templateID string) {
	key = displayNameCacheKey(key)
	templateID = strings.TrimSpace(templateID)
	if key == "" || templateID == "" {
		return
	}
	templateDisplayNameNotFoundCache.Delete(key)
	removeDisplayNameNotFoundFIFOKey(key)
	_, alreadyCached := templateDisplayNameCache.Get(key)
	if !alreadyCached && templateDisplayNameCache.ItemCount() >= templateDisplayNameCacheMaxLen {
		evictOneDisplayNameCacheEntry()
	}
	templateDisplayNameCache.Set(key, templateID, templateDisplayNameCacheTTL)
	if !alreadyCached {
		appendDisplayNamePositiveFIFO(key)
	}
}

func appendDisplayNamePositiveFIFO(key string) {
	displayNameCacheFIFOMu.Lock()
	displayNamePositiveFIFO = append(displayNamePositiveFIFO, key)
	displayNameCacheFIFOMu.Unlock()
}

func appendDisplayNameNotFoundFIFO(key string) {
	displayNameCacheFIFOMu.Lock()
	displayNameNotFoundFIFO = append(displayNameNotFoundFIFO, key)
	displayNameCacheFIFOMu.Unlock()
}

func removeDisplayNamePositiveFIFOKey(key string) {
	displayNameCacheFIFOMu.Lock()
	displayNamePositiveFIFO = removeFIFOKey(displayNamePositiveFIFO, key)
	displayNameCacheFIFOMu.Unlock()
}

func removeDisplayNameNotFoundFIFOKey(key string) {
	displayNameCacheFIFOMu.Lock()
	displayNameNotFoundFIFO = removeFIFOKey(displayNameNotFoundFIFO, key)
	displayNameCacheFIFOMu.Unlock()
}

func removeFIFOKey(keys []string, key string) []string {
	for i, existing := range keys {
		if existing == key {
			return append(keys[:i], keys[i+1:]...)
		}
	}
	return keys
}

func resetDisplayNameCacheFIFOForTest() {
	displayNameCacheFIFOMu.Lock()
	displayNamePositiveFIFO = nil
	displayNameNotFoundFIFO = nil
	displayNameCacheFIFOMu.Unlock()
}

func evictOneDisplayNameCacheEntry() {
	displayNameCacheFIFOMu.Lock()
	if len(displayNamePositiveFIFO) == 0 {
		displayNameCacheFIFOMu.Unlock()
		for key := range templateDisplayNameCache.Items() {
			templateDisplayNameCache.Delete(key)
			templateRequestLockGroup.delete(displayNameLockKey(key))
			return
		}
		return
	}
	key := displayNamePositiveFIFO[0]
	displayNamePositiveFIFO = displayNamePositiveFIFO[1:]
	displayNameCacheFIFOMu.Unlock()
	templateDisplayNameCache.Delete(key)
	templateRequestLockGroup.delete(displayNameLockKey(key))
}

func evictOneDisplayNameNotFoundCacheEntry() {
	displayNameCacheFIFOMu.Lock()
	if len(displayNameNotFoundFIFO) == 0 {
		displayNameCacheFIFOMu.Unlock()
		for key := range templateDisplayNameNotFoundCache.Items() {
			templateDisplayNameNotFoundCache.Delete(key)
			templateRequestLockGroup.delete(displayNameLockKey(key))
			return
		}
		return
	}
	key := displayNameNotFoundFIFO[0]
	displayNameNotFoundFIFO = displayNameNotFoundFIFO[1:]
	displayNameCacheFIFOMu.Unlock()
	templateDisplayNameNotFoundCache.Delete(key)
	templateRequestLockGroup.delete(displayNameLockKey(key))
}

func invalidateTemplateDisplayNameCache(names ...string) {
	for _, name := range names {
		key := displayNameCacheKey(name)
		if key == "" {
			continue
		}
		templateDisplayNameCache.Delete(key)
		templateDisplayNameNotFoundCache.Delete(key)
		removeDisplayNamePositiveFIFOKey(key)
		removeDisplayNameNotFoundFIFOKey(key)
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
	templateKindCache.Set(templateID, strings.TrimSpace(kind), templateDefinitionCacheTTL)
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
