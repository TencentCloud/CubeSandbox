// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"math/rand"
	"runtime/debug"

	"golang.org/x/sync/errgroup"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
)

// Select 调度主流程：预过滤 -> 兜底过滤（可选）-> 并行过滤 -> 评分 -> 加权随机选出最终节点。
// 过滤阶段失败时，若启用了 template_locality 过滤则直接返回错误，
// 否则降级走 BackoffSelect 兜底路径
func Select(selCtx *selctx.SelectorCtx) (nodes *node.Node, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.G(selCtx.Ctx).Fatalf("Select panic:%+v", string(debug.Stack()))
			err = ret.Errorf(errorcode.ErrorCode_MasterInternalError, "Select panic:%s", r)
		}
	}()

	// 预过滤：取出候选节点集合
	if err := runPreFilter(selCtx); err != nil {
		if shouldSkipBackoffForTemplate(selCtx) {
			return nil, err
		}

		// 预过滤失败时尝试宽松条件的兜底过滤
		if err = runBackoffFilter(selCtx); err != nil {
			return nil, err
		}
	}

	// 主过滤：并行执行所有过滤插件，节点必须通过全部过滤
	if err := runFilter(selCtx, scheduler.filter); err != nil {
		if shouldSkipBackoffForTemplate(selCtx) {
			return nil, err
		}

		log.G(selCtx.Ctx).Errorf("scheduler_Select fail,try BackoffSelect")
		return BackoffSelect(selCtx)
	}

	// 评分：各评分插件加权求和，得到节点评分列表
	if err := runScoreFilter(selCtx, scheduler.score); err != nil {
		return nil, err
	}

	// 从评分最高的前 N 个节点中加权随机选择一个
	return selCtx.LeastRandomSelect(config.GetConfig().Scheduler.PrioritySelectNum), nil
}

// shouldSkipBackoffForTemplate 判断是否跳过兜底（Backoff）路径：
// 当请求携带 TemplateID 且启用了 template_locality 过滤插件时，
// 模板本地化约束必须严格执行，不允许降级到宽松的兜底过滤
func shouldSkipBackoffForTemplate(selCtx *selctx.SelectorCtx) bool {
	if selCtx == nil || selCtx.ReqRes == nil || selCtx.ReqRes.TemplateID == "" {
		return false
	}
	templateLocalitySelectorID := constants.SelectorFilterID + "/" + "template_locality"
	for _, selector := range scheduler.filter {
		if selector != nil && selector.ID() == templateLocalitySelectorID {
			return true
		}
	}
	return false
}

// BackoffSelect 兜底选节点：用宽松条件的 backoffSelector 重新选取候选节点，
// 再从候选中随机选一个返回（用于主过滤失败后的降级路径）
func BackoffSelect(selCtx *selctx.SelectorCtx) (nodes *node.Node, err error) {
	if scheduler.backoffSelector == nil {
		return nil, ret.Err(errorcode.ErrorCode_MasterInternalError, "should RegisterPreSelector")
	}

	if result, err := scheduler.backoffSelector.Select(selCtx); err != nil {
		return nil, ret.Err(errorcode.ErrorCode_SelectNodesFailed, ErrPreSelect.Error())
	} else {
		selCtx.SetNodes(result)
	}

	if selCtx.Nodes().Len() == 0 {
		return nil, ret.Err(errorcode.ErrorCode_SelectNodesNoRes, ErrNoRes.Error())
	}

	selectedHost := selCtx.Nodes()[rand.Intn(selCtx.Nodes().Len())]
	return selectedHost, nil
}

// runBackoffFilter 执行兜底过滤：用 backoffSelector 选出候选节点并写回 selCtx
func runBackoffFilter(selCtx *selctx.SelectorCtx) (err error) {
	if scheduler.backoffSelector == nil {
		return ret.Err(errorcode.ErrorCode_MasterInternalError, "should RegisterPreSelector")
	}

	if result, err := scheduler.backoffSelector.Select(selCtx); err != nil {
		return ret.Err(errorcode.ErrorCode_SelectNodesFailed, ErrPreSelect.Error())
	} else {
		selCtx.SetNodes(result)
	}

	if selCtx.Nodes().Len() == 0 {
		return ret.Err(errorcode.ErrorCode_SelectNodesNoRes, ErrNoRes.Error())
	}
	return nil
}

