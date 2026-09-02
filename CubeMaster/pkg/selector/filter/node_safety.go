// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package filter

import (
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

// nodeSafetyFilter repeats the non-negotiable node health checks after any
// backoff candidate expansion. The normal PreFilter already performs these
// checks; keeping them in the custom Profile guard set prevents an explicitly
// configured backoff policy from bypassing them.
//
// The checks deliberately mirror pkg/selector/prefilter exactly — same
// timeout (MetricUpdateTimeout) for both metric timestamps — so the guard
// never drops a node the canonical prefilter would have kept.
// SchedulerConf.LocalMetricUpdateTimeout exists in conf.yaml but is
// referenced nowhere else in the codebase; until the canonical prefilter
// adopts a dedicated local-metric timeout, this guard shares
// MetricUpdateTimeout for the local timestamp as well so the two paths
// cannot disagree. One intentional divergence: the prefilter Fatalf's on
// CpuLoadUsage > CpuTotal (an invariant violation worth a crash in the
// canonical path), while this guard drops the node — a profile guard must
// not take the scheduler down.
type nodeSafetyFilter struct{}

func NewNodeSafetyFilter() *nodeSafetyFilter { return &nodeSafetyFilter{} }

func (*nodeSafetyFilter) ID() string { return constants.SelectorFilterID + "/node_safety" }

func (*nodeSafetyFilter) Select(selection *selctx.SelectorCtx) (node.NodeList, error) {
	current := config.GetConfig()
	if current == nil || current.Scheduler == nil {
		return nil, ret.Errorf(errorcode.ErrorCode_MasterInternalError, "scheduler config is nil")
	}
	scheduler := current.Scheduler
	result := make(node.NodeList, 0, len(selection.Nodes()))
	for _, candidate := range selection.Nodes() {
		if candidate == nil || !candidate.Healthy {
			continue
		}
		if candidate.MvmNum >= localcache.RealMaxMvmLimit(candidate) {
			continue
		}
		if candidate.CpuLoadUsage > float64(candidate.CpuTotal) {
			continue
		}
		if scheduler.MetricUpdateTimeout > 0 && time.Since(candidate.MetricUpdate) > scheduler.MetricUpdateTimeout {
			continue
		}
		if scheduler.MetricUpdateTimeout > 0 && time.Since(candidate.MetricLocalUpdateAt) > scheduler.MetricUpdateTimeout {
			continue
		}
		result = append(result, candidate)
	}
	return result, nil
}
