// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	defaultReapInterval = 5 * time.Second
	maxConcurrentReaps  = 32
	// reapBatchLimit caps how many expired sandboxes a single tick will
	// destroy. Anything beyond this rolls over to the next tick, which
	// keeps a single replica from holding the cluster-wide reaper lease
	// for an unbounded time when many sandboxes expire simultaneously.
	reapBatchLimit = 256
	// reaperLockTTL bounds how long a crashed leader can stall the
	// reaper. The active leader refreshes the lease via SET NX EX every
	// reapInterval, so any leader that is alive will keep extending it.
	reaperLockTTL = 30 * time.Second
	// reaperShutdownTimeout caps how long the reaper waits for in-flight
	// destroy goroutines to drain on shutdown. The cubemaster process
	// must not block forever just because Cubelet is unresponsive; if
	// the deadline fires we log and exit, leaving the in-flight ids in
	// the TTL index so the next-elected leader picks them up.
	reaperShutdownTimeout = 15 * time.Second
)

var (
	reaperStarted atomic.Bool
	// reaperHolder uniquely identifies this CubeMaster process inside the
	// reaper-lock CAS so we never accidentally release another replica's
	// lease, even after a long GC pause that lets the lease expire.
	reaperHolder = uuid.New().String()
)

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

	// destroyWG tracks every destroy goroutine spawned by reapOnce so
	// graceful shutdown can wait for them. Without this WG a SIGTERM
	// would orphan in-flight DestroySandbox RPCs, leaving the local
	// task pipeline mid-flight and inflating the "ghost sandbox"
	// rate after restart.
	var destroyWG sync.WaitGroup
	for {
		select {
		case <-ctx.Done():
			log.G(ctx).Infof("sandbox TTL reaper stopping: %v, waiting for in-flight destroys (timeout=%s)",
				ctx.Err(), reaperShutdownTimeout)
			waitWithTimeout(&destroyWG, reaperShutdownTimeout, func(drained bool) {
				if drained {
					log.G(ctx).Infof("sandbox TTL reaper stopped cleanly")
				} else {
					log.G(ctx).Warnf("sandbox TTL reaper shutdown timeout: leftover destroys may resume on next leader")
				}
			})
			return
		case <-ticker.C:
			recov.WithRecover(func() { reapOnce(ctx, &destroyWG) }, func(p interface{}) {
				log.G(ctx).Errorf("sandbox TTL reaper panic recovered: %v", p)
			})
		}
	}
}

// waitWithTimeout blocks until either wg.Done() drained or the timeout
// fires, then invokes report so the caller can emit a single log line
// without duplicating the timer plumbing.
func waitWithTimeout(wg *sync.WaitGroup, timeout time.Duration, report func(drained bool)) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		report(true)
	case <-time.After(timeout):
		report(false)
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

func reapOnce(ctx context.Context, destroyWG *sync.WaitGroup) {
	if config.GetConfig().Common.MockUpdateAction {
		return
	}
	ctx, _ = withReaperTrace(ctx, "SandboxReaperScan", "")

	// In a multi-master HA deployment every replica boots its own reaper
	// goroutine. Without coordination they would all enumerate the same
	// expired set and race to DestroySandbox, producing duplicate
	// requests and a flood of "sandbox not found" errors against
	// CubeMaster / Cubelet. The Redis SET NX EX lease elects a single
	// active reaper for `reaperLockTTL`; the other replicas simply skip
	// this tick.
	locked, err := localcache.AcquireReaperLock(ctx, reaperHolder, reaperLockTTL)
	if err != nil {
		log.G(ctx).Warnf("sandbox TTL reaper: acquire lease failed: %v", err)
		return
	}
	if !locked {
		return
	}
	defer localcache.ReleaseReaperLock(ctx, reaperHolder)

	expired, err := localcache.ListExpiredSandboxIDs(ctx, time.Now(), reapBatchLimit)
	if err != nil {
		log.G(ctx).Warnf("sandbox TTL reaper: list expired failed: %v", err)
		return
	}
	if len(expired) == 0 {
		return
	}
	log.G(ctx).Infof("sandbox TTL reaper: %d expired sandbox(es) to destroy (cap=%d)",
		len(expired), reapBatchLimit)

	sem := make(chan struct{}, maxConcurrentReaps)
	// localWG bounds the *current tick* so we know when this batch is
	// fully drained; destroyWG is the process-level barrier the runner
	// goroutine uses on shutdown. We add to both: localWG so the dispatch
	// loop can return at end-of-tick without ditching live work, and
	// destroyWG so a SIGTERM in the middle of a tick still has something
	// to wait on.
	var localWG sync.WaitGroup
	for _, sandboxID := range expired {
		select {
		case <-ctx.Done():
			goto drain
		case sem <- struct{}{}:
		}
		id := sandboxID
		localWG.Add(1)
		destroyWG.Add(1)
		go func() {
			defer func() {
				<-sem
				localWG.Done()
				destroyWG.Done()
			}()
			recov.WithRecover(func() { destroyExpiredSandbox(ctx, id) }, func(p interface{}) {
				log.G(ctx).Errorf("sandbox TTL reaper destroy panic %s: %v", id, p)
			})
		}()
	}
drain:
	localWG.Wait()
}

// destroyExpiredSandbox issues a synchronous DestroySandbox for one
// sandbox whose TTL has elapsed and only clears the proxy/index entries
// after the destroy itself reports success.
//
// The earlier version cleared EndAt *before* calling DestroySandbox in
// async mode, which had two failure modes:
//  1. async destroy fails silently → sandbox is leaked, never retried,
//     because the TTL secondary index has already lost the member.
//  2. SetTimeout extends the TTL between scan-tick and destroy-tick →
//     the EndAt write race-killed a freshly-extended sandbox.
//
// Sync: true + post-success cleanup makes both failure modes self-heal
// on the next reaper tick.
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

	// Synchronous destroy: we must observe success before we drop EndAt
	// or the secondary index, otherwise a transient Cubelet failure
	// would leak the sandbox forever. DestroySandbox returns rsp.Ret
	// shaped exactly like the HTTP path; a non-Success ret_code just
	// means we'll retry on the next tick because EndAt is still set.
	rsp := DestroySandbox(ctx, &types.DeleteCubeSandboxReq{
		RequestID:    requestID,
		SandboxID:    sandboxID,
		InstanceType: cubebox.InstanceType_cubebox.String(),
		Sync:         true,
	})
	if rsp == nil || rsp.Ret == nil {
		log.G(ctx).Warnf("reaper destroy sandbox=%s returned nil response (will retry)", sandboxID)
		return
	}
	if rsp.Ret.RetCode != int(errorcode.ErrorCode_Success) {
		log.G(ctx).Warnf("reaper destroy sandbox=%s ret_code=%d ret_msg=%s (will retry next tick)",
			sandboxID, rsp.Ret.RetCode, rsp.Ret.RetMsg)
		return
	}
	log.G(ctx).Infof("reaper destroy sandbox=%s ret_code=%d ret_msg=%s",
		sandboxID, rsp.Ret.RetCode, rsp.Ret.RetMsg)
}
