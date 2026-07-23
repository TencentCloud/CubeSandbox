// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package lifecycle

import (
	"context"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/task"
)

// Init wires the lifecycle metadata channel into the sandbox create/destroy
// hooks. Call exactly once at process start, after wrapredis is reachable.
//
// Failures here are intentionally non-fatal: lifecycle metadata is an
// observability/coordination side channel for the auto-pause sidecar; if it
// is missing the rest of CubeMaster keeps working and sandboxes still serve
// traffic. Callers (main.go) should log a warning and proceed.
//
// We use the single shared wrapredis pool. The sidecar consumes lifecycle
// metadata and the sandbox proxy map (cube:v1:shared:sandbox:proxy) from the
// same Redis instance, so any pool that can write proxy entries can also write
// lifecycle entries.
func Init(ctx context.Context) error {
	pool := wrapredis.GetRedis()
	if isNilPool(pool) {
		log.G(ctx).Warnf("lifecycle: redis pool unavailable; auto-pause metadata channel disabled")
		return nil
	}

	store := NewStore(pool)
	setDefaultStore(store)

	sandbox.RegisterAfterCreateSandboxSuccessHook(onAfterCreate)
	// Both the synchronous destroy path (sandbox_remove.callCubelet) and the
	// asynchronous task executor end with their own success hook. Register on
	// both so we publish exactly once for either deletion mode.
	sandbox.RegisterAfterDestroySandboxSuccessHook(onAfterDestroy)
	task.RegisterAfterDestroyTaskSuccessHook(onAfterDestroy)

	sandbox.RegisterAfterUpdateSandboxSuccessHook(onAfterUpdate)

	sandbox.SetTimeoutProvider(&storeTimeoutProvider{store: store})

	log.G(ctx).Infof("lifecycle: auto-pause metadata channel ready (key=%s, stream=%s)",
		MetaKey, EventStreamKey)
	return nil
}

// storeTimeoutProvider adapts our *Store to sandbox.TimeoutProvider.
type storeTimeoutProvider struct {
	store *Store
}

// RefreshTimeout reads the existing meta (preserving fields the request
// doesn't carry: AutoPause / AutoResume / TemplateID / HostID / HostIP /
// InstanceType), rewrites CreatedAt + TimeoutSeconds + EndAt, and publishes
// an OpUpdate event so every sidecar replica converges on the new view.
func (p *storeTimeoutProvider) RefreshTimeout(ctx context.Context, sandboxID string, timeoutSeconds int) (int64, error) {
	if p == nil || p.store == nil {
		return 0, nil
	}
	meta, err := p.store.LoadMeta(ctx, sandboxID)
	if err != nil {
		return 0, err
	}
	if meta == nil {
		return 0, nil
	}

	now := time.Now().UnixMilli()
	ts := timeoutSeconds
	meta.TimeoutSeconds = &ts
	meta.CreatedAt = now
	meta.EndAt = projectedEndAt(now, timeoutSeconds)
	p.store.PublishUpdate(ctx, meta)
	return meta.EndAt, nil
}

// projectedEndAt maps idle TTL to EndAt (unix ms). See docs/guide/lifecycle.md.
func projectedEndAt(nowMs int64, timeoutSeconds int) int64 {
	if timeoutSeconds < 0 {
		return 0
	}
	return nowMs + int64(timeoutSeconds)*1000
}

// LookupEndAt reads the latest meta.EndAt straight from the lifecycle snapshot in Redis.
func (p *storeTimeoutProvider) LookupEndAt(ctx context.Context, sandboxID string) (int64, error) {
	if p == nil || p.store == nil {
		return 0, nil
	}
	meta, err := p.store.LoadMeta(ctx, sandboxID)
	if err != nil {
		return 0, err
	}
	if meta == nil {
		return 0, nil
	}
	if meta.EndAt > 0 {
		return meta.EndAt, nil
	}
	if meta.CreatedAt > 0 && meta.TimeoutSeconds != nil && *meta.TimeoutSeconds > 0 {
		return meta.CreatedAt + int64(*meta.TimeoutSeconds)*1000, nil
	}
	return 0, nil
}

// isNilPool guards against wrapredis.GetRedis returning a typed-nil
// (*RedisWrap)(nil) — that satisfies a nil interface check via != nil but
// is functionally unusable. We unwrap by inspecting the concrete pool.
func isNilPool(w *wrapredis.RedisWrap) bool {
	return w == nil || w.RedisConnPool == nil
}

func onAfterCreate(ctx context.Context, sandboxID, hostID, hostIP string, req *sandboxtypes.CreateCubeSandboxReq) error {
	store := getDefaultStore()
	if store == nil || req == nil {
		return nil
	}
	now := time.Now().UnixMilli()
	// ConstructCubeletReq normalizes req.Timeout before create; guard nil defensively.
	timeoutSeconds := sandboxtypes.NeverTimeout
	if req.Timeout != nil {
		timeoutSeconds = *req.Timeout
	}
	ts := timeoutSeconds
	meta := &SandboxLifecycleMeta{
		SandboxID:      sandboxID,
		HostID:         hostID,
		HostIP:         hostIP,
		InstanceType:   req.InstanceType,
		TimeoutSeconds: &ts,
		AutoPause:      req.AutoPause,
		AutoResume:     req.AutoResume,
		CreatedAt:      now,
		EndAt:          projectedEndAt(now, timeoutSeconds),
	}
	if req.Annotations != nil {
		// Template ID is conventionally carried via annotations from CubeAPI;
		// the field is informational so we tolerate it being absent.
		if v, ok := req.Annotations["template_id"]; ok {
			meta.TemplateID = v
		}
	}
	store.PublishCreate(ctx, meta)
	return nil
}

func onAfterDestroy(ctx context.Context, sandboxID string) error {
	if store := getDefaultStore(); store != nil {
		store.PublishDelete(ctx, sandboxID)
	}
	return nil
}

// PublishStateDefault is the public entry point for callers that don't hold a
// *Store reference to announce a pause / resume transition to the CLM.
// Safe to call when lifecycle is disabled or Redis is unreachable — the
// underlying Store swallows those cases.
func PublishStateDefault(ctx context.Context, sandboxID, state, source string) {
	if store := getDefaultStore(); store != nil {
		store.PublishState(ctx, sandboxID, state, source)
	}
}

// onAfterUpdate maps a successful pause / resume action to the corresponding
// terminal state and forwards it to the CLM via the events stream.
// Registered with sandbox.RegisterAfterUpdateSandboxSuccessHook in Init.
//
// Unknown actions are ignored (the update handler already validates that
// action ∈ {"pause","resume"}; this is defence-in-depth against future
// action codes reaching the hook chain without a schema update here).
func onAfterUpdate(ctx context.Context, sandboxID, _ /*instanceType*/, action, _ /*requestID*/ string) {
	var state string
	switch action {
	case "pause":
		state = StatePaused
	case "resume":
		state = StateRunning
	default:
		return
	}
	PublishStateDefault(ctx, sandboxID, state, "api")
}
