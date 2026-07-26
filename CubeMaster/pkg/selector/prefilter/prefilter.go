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

func NewPreFilter() *prefilter {
	filter := &prefilter{}
	return filter
}

func (l *prefilter) ID() string {
	return constants.SelectorPreFilterID
}

func (l *prefilter) Select(selCtx *selctx.SelectorCtx) (node.NodeList, error) {
	sconf := config.GetConfig().Scheduler
	if sconf == nil {
		return nil, ret.Errorf(errorcode.ErrorCode_MasterInternalError, "scheduler config is nil")
	}

	nodes := localcache.GetSchedulableNodesByInstanceType(sconf.PreSelectNum, selCtx.InstanceType)

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
