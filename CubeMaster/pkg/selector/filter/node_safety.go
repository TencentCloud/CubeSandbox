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
type nodeSafetyFilter struct{}

func NewNodeSafetyFilter() *nodeSafetyFilter { return &nodeSafetyFilter{} }

func (*nodeSafetyFilter) ID() string { return constants.SelectorFilterID + "/node_safety" }

func (*nodeSafetyFilter) Select(selection *selctx.SelectorCtx) (node.NodeList, error) {
	current := config.GetConfig()
	if current == nil || current.Scheduler == nil {
		return nil, ret.Errorf(errorcode.ErrorCode_MasterInternalError, "scheduler config is nil")
	}
	scheduler := current.Scheduler
	localMetricTimeout := scheduler.LocalMetricUpdateTimeout
	if localMetricTimeout <= 0 {
		localMetricTimeout = scheduler.MetricUpdateTimeout
	}
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
		if localMetricTimeout > 0 && time.Since(candidate.MetricLocalUpdateAt) > localMetricTimeout {
			continue
		}
		result = append(result, candidate)
	}
	return result, nil
}
