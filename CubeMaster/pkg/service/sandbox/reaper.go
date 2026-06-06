// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	defaultReapInterval = 5 * time.Second
	maxConcurrentReaps  = 32
)

var reaperStarted atomic.Bool

// StartTimeoutReaper launches the background goroutine that destroys
// sandboxes whose EndAt deadline has passed.
func StartTimeoutReaper(ctx context.Context) {
	if !reaperStarted.CompareAndSwap(false, true) {
		log.G(ctx).Warnf("sandbox TTL reaper already started, ignoring duplicate call")
		return
	}
	go runTimeoutReaper(ctx, defaultReapInterval)
}

func runTimeoutReaper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultReapInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.G(ctx).Infof("sandbox TTL reaper started, interval=%s, max_concurrent=%d",
		interval, maxConcurrentReaps)
	for {
		select {
		case <-ctx.Done():
			log.G(ctx).Infof("sandbox TTL reaper stopped: %v", ctx.Err())
			return
		case <-ticker.C:
			recov.WithRecover(func() { reapOnce(ctx) }, func(p interface{}) {
				log.G(ctx).Errorf("sandbox TTL reaper panic recovered: %v", p)
			})
		}
	}
}

func withReaperTrace(ctx context.Context, action, sandboxID string) (context.Context, string) {
	requestID := uuid.New().String()
	rt := &CubeLog.RequestTrace{
		RequestID:    requestID,
		Action:       action,
		Caller:       "cubemaster.reaper",
		Callee:       "cubemaster",
		InstanceID:   sandboxID,
		InstanceType: cubebox.InstanceType_cubebox.String(),
		Timestamp:    time.Now(),
	}
	return CubeLog.WithRequestTrace(ctx, rt), requestID
}

func reapOnce(ctx context.Context) {
	if config.GetConfig().Common.MockUpdateAction {
		return
	}
	ctx, _ = withReaperTrace(ctx, "SandboxReaperScan", "")

	expired, err := localcache.ListExpiredSandboxIDs(ctx, time.Now())
	if err != nil {
		log.G(ctx).Warnf("sandbox TTL reaper: list expired failed: %v", err)
		return
	}
	if len(expired) == 0 {
		return
	}
	log.G(ctx).Infof("sandbox TTL reaper: %d expired sandbox(es) to destroy", len(expired))
	sem := make(chan struct{}, maxConcurrentReaps)
	for _, sandboxID := range expired {
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		id := sandboxID
		go func() {
			defer func() { <-sem }()
			recov.WithRecover(func() { destroyExpiredSandbox(ctx, id) }, func(p interface{}) {
				log.G(ctx).Errorf("sandbox TTL reaper destroy panic %s: %v", id, p)
			})
		}()
	}
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}
}

func destroyExpiredSandbox(ctx context.Context, sandboxID string) {
	ctx, requestID := withReaperTrace(ctx, "SandboxReaperDestroy", sandboxID)
	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(map[string]interface{}{
		"RequestId":    requestID,
		"InstanceId":   sandboxID,
		"InstanceType": cubebox.InstanceType_cubebox.String(),
		"Caller":       "reaper",
	}))

	proxy, ok := localcache.GetSandboxProxyMap(ctx, sandboxID)
	if !ok || proxy == nil {
		log.G(ctx).Infof("reaper: sandbox=%s already gone, skip", sandboxID)
		return
	}
	if proxy.EndAt == "" {
		log.G(ctx).Infof("reaper: sandbox=%s EndAt cleared, skip", sandboxID)
		return
	}
	endNanos, err := strconv.ParseInt(proxy.EndAt, 10, 64)
	if err != nil || endNanos <= 0 {
		log.G(ctx).Warnf("reaper: sandbox=%s malformed EndAt=%q, skip", sandboxID, proxy.EndAt)
		return
	}
	if time.Now().UnixNano() <= endNanos {
		log.G(ctx).Infof("reaper: sandbox=%s EndAt was extended to %s, skip",
			sandboxID, time.Unix(0, endNanos).UTC().Format(time.RFC3339Nano))
		return
	}

	proxy.EndAt = ""
	if err := localcache.SetSandboxProxyMap(ctx, proxy); err != nil {
		log.G(ctx).Warnf("reaper clear EndAt sandbox=%s failed: %v", sandboxID, err)
	}

	rsp := DestroySandbox(ctx, &types.DeleteCubeSandboxReq{
		RequestID:    requestID,
		SandboxID:    sandboxID,
		InstanceType: cubebox.InstanceType_cubebox.String(),
		Sync:         false,
	})
	if rsp == nil || rsp.Ret == nil {
		log.G(ctx).Warnf("reaper destroy sandbox=%s returned nil response", sandboxID)
		return
	}
	log.G(ctx).Infof("reaper destroy sandbox=%s ret_code=%d ret_msg=%s",
		sandboxID, rsp.Ret.RetCode, rsp.Ret.RetMsg)
}
