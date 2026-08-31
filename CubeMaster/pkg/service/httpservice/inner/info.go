// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package inner

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func getNodeInfo(ctx context.Context, req *types.GetNodeReq) (rsp *types.GetNodeRes) {
	log.G(ctx).Debugf("%+v", utils.InterfaceToString(req))
	defer func() {
		if log.IsDebug() {
			log.G(ctx).Debugf("getNodeInfo_rsp:%+v", utils.InterfaceToString(rsp))
		}
		CubeLog.GetTraceInfo(ctx).RetCode = int64(rsp.Ret.RetCode)
	}()
	if req.HostID != "" {
		n, exist := localcache.GetNode(req.HostID)
		if !exist {
			return &types.GetNodeRes{
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_NotFound),
					RetMsg:  errorcode.ErrorCode_NotFound.String(),
				},
			}
		}
		rsp = &types.GetNodeRes{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_Success),
				RetMsg:  errorcode.ErrorCode_Success.String(),
			},
		}
		if req.ScoreOnly {
			rsp.Data = append(rsp.Data, &node.Node{
				InsID:               n.ID(),
				MetricLocalUpdateAt: n.MetricLocalUpdateAt,
				MetaDataUpdateAt:    n.MetaDataUpdateAt,
				MetricUpdate:        n.MetricUpdate,
				Score:               n.Score,
			})
		} else {
			rsp.Data = append(rsp.Data, n)
		}
		return
	}

	nodes := localcache.GetNodes(-1)
	rsp = &types.GetNodeRes{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	if req.ScoreOnly {
		for _, n := range nodes {
			rsp.Data = append(rsp.Data, &node.Node{
				InsID:               n.ID(),
				Score:               n.Score,
				MetricLocalUpdateAt: n.MetricLocalUpdateAt,
				MetaDataUpdateAt:    n.MetaDataUpdateAt,
				MetricUpdate:        n.MetricUpdate,
			})
		}
	} else {
		rsp.Data = nodes
	}
	return rsp
}
