// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package prefilter provides the prefilter module
package prefilter

import (
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

type prefilter struct {
}

// getSchedulableNodesByInstanceType 可调度的节点获取函数（可替换，便于测试）
var getSchedulableNodesByInstanceType = localcache.GetSchedulableNodesByInstanceType

func NewPreFilter() *prefilter {
	filter := &prefilter{}
	return filter
}

func (l *prefilter) ID() string {
	return constants.SelectorPreFilterID
}

// Select 预过滤：从本地缓存中取出可调度节点，剔除以下节点：
// 1. 不健康的节点；
// 2. 在最近过滤名单中（未禁用熔断过滤时）；
// 3. 不满足硬性节点亲和性约束的节点；
// 4. MVM 数量达到上限的节点；
// 5. 指标更新时间超时的节点（全局指标或本地指标）
func (l *prefilter) Select(selCtx *selctx.SelectorCtx) (node.NodeList, error) {
	sconf := config.GetConfig().Scheduler
	if sconf == nil {
		return nil, ret.Errorf(errorcode.ErrorCode_MasterInternalError, "scheduler config is nil")
	}

	nodes := getSchedulableNodesByInstanceType(sconf.PreSelectNum, selCtx.InstanceType)

	if log.IsDebug() {
		log.G(selCtx.Ctx).Debugf("GetSchedulableNodesByInstanceType:%+v,size:%d", nodes.String(), nodes.Len())
	}
	newNodes := make(node.NodeList, 0, nodes.Len())
	for i := range nodes {
		n := nodes[i]
		if !n.Healthy {

			log.G(selCtx.Ctx).Warnf("%s not healthy", n.IP)
			continue
		}
		if !sconf.DisableCircuitFilter {
			if selCtx.FilterOut(n) {
				log.G(selCtx.Ctx).Warnf("%s filter_out", n.IP)
				continue
			}
		}

		if selCtx.Affinity.NodeSelector != nil && !selCtx.Affinity.NodeSelector.Match(n) {
			log.G(selCtx.Ctx).Warnf("%s affinity_out", n.IP)
			continue
		}

		if n.MvmNum >= localcache.RealMaxMvmLimit(n) {
			log.G(selCtx.Ctx).WithFields(map[string]any{
				"CalleeCluster": n.ClusterLabel,
			}).Errorf("%s NodeMaxMvmNum exceed:%v", n.IP, n.MvmNum)
			continue
		}

		if n.CpuLoadUsage > float64(n.CpuTotal) {
			log.G(selCtx.Ctx).WithFields(map[string]any{
				"CalleeCluster": n.ClusterLabel,
			}).Fatalf("%s CpuLoadUsage exceed:%v", n.IP, n.CpuLoadUsage)
		}
		if time.Since(n.MetricUpdate) > sconf.MetricUpdateTimeout {
			log.G(selCtx.Ctx).WithFields(map[string]any{
				"CalleeCluster": n.ClusterLabel,
			}).Warnf("%s MetricUpdate timeout,lastupdate:%v", n.IP, n.MetricUpdate)
			continue
		}
		if time.Since(n.MetricLocalUpdateAt) > sconf.MetricUpdateTimeout {
			log.G(selCtx.Ctx).WithFields(map[string]any{
				"CalleeCluster": n.ClusterLabel,
			}).Warnf("%s MetricLocalUpdate timeout,lastupdate:%v", n.IP, n.MetricLocalUpdateAt)
			continue
		}
		newNodes.Append(n)
	}
	if log.IsDebug() {
		log.G(selCtx.Ctx).Debugf("%v select:%v", l.ID(), newNodes.String())
	} else {
		log.G(selCtx.Ctx).Infof("%v select_size:%v", l.ID(), newNodes.Len())
	}
	return newNodes, nil
}
