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
// metadata and the bypass_host_proxy map from the same Redis instance, so
// any pool that can write proxy entries can also write lifecycle entries.
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

	log.G(ctx).Infof("lifecycle: auto-pause metadata channel ready (key=%s, stream=%s)",
		MetaKey, EventStreamKey)
	return nil
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
	meta := &SandboxLifecycleMeta{
		SandboxID:      sandboxID,
		HostID:         hostID,
		HostIP:         hostIP,
		InstanceType:   req.InstanceType,
		TimeoutSeconds: req.Timeout,
		AutoPause:      req.AutoPause,
		AutoResume:     req.AutoResume,
		CreatedAt:      time.Now().UnixMilli(),
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
