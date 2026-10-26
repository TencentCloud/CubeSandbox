// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/affinity"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func PreSchedule(ctx context.Context, req *types.CreateCubeSandboxReq) (*node.Node, error) {
	selctx := selctx.New(config.GetConfig().Scheduler.LeastSelectName)

	if v := constants.GetNodeSelector(ctx); v != nil {
		nl, ok := v.(affinity.NodeSelector)
		if ok {
			selctx.Affinity.NodeSelector = nl
		}
	}
	if v := constants.GetPreferredSchedulingTerms(ctx); v != nil {
		np, ok := v.(affinity.PreferredSchedulingTerms)
		if ok {
			selctx.Affinity.NodePrefererd = np
		}
	}

	reqResource, err := checkAndGetReqResource(req)
	if err != nil {
		return nil, err
	}
	selctx.ReqRes = reqResource
	selectHost, err := scheduler.Select(selctx)
	if err != nil {
		return nil, err
	}
	if selectHost == nil {
		return nil, scheduler.ErrNoRes
	}
	preOccupy(ctx, selctx)
	return selectHost, nil
}

func preOccupy(ctx context.Context, selCtx *selctx.SelectorCtx) error {

	return nil
}
