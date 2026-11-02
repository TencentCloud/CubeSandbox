// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package filter

import (
	"errors"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

// diskFilter 磁盘资源过滤插件：剔除存储盘、系统盘或数据盘使用率超过阈值的节点
type diskFilter struct {
}

func NewDiskFilter() *diskFilter {
	return &diskFilter{}
}

func (l *diskFilter) ID() string {
	return constants.SelectorFilterID + "/" + "disk"
}

func (l *diskFilter) String() string {
	return l.ID()
}

// Select 过滤规则：三类磁盘（存储盘/系统盘/数据盘）的使用率都必须低于 DiskUsageMaxPercent
func (l *diskFilter) Select(selCtx *selctx.SelectorCtx) (node.NodeList, error) {
	sconf := config.GetConfig().Scheduler
	if sconf == nil {
		return nil, errors.New("diskFilter:sconf is nil")
	}
	inList := selCtx.Nodes()
	nodes := make(node.NodeList, 0, inList.Len())
	for i := range inList {
		n := inList[i]
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
		nodes.Append(n)
	}
	if log.IsDebug() {
		log.G(selCtx.Ctx).Debugf("%v select:%v", l.ID(), nodes.String())
	} else {
		log.G(selCtx.Ctx).Infof("%v select_size:%v", l.ID(), nodes.Len())
	}
	return nodes, nil
}
