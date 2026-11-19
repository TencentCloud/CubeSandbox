// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package remotestatus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

// A node that was down while Master consumed one of its xfs-s3 pause
// packages (the sandbox resumed elsewhere from S3) keeps that package and the
// sandbox's PAUSED tombstone: the cleanup that Resume sends to the origin
// could not reach it. Once the node answers again this loop reconciles it
// against Master's own records:
//
//   - a local xfs-s3 pause package that no pause binding names any more is
//     handed to the ordinary pause cleanup. Cubelet's keep gate still holds a
//     package a live sandbox maps, so the loop cannot pull memory out from
//     under a running guest;
//   - a PAUSED xfs-s3 tombstone whose sandbox has no pause binding and runs
//     on another node is dropped there, and so is a PAUSED tombstone of a
//     sandbox Master no longer has a spec for (deleted while the node was
//     away).
//
// Both only act on facts that are settled: anything younger than the grace
// period, or any record Master cannot read, is left for the next round.

const (
	orphanInterval = 2 * time.Minute
	orphanGrace    = 10 * time.Minute
)

var (
	listNodeIPs          = listVerifierNodes
	listLocalSnapshots   = cubelet.ListLocalSnapshots
	listNodeSandboxes    = cubelet.List
	pauseBySnapshot      = pausesnap.GetBySnapshotID
	pauseBySandbox       = pausesnap.GetBySandbox
	cleanupPausePackage  = pausesnap.CleanupPauseSnapshot
	dropPausedTombstone  = pausesnap.DropPausedTombstone
	sandboxBackend       = defaultSandboxBackend
	sandboxRunningHostIP = defaultSandboxRunningHost
	nowFn                = time.Now
)

func defaultSandboxBackend(ctx context.Context, sandboxID string) (string, error) {
	spec, err := sandboxspec.Get(ctx, sandboxID)
	if err != nil {
		return "", err
	}
	return spec.Backend, nil
}

// defaultSandboxRunningHost returns where the routing table says the sandbox
// runs, provided that node confirms it is RUNNING there.
func defaultSandboxRunningHost(ctx context.Context, sandboxID string) string {
	pm, ok := localcache.GetSandboxProxyMap(ctx, sandboxID)
	if !ok || pm == nil || strings.TrimSpace(pm.HostIP) == "" {
		return ""
	}
	host := strings.TrimSpace(pm.HostIP)
	rsp, err := listNodeSandboxes(ctx, cubeletAddr(host), &cubebox.ListCubeSandboxRequest{Id: &sandboxID})
	if err != nil || rsp == nil {
		return ""
	}
	for _, item := range rsp.GetItems() {
		for _, c := range item.GetContainers() {
			if c.GetId() == sandboxID && c.GetState() == cubebox.ContainerState_CONTAINER_RUNNING {
				return host
			}
		}
	}
	return ""
}

func startOrphanReconciler(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(orphanInterval):
			}
			ReconcileXFSS3Orphans(ctx)
		}
	}()
}

// ReconcileXFSS3Orphans runs one round over every healthy node.
func ReconcileXFSS3Orphans(ctx context.Context) {
	for _, ip := range listNodeIPs() {
		if ctx.Err() != nil {
			return
		}
		reconcileOrphanPackages(ctx, ip)
		reconcileOrphanTombstones(ctx, ip)
	}
}