// runPreFilter 执行预过滤：用 preSelector 选出候选节点并写回 selCtx
func runPreFilter(selCtx *selctx.SelectorCtx) (err error) {
	if scheduler.preSelector == nil {
		return ret.Err(errorcode.ErrorCode_MasterInternalError, "should RegisterPreSelector")
	}

	if result, err := scheduler.preSelector.Select(selCtx); err != nil {
		return ret.Err(errorcode.ErrorCode_SelectNodesFailed, ErrPreSelect.Error())
	} else {
		selCtx.SetNodes(result)
	}

	if selCtx.Nodes().Len() == 0 {
		return ret.Err(errorcode.ErrorCode_SelectNodesNoRes, ErrNoRes.Error())
	}
	return nil
}

// runFilter 并行执行所有过滤插件，并把通过全部过滤的节点写回 selCtx
func runFilter(selCtx *selctx.SelectorCtx, filters []filter.Selector) error {
	if tmpResult, err := parallelRunFilters(selCtx, filters); err != nil {
		log.G(selCtx.Ctx).Warnf("runFilter_failed, err: %v", err)
		return err
	} else {
		if tmpResult.Len() == 0 {
			return ret.Err(errorcode.ErrorCode_SelectNodesNoRes, ErrNoRes.Error())
		}
		selCtx.SetNodes(tmpResult)
	}
	return nil
}

// parallelRunFilters 并发执行所有过滤插件：
// 每个过滤插件独立运行，统计每个节点通过（被各插件保留）的次数；
// 只有被全部过滤插件保留的节点才进入最终结果
func parallelRunFilters(selCtx *selctx.SelectorCtx, filters []filter.Selector) (node.NodeList, error) {
	eg, _ := errgroup.WithContext(selCtx.Ctx)
	tmpStat := &utils.AtomicMapStat{}
	for _, f := range filters {
		f := f
		eg.Go(func() (err error) {
			f := f
			defer func() {
				if r := recover(); r != nil {
					err = ret.Errorf(errorcode.ErrorCode_MasterInternalError, "parallelRunFilters panic:%s", r)
				}
			}()

			if tmp, err := f.Select(selCtx); err != nil {
				return err
			} else {
				for _, n := range tmp {

					tmpStat.Add(n.ID(), 1)
				}
			}
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		log.G(selCtx.Ctx).Errorf("parallelRunFilters failed, err: %v", err)
		return nil, ret.Err(errorcode.ErrorCode_MasterInternalError, err.Error())
	}

	result := node.NodeList{}
	expectedCnt := len(filters)
	for _, n := range selCtx.Nodes() {
		if expectedCnt == tmpStat.Get(n.ID()) {
			result.Append(n)
		}
	}
	return result, nil
}

// runScoreFilter 执行评分阶段：
// 逐个运行评分插件（跳过被禁用的），每个插件返回的分数乘以其权重后累加到节点总分；
// 最后按总权重归一化，并按分数降序排序得到评分结果列表。
// 评分结果还会交给 postScore 做后处理（如白名单加权调整）
func runScoreFilter(selCtx *selctx.SelectorCtx, scores []score.Selector) error {
	if len(scores) == 0 {
		return nil
	}

	totalPluginWeight := 0.0

	resultMap := map[string]*node.NodeScore{}
	for _, f := range scores {
		if f.Disable() {
			continue
		}
		if tmpResult, err := f.Select(selCtx); err != nil {
			continue
		} else {
			if len(tmpResult) > 0 {
				totalPluginWeight += f.Weight()
				for _, n := range tmpResult {

					n.Score *= f.Weight()
					if old, ok := resultMap[n.ID()]; ok {
						old.Score += n.Score
					} else {

						resultMap[n.ID()] = n
					}
				}
			}
		}
	}

	if len(resultMap) == 0 {
		if selCtx.Nodes().Len() == 0 {
			return ret.Err(errorcode.ErrorCode_SelectNodesNoRes, ErrNoRes.Error())
		}
		return nil
	}

	result := make(node.NodeScoreList, 0, len(resultMap))
	if totalPluginWeight == 0.0 {
		totalPluginWeight = 1.0
	}

	// 按总权重归一化，避免各插件权重比例影响排序
	for _, n := range resultMap {
		n.Score /= totalPluginWeight
		result.Append(n)
	}

	if scheduler.postScore != nil {
		_ = scheduler.postScore.PostedScore(selCtx, resultMap)
	}

	result.AllSortByScore()
	if log.IsDebug() {
		log.G(selCtx.Ctx).Debugf("runScoreFilter:%v", result.String())
	} else {
		log.G(selCtx.Ctx).Infof("runScoreFilter:%v", result.Len())
	}

	selCtx.SetNodeScoreList(result)
	if selCtx.Nodes().Len() == 0 {
		return ret.Err(errorcode.ErrorCode_SelectNodesNoRes, ErrNoRes.Error())
	}
	return nil
}
