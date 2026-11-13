// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
)

var reserveSnapshotCreateFn = templatecenter.ReserveSnapshotCreate
var detachSnapshotCreateRefFn = templatecenter.DetachSnapshotRuntimeBinding

func reserveCreatedSandboxRuntimeRef(ctx context.Context, req *types.CreateCubeSandboxReq) error {
	snapshotID := strings.TrimSpace(req.Annotations[constants.CubeAnnotationRuntimeSnapshotID])
	if snapshotID == "" {
		return nil
	}
	if len(req.DistributionScope) != 1 {
		return fmt.Errorf("snapshot %s create requires one selected node", snapshotID)
	}
	nodeID, nodeIP := req.DistributionScope[0], ""
	if net.ParseIP(nodeID) != nil {
		nodeIP, nodeID = nodeID, ""
	}
	if resolved := templateResolveResultFromContext(ctx); resolved != nil && resolved.HasChosenReplica {
		nodeID, nodeIP = resolved.ChosenReplica.NodeID, resolved.ChosenReplica.NodeIP
	}
	sandboxID := strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := reserveSnapshotCreateFn(ctx, snapshotID, sandboxID, nodeID, nodeIP); err != nil {
		return err
	}
	req.Annotations[constants.CubeAnnotationDesiredSandboxID] = sandboxID
	return nil
}

// Only a sealed, undispatched attempt is safe to release immediately. A node
// error or timeout can still leave a running VM and must retain its durable pin.
func releaseUndispatchedSnapshotCreate(ctx context.Context, req *types.CreateCubeSandboxReq, rsp *types.CreateCubeSandboxRes) {
	if rsp == nil || !rsp.CreateNotStarted || rsp.Ret == nil || rsp.Ret.RetCode == int(errorcode.ErrorCode_Success) {
		return
	}
	sandboxID := req.Annotations[constants.CubeAnnotationDesiredSandboxID]
	if sandboxID == "" || req.Annotations[constants.CubeAnnotationRuntimeSnapshotID] == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := detachSnapshotCreateRefFn(cleanupCtx, sandboxID, templatecenter.SnapshotRuntimeBindingCreatePending, "create rejected before node dispatch"); err != nil {
		log.G(ctx).Warnf("release undispatched snapshot create reservation sandbox=%s: %v", sandboxID, err)
	}
}
