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

// getSchedulableNodesByInstanceType fetches schedulable nodes (swappable for tests).
var getSchedulableNodesByInstanceType = localcache.GetSchedulableNodesByInstanceType

func NewPreFilter() *prefilter {
	filter := &prefilter{}
	return filter
}

func (l *prefilter) ID() string {
	return constants.SelectorPreFilterID
}

// Select pre-filters: take schedulable nodes from the local cache and drop
// unhealthy nodes, nodes on the recent-filter list (when circuit filtering is
// enabled), nodes violating hard affinity constraints, nodes at the MVM cap,
// and nodes with stale metrics (global or local).
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
		if nodeAdmitted(selCtx, sconf, nodes[i]) {
			newNodes.Append(nodes[i])
		}
	}
	if log.IsDebug() {
		log.G(selCtx.Ctx).Debugf("%v select:%v", l.ID(), newNodes.String())
	}
	return newNodes, nil
}

// NodeAdmitted reports whether a single node passes the same prefilter
// checks as Select.
func NodeAdmitted(selCtx *selctx.SelectorCtx, n *node.Node) bool {
	if selCtx == nil || n == nil {
		return false
	}
	cfg := config.GetConfig()
	if cfg == nil || cfg.Scheduler == nil {
		return false
	}
	return nodeAdmitted(selCtx, cfg.Scheduler, n)
}

func nodeAdmitted(selCtx *selctx.SelectorCtx, sconf *config.WrapperSchedulerConf, n *node.Node) bool {
	if n == nil || !n.Healthy {
		if n != nil {
			log.G(selCtx.Ctx).Warnf("%s not healthy", n.IP)
		}
		return false
	}
	if !sconf.DisableCircuitFilter {
		if selCtx.FilterOut(n) {
			log.G(selCtx.Ctx).Warnf("%s filter_out", n.IP)
			return false
		}
	}
	if selCtx.Affinity.NodeSelector != nil && !selCtx.Affinity.NodeSelector.Match(n) {
		log.G(selCtx.Ctx).Warnf("%s affinity_out", n.IP)
		return false
	}
	if n.MvmNum >= localcache.RealMaxMvmLimit(n) {
		log.G(selCtx.Ctx).WithFields(map[string]any{
			"CalleeCluster": n.ClusterLabel,
		}).Errorf("%s NodeMaxMvmNum exceed:%v", n.IP, n.MvmNum)
		return false
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
		return false
	}
	if time.Since(n.MetricLocalUpdateAt) > sconf.MetricUpdateTimeout {
		log.G(selCtx.Ctx).WithFields(map[string]any{
			"CalleeCluster": n.ClusterLabel,
		}).Warnf("%s MetricLocalUpdate timeout,lastupdate:%v", n.IP, n.MetricLocalUpdateAt)
		return false
	}
	return true
}

// SetSchedulableSourceForTest replaces the node source used by Select and
// returns a function that restores it.
func SetSchedulableSourceForTest(fn func(n int, product string) node.NodeList) func() {
	prev := getSchedulableNodesByInstanceType
	if fn == nil {
		fn = localcache.GetSchedulableNodesByInstanceType
	}
	getSchedulableNodesByInstanceType = fn
	return func() { getSchedulableNodesByInstanceType = prev }
}
