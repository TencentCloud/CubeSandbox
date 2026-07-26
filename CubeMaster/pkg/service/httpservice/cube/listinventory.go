// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

func handleListInventoryAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	rsp := &types.ListInventoryRes{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	defer func() {
		rt.RetCode = int64(rsp.Ret.RetCode)
	}()
	req := &types.ListInventoryReq{}
	if err := utils.DecodeHttpBody(c.Request.Body, req); err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_MasterParamsError)
		rsp.Ret.RetMsg = err.Error()
		common.WriteAPI(c, rsp)
		return
	}

	rt.RequestID = req.RequestID
	rsp.RequestID = req.RequestID
	if req.InstanceType == "" {
		req.InstanceType = cubebox.InstanceType_cubebox.String()
	}
	rt.InstanceType = req.InstanceType
	ctx := log.WithLogger(c.Request.Context(), log.G(c.Request.Context()).WithFields(map[string]any{
		"RequestId":    req.RequestID,
		"InstanceType": req.InstanceType,
	}))
	log.G(ctx).Infof("handleListInventoryAction:%+v", utils.InterfaceToString(req))
	allNodes := localcache.GetHealthyNodesByInstanceType(-1, req.InstanceType)
	inventoryType := map[string]*types.InstanceTypeQuotaItem{}
	zoneFilter := []string{}
	cpuTypeFilter := []string{}

	for _, v := range req.Filters {
		if v.Name == "zone" {
			zoneFilter = v.Values
		}
		if v.Name == "cpu-type" {
			cpuTypeFilter = v.Values
		}
	}

	for _, n := range allNodes {
		if len(zoneFilter) > 0 {
			if !utils.Contains(n.Zone, zoneFilter) {
				continue
			}
		}
		if len(cpuTypeFilter) > 0 {
			if !utils.Contains(n.CPUType, cpuTypeFilter) {
				continue
			}
		}

		node_max_mem_reserved_in_mb := getNodeMaxMemReservedConf(n)
		info := &types.InstanceTypeQuotaItem{
			Zone:    n.Zone,
			CPUType: n.CPUType,
			Memory:  n.QuotaMem - n.QuotaMemUsage - node_max_mem_reserved_in_mb,
			CPU:     int64((n.QuotaCpu - n.QuotaCpuUsage) / 1000),
		}
		mergeInventory(inventoryType, info)
	}
	for _, v := range inventoryType {
		rsp.Data = append(rsp.Data, v)
	}
	log.G(ctx).Infof("handleListInventoryAction success:%s", utils.InterfaceToString(rsp))
	common.WriteAPI(c, rsp)
}

func mergeInventory(all map[string]*types.InstanceTypeQuotaItem, in *types.InstanceTypeQuotaItem) {
	key := in.Zone + in.CPUType
	if old, ok := all[key]; !ok {
		all[key] = in
	} else {
		old.CPU += in.CPU
		old.Memory += in.Memory
	}
}

func getNodeMaxMemReservedConf(n *node.Node) int64 {
	if n == nil {
		return 0
	}
	if sconf := config.GetConfig().Scheduler; sconf != nil {
		return sconf.GetEffectiveNodeMaxMemReservedInMB(n.InstanceType, n.QuotaMem)
	}
	return 0
}
