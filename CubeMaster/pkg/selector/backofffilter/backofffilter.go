// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package backofffilter provides the prefilter module
package backofffilter

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

type backofffilter struct {
}

func NewBackoffFilter() *backofffilter {
	filter := &backofffilter{}
	return filter
}

func (l *backofffilter) ID() string {
	return constants.SelectorBackoffFilterID
}

// Select 兜底过滤（主过滤失败后的降级路径）：
// 未配置兜底亲和性选择器或该实例类型被禁用兜底过滤时返回空列表；
// 否则放宽条件重新选取节点：仅校验兜底亲和性、MVM 上限与三类磁盘使用率
func (l *backofffilter) Select(selCtx *selctx.SelectorCtx) (node.NodeList, error) {
	sconf := config.GetConfig().Scheduler
	if sconf == nil {
		return nil, ret.Errorf(errorcode.ErrorCode_MasterInternalError, "scheduler config is nil")
	}
	if selCtx.Affinity.BackoffNodeSelector == nil {

		log.G(selCtx.Ctx).Infof("%s backoff filter skipped, no backoffNodeSelector configured", l.ID())
		return node.NodeList{}, nil
	}

	if sconf.DisableBackoffFilterInstanceType != nil {
		if _, ok := sconf.DisableBackoffFilterInstanceType[selCtx.InstanceType]; ok {
			log.G(selCtx.Ctx).Infof("%s backoff filter disabled for instance type: %s", l.ID(), selCtx.InstanceType)
			return node.NodeList{}, nil
		}
	}
	nodes := localcache.GetSchedulableNodesByInstanceType(-1, selCtx.InstanceType)
	if log.IsDebug() {
		log.G(selCtx.Ctx).Debugf("GetSchedulableNodes:%+v,size:%d", nodes.String(), nodes.Len())
	}
	newNodes := make(node.NodeList, 0, nodes.Len())
	for i := range nodes {
		n := nodes[i]

		if selCtx.Affinity.BackoffNodeSelector != nil && !selCtx.Affinity.BackoffNodeSelector.Match(n) {
			log.G(selCtx.Ctx).Warnf("%s backoff affinity_out", n.IP)
			continue
		}

		if n.MvmNum >= localcache.RealMaxMvmLimit(n) {
			log.G(selCtx.Ctx).Errorf("%s NodeMaxMvmNum exceed:%v", n.IP, n.MvmNum)
			continue
		}
		if n.StorageDiskUsagePer >= sconf.DiskUsageMaxPercent {
			log.G(selCtx.Ctx).WithFields(map[string]any{
				"CalleeCluster": n.ClusterLabel,
			}).Fatalf("%v select:%v, StorageDiskUsagePer:%v, DiskUsageMaxPercent:%v",
				l.ID(), n.ID(), n.StorageDiskUsagePer, sconf.DiskUsageMaxPercent)
			continue
		}
		if n.SysDiskUsagePer >= sconf.DiskUsageMaxPercent {
			log.G(selCtx.Ctx).WithFields(map[string]any{
				"CalleeCluster": n.ClusterLabel,
			}).Fatalf("%v select:%v, SysDiskUsagePer:%v, DiskUsageMaxPercent:%v",
				l.ID(), n.ID(), n.SysDiskUsagePer, sconf.DiskUsageMaxPercent)
			continue
		}
		if n.DataDiskUsagePer >= sconf.DiskUsageMaxPercent {
			log.G(selCtx.Ctx).WithFields(map[string]any{
				"CalleeCluster": n.ClusterLabel,
			}).Fatalf("%v select:%v, DataDiskUsagePer:%v, DiskUsageMaxPercent:%v",
				l.ID(), n.ID(), n.DataDiskUsagePer, sconf.DiskUsageMaxPercent)
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