func reconcileOrphanPackages(ctx context.Context, ip string) {
	rsp, err := listLocalSnapshots(ctx, cubeletAddr(ip), &cubebox.ListLocalSnapshotsRequest{
		RequestID: uuid.NewString(),
		Backend:   constants.SnapshotBackendXFS,
	})
	if err == nil && rsp == nil {
		err = errors.New("empty response")
	}
	if err == nil && rsp.GetRet() != nil && rsp.GetRet().GetRetCode() != cubeleterrorcode.ErrorCode_Success {
		err = fmt.Errorf("ret %v: %s", rsp.GetRet().GetRetCode(), rsp.GetRet().GetRetMsg())
	}
	if err != nil {
		// A node whose packages cannot be listed is skipped this round;
		// saying so keeps a node that never answers from going unnoticed.
		log.G(ctx).Warnf("xfs-s3 orphan: list local packages on %s failed, skipping this round: %v", ip, err)
		return
	}
	for _, s := range rsp.GetSnapshots() {
		if s.GetBackend() != constants.SnapshotBackendXFSS3 || s.GetKind() != "pause_snapshot" {
			continue
		}
		snapID := strings.TrimSpace(s.GetSnapshotID())
		created, err := time.Parse(time.RFC3339, strings.TrimSpace(s.GetCreatedAt()))
		if snapID == "" || err != nil || nowFn().Sub(created) < orphanGrace {
			continue
		}
		if _, err := pauseBySnapshot(ctx, snapID); !errors.Is(err, pausesnap.ErrNotFound) {
			// Still bound, or the binding cannot be read: not an orphan.
			continue
		}
		if err := cleanupPausePackage(ctx, uuid.NewString(), ip, snapID, constants.SnapshotBackendXFSS3); err != nil {
			log.G(ctx).Warnf("xfs-s3 orphan: cleanup of package %s on %s: %v", snapID, ip, err)
			continue
		}
		log.G(ctx).Infof("xfs-s3 orphan: package %s on %s has no pause binding; cleaned (a live sandbox keeps it mapped)", snapID, ip)
	}
}

func reconcileOrphanTombstones(ctx context.Context, ip string) {
	rsp, err := listNodeSandboxes(ctx, cubeletAddr(ip), &cubebox.ListCubeSandboxRequest{})
	if err == nil && rsp == nil {
		err = errors.New("empty response")
	}
	if err != nil {
		log.G(ctx).Warnf("xfs-s3 orphan: list sandboxes on %s failed, skipping this round: %v", ip, err)
		return
	}
	for _, item := range rsp.GetItems() {
		sandboxID := strings.TrimSpace(item.GetId())
		if sandboxID == "" || !pausedTombstone(item, sandboxID) {
			continue
		}
		if _, err := pauseBySandbox(ctx, sandboxID); !errors.Is(err, pausesnap.ErrNotFound) {
			continue
		}
		backend, err := sandboxBackend(ctx, sandboxID)
		if errors.Is(err, sandboxspec.ErrSandboxSpecNotFound) {
			// Master no longer knows the sandbox at all: it was deleted
			// while this node was unreachable. Nothing can resume from the
			// tombstone any more.
			if err := dropPausedTombstone(ctx, uuid.NewString(), sandboxID, ip); err != nil {
				log.G(ctx).Warnf("xfs-s3 orphan: drop PAUSED tombstone of deleted sandbox %s on %s: %v", sandboxID, ip, err)
			} else {
				log.G(ctx).Infof("xfs-s3 orphan: dropped PAUSED tombstone of deleted sandbox %s on %s", sandboxID, ip)
			}
			continue
		}
		if err != nil || !constants.IsXFSS3Backend(backend) {
			continue
		}
		host := sandboxRunningHostIP(ctx, sandboxID)
		if host == "" || host == ip {
			continue
		}
		if err := dropPausedTombstone(ctx, uuid.NewString(), sandboxID, ip); err != nil {
			log.G(ctx).Warnf("xfs-s3 orphan: drop PAUSED tombstone of %s on %s: %v", sandboxID, ip, err)
			continue
		}
		log.G(ctx).Infof("xfs-s3 orphan: dropped PAUSED tombstone of %s on %s; it runs on %s", sandboxID, ip, host)
	}
}

func pausedTombstone(item *cubebox.CubeSandbox, sandboxID string) bool {
	at := item.GetPausedAt()
	if at == 0 {
		at = item.GetCreatedAt()
	}
	if nowFn().Sub(time.Unix(0, at)) < orphanGrace {
		return false
	}
	for _, c := range item.GetContainers() {
		if c.GetId() == sandboxID {
			return c.GetState() == cubebox.ContainerState_CONTAINER_PAUSED
		}
	}
	return false
}
