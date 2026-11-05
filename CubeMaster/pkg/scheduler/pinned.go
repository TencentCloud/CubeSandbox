// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"runtime/debug"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/prefilter"
)

// SelectPinned applies the full scheduling pipeline to the given nodes only.
// It never scans the node cache or falls back to backoff selection, so an
// empty result means none of the nodes can take the request.
func SelectPinned(selCtx *selctx.SelectorCtx, pinned node.NodeList) (selected *node.Node, err error) {
	if selCtx == nil {
		return nil, ret.Err(errorcode.ErrorCode_MasterInternalError, "selector context is nil")
	}
	defer func() {
		if r := recover(); r != nil {
			log.G(selCtx.Ctx).Fatalf("SelectPinned panic:%+v", string(debug.Stack()))
			err = ret.Errorf(errorcode.ErrorCode_MasterInternalError, "SelectPinned panic:%s", r)
		}
	}()
	pipeline, releasePipeline := currentPipeline(selCtx)
	if releasePipeline != nil {
		defer releasePipeline()
	}
	if pipeline == nil {
		return nil, ret.Err(errorcode.ErrorCode_MasterInternalError, "scheduler profile is not initialized")
	}
	selCtx.SetProfileName(pipeline.Name)
	selCtx.SetRejectReason("")

	admitted := make(node.NodeList, 0, len(pinned))
	for i := range pinned {
		if prefilter.NodeAdmitted(selCtx, pinned[i]) {
			admitted.Append(pinned[i])
		}
	}
	if admitted.Len() == 0 {
		return nil, ret.Err(errorcode.ErrorCode_SelectNodesNoRes, ErrNoRes.Error())
	}
	selCtx.SetNodes(admitted)
	freezeSnapshot(selCtx, pipeline)
	if err = runProfileFilters(selCtx, pluginKindGuard, pipeline.Guards); err != nil {
		return nil, err
	}
	if err = runProfileFilters(selCtx, pluginKindFilter, pipeline.Filters); err != nil {
		return nil, err
	}
	if err = runProfileScores(selCtx, pipeline.Scores); err != nil {
		return nil, err
	}
	selected = selectNode(selCtx, pipeline)
	if selected == nil {
		return nil, ret.Err(errorcode.ErrorCode_SelectNodesNoRes, ErrNoRes.Error())
	}
	return selected, nil
}
