// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"runtime/debug"
	"sort"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/profile"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
)

// Select 调度主流程：预过滤 -> 兜底过滤（可选）-> 并行过滤 -> 评分 -> 加权随机选出最终节点。
// 过滤阶段失败时，若启用了 template_locality 过滤则直接返回错误，
// 否则降级走 BackoffSelect 兜底路径
func Select(selCtx *selctx.SelectorCtx) (nodes *node.Node, err error) {
	if selCtx == nil {
		return nil, ret.Err(errorcode.ErrorCode_MasterInternalError, "selector context is nil")
	}
	startTime := time.Now()
	// Registered first so it runs after the panic-recovery defer below and
	// observes the final (nodes, err) pair, including the BackoffSelect
	// fallback and recovered panics.
	defer func() { observeSelect(selCtx, nodes, err, startTime) }()

	defer func() {
		if r := recover(); r != nil {
			log.G(selCtx.Ctx).Fatalf("Select panic:%+v", string(debug.Stack()))
			err = ret.Errorf(errorcode.ErrorCode_MasterInternalError, "Select panic:%s", r)
		}
	}()
	pipeline, releasePipeline := currentPipeline(selCtx)
	if releasePipeline != nil {
		defer releasePipeline()
	}
	if pipeline == nil {
		return nil, ret.Err(errorcode.ErrorCode_MasterInternalError, "scheduler profile is not initialized")
	}
	selCtx.ProfileName = pipeline.Name

	// 预过滤：取出候选节点集合
	if err := runPreFilter(selCtx); err != nil {
		legacy := len(pipeline.Guards) == 0
		if pipeline.NoCandidate != profile.NoCandidateBackoff ||
			(legacy && pipelineHasTemplateGuard(selCtx, pipeline)) ||
			(!legacy && !isNoCandidateError(err)) {
			return nil, err
		}

		// 预过滤失败时尝试宽松条件的兜底过滤
		if len(pipeline.Guards) == 0 {
			if err = runBackoffFilter(selCtx); err != nil {
				return nil, err
			}
		} else {
			return backoffSelectWithPipeline(selCtx, pipeline)
		}
	}
	freezeSnapshot(selCtx)

	// 主过滤：并行执行所有过滤插件，节点必须通过全部过滤
	if err := runProfileFilters(selCtx, pipeline.Guards); err != nil {
		return nil, err
	}
	if err := runProfileFilters(selCtx, pipeline.Filters); err != nil {
		legacy := len(pipeline.Guards) == 0
		if pipeline.NoCandidate != profile.NoCandidateBackoff ||
			(legacy && pipelineHasTemplateGuard(selCtx, pipeline)) ||
			(!legacy && !isNoCandidateError(err)) {
			return nil, err
		}

		log.G(selCtx.Ctx).Errorf("scheduler_Select fail,try BackoffSelect")
		if len(pipeline.Guards) == 0 {
			return BackoffSelect(selCtx)
		}
		return backoffSelectWithPipeline(selCtx, pipeline)
	}

	// 评分：各评分插件加权求和，得到节点评分列表
	if err := runProfileScores(selCtx, pipeline.Scores); err != nil {
		return nil, err
	}

	// 按 Profile 的选择策略（best / top_n）从评分结果中选出最终节点
	selected := selectNode(selCtx, pipeline)
	if selected == nil && pipeline.NoCandidate == profile.NoCandidateBackoff {
		if len(pipeline.Guards) == 0 {
			return BackoffSelect(selCtx)
		}
		return backoffSelectWithPipeline(selCtx, pipeline)
	}
	return selected, nil
}

func currentPipeline(selCtx *selctx.SelectorCtx) (*profile.Pipeline, func()) {
	for profiles := scheduler.profiles.Load(); profiles != nil; profiles = scheduler.profiles.Load() {
		if pipeline, release, acquired := profiles.Acquire(selCtx); acquired {
			return pipeline, release
		}
	}
	// Tests and legacy embedders may construct the package singleton directly.
	// Keep that path functional without weakening production validation.
	topN := 1
	if current := config.GetConfig(); current != nil && current.Scheduler != nil {
		topN = current.Scheduler.PrioritySelectNum
	}
	pipeline := &profile.Pipeline{
		Name: "default", TopN: topN,
		Selection: profile.SelectionRandom, NoCandidate: profile.NoCandidateBackoff,
	}
	for _, selector := range scheduler.filter {
		pipeline.Filters = append(pipeline.Filters, profile.FilterPlugin{
			Name: selector.ID(), Selector: selector, Failure: profile.FilterFailClosed,
		})
	}
	for _, selector := range scheduler.score {
		pipeline.Scores = append(pipeline.Scores, profile.ScorePlugin{
			Name: selector.ID(), Selector: selector, Weight: selector.Weight(), Failure: profile.ScoreSkip,
		})
	}
	return pipeline, nil
}

func isNoCandidateError(err error) bool {
	status, _ := ret.FromError(err)
	return status != nil && status.Code() == errorcode.ErrorCode_SelectNodesNoRes
}

// pipelineHasTemplateGuard 判断当前 Profile 是否启用了 template_locality 过滤：
// 启用时模板本地化约束必须严格执行，不允许降级到宽松的兜底过滤
func pipelineHasTemplateGuard(selCtx *selctx.SelectorCtx, pipeline *profile.Pipeline) bool {
	if selCtx == nil || selCtx.ReqRes == nil || selCtx.ReqRes.TemplateID == "" || pipeline == nil {
		return false
	}
	templateLocalitySelectorID := constants.SelectorFilterID + "/" + "template_locality"
	for _, binding := range append(append([]profile.FilterPlugin(nil), pipeline.Guards...), pipeline.Filters...) {
		if binding.Selector != nil && binding.Selector.ID() == templateLocalitySelectorID {
			return true
		}
	}
	return false
}

func selectNode(selCtx *selctx.SelectorCtx, pipeline *profile.Pipeline) *node.Node {
	if selCtx == nil || pipeline == nil || selCtx.Nodes().Len() == 0 {
		return nil
	}
	if pipeline.Selection == profile.SelectionHighest {
		return selCtx.Nodes()[0]
	}
	return selCtx.LeastRandomSelect(pipeline.TopN)
}

func backoffSelectWithPipeline(selCtx *selctx.SelectorCtx, pipeline *profile.Pipeline) (*node.Node, error) {
	if err := runBackoffFilter(selCtx); err != nil {
		return nil, err
	}
	freezeSnapshot(selCtx)
	if err := runProfileFilters(selCtx, pipeline.Guards); err != nil {
		return nil, err
	}
	if err := runProfileFilters(selCtx, pipeline.Filters); err != nil {
		return nil, err
	}
	if err := runProfileScores(selCtx, pipeline.Scores); err != nil {
		return nil, err
	}
	selected := selectNode(selCtx, pipeline)
	if selected == nil {
		return nil, ret.Err(errorcode.ErrorCode_SelectNodesNoRes, ErrNoRes.Error())
	}
	return selected, nil
}

func freezeSnapshot(selCtx *selctx.SelectorCtx) {
	facts := make(map[string]selctx.SnapshotNodeFacts, len(selCtx.Nodes()))
	request := selCtx.GetReqRes()
	for _, candidate := range selCtx.Nodes() {
		if candidate == nil {
			continue
		}
		value := selctx.SnapshotNodeFacts{}
		if request != nil && request.TemplateID != "" && !request.AllowNonLocalTemplate {
			value.TemplateLocalKnown = true
			value.TemplateLocal = localcache.GetImageStateByNode(request.TemplateID, candidate.ID()) != nil
		}
		if request != nil && request.EnforceSnapshotStorage {
			value.SnapshotStorageKnown = true
			state, ok := localcache.GetSnapshotStorageState(candidate.ID(), candidate.HostIP())
			value.SnapshotStorageAllowed = ok && localcache.IsSnapshotStorageWriteAllowed(state.Mode)
		}
		facts[candidate.ID()] = value
	}
	selCtx.SetSnapshotFacts(facts)
	selCtx.FreezeSnapshot()
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

// runProfileFilters 并发执行 Profile 中的一组过滤插件：
// 每个插件独立运行并校验其返回的候选节点，只有被全部插件保留的节点才进入结果；
// 插件失败按 Failure 策略决定 fail-open（放行全部候选）或 fail-closed（直接报错）
func runProfileFilters(selCtx *selctx.SelectorCtx, filters []profile.FilterPlugin) error {
	if len(filters) == 0 {
		return nil
	}
	type filterResult struct {
		nodes node.NodeList
		err   error
	}
	results := make([]filterResult, len(filters))
	candidates := make(map[string]struct{}, len(selCtx.Nodes()))
	for _, candidate := range selCtx.Nodes() {
		if candidate == nil || candidate.ID() == "" {
			return ret.Err(errorcode.ErrorCode_MasterInternalError, "scheduler candidate has an empty id")
		}
		if _, duplicate := candidates[candidate.ID()]; duplicate {
			return ret.Errorf(errorcode.ErrorCode_MasterInternalError, "duplicate scheduler candidate id %q", candidate.ID())
		}
		candidates[candidate.ID()] = struct{}{}
	}
	eg, _ := errgroup.WithContext(selCtx.Ctx)
	for index := range filters {
		index := index
		eg.Go(func() (err error) {
			defer func() {
				if recovered := recover(); recovered != nil {
					results[index].err = fmt.Errorf("filter plugin %q panic: %v", filters[index].Name, recovered)
				}
			}()
			if filters[index].Selector == nil {
				results[index].err = fmt.Errorf("filter plugin %q is nil", filters[index].Name)
				return nil
			}
			results[index].nodes, results[index].err = filters[index].Selector.Select(selCtx)
			return nil
		})
	}
	_ = eg.Wait()

	counts := make(map[string]int, len(selCtx.Nodes()))
	for index, result := range results {
		seen := make(map[string]struct{}, len(result.nodes))
		if result.err == nil {
			for _, candidate := range result.nodes {
				switch {
				case candidate == nil:
					result.err = errors.New("filter plugin returned a nil node")
				case candidate.ID() == "":
					result.err = fmt.Errorf("filter plugin %q returned a node with an empty id", filters[index].Name)
				default:
					if _, exists := candidates[candidate.ID()]; !exists {
						result.err = fmt.Errorf("filter plugin %q returned non-candidate node %q", filters[index].Name, candidate.ID())
					} else if _, duplicate := seen[candidate.ID()]; duplicate {
						result.err = fmt.Errorf("filter plugin %q returned duplicate node %q", filters[index].Name, candidate.ID())
					} else {
						seen[candidate.ID()] = struct{}{}
					}
				}
				if result.err != nil {
					break
				}
			}
		}
		if result.err != nil {
			if filters[index].Failure == profile.FilterFailOpen {
				log.G(selCtx.Ctx).Warnf("RISK: scheduler filter %q failed open: %v", filters[index].Name, result.err)
				for _, candidate := range selCtx.Nodes() {
					counts[candidate.ID()]++
				}
				continue
			}
			return ret.Err(errorcode.ErrorCode_MasterInternalError, result.err.Error())
		}
		for _, candidate := range result.nodes {
			counts[candidate.ID()]++
		}
	}
	result := make(node.NodeList, 0, len(selCtx.Nodes()))
	for _, candidate := range selCtx.Nodes() {
		if counts[candidate.ID()] == len(filters) {
			result = append(result, candidate)
		}
	}
	if len(result) == 0 {
		return ret.Err(errorcode.ErrorCode_SelectNodesNoRes, ErrNoRes.Error())
	}
	selCtx.SetNodes(result)
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
	bindings := make([]profile.ScorePlugin, 0, len(scores))
	for _, selector := range scores {
		bindings = append(bindings, profile.ScorePlugin{
			Name: selector.ID(), Selector: selector, Weight: selector.Weight(), Failure: profile.ScoreSkip,
		})
	}
	return runProfileScores(selCtx, bindings)
}

func runProfileScores(selCtx *selctx.SelectorCtx, scores []profile.ScorePlugin) error {
	if len(scores) == 0 {
		return nil
	}
	type scoreResult struct {
		nodes node.NodeScoreList
		err   error
		skip  bool
	}
	results := make([]scoreResult, len(scores))
	eg, _ := errgroup.WithContext(selCtx.Ctx)
	for index := range scores {
		index := index
		eg.Go(func() (err error) {
			defer func() {
				if recovered := recover(); recovered != nil {
					results[index].err = fmt.Errorf("score plugin %q panic: %v", scores[index].Name, recovered)
				}
			}()
			selector := scores[index].Selector
			if selector == nil {
				results[index].err = fmt.Errorf("score plugin %q is nil", scores[index].Name)
				return nil
			}
			if !scores[index].ForceEnabled && selector.Disable() {
				results[index].skip = true
				return nil
			}
			results[index].nodes, results[index].err = selector.Select(selCtx)
			return nil
		})
	}
	_ = eg.Wait()

	totalWeight := 0.0
	aggregated := make(map[string]*node.NodeScore, len(selCtx.Nodes()))
	candidates := make(map[string]*node.Node, len(selCtx.Nodes()))
	for _, candidate := range selCtx.Nodes() {
		if candidate == nil || candidate.ID() == "" {
			return ret.Err(errorcode.ErrorCode_MasterInternalError, "scheduler candidate has an empty id")
		}
		if _, duplicate := candidates[candidate.ID()]; duplicate {
			return ret.Errorf(errorcode.ErrorCode_MasterInternalError, "duplicate scheduler candidate id %q", candidate.ID())
		}
		candidates[candidate.ID()] = candidate
	}
	for index, result := range results {
		binding := scores[index]
		if result.skip {
			continue
		}
		if result.err == nil {
			seen := make(map[string]struct{}, len(result.nodes))
			for _, scored := range result.nodes {
				switch {
				case scored == nil:
					result.err = fmt.Errorf("score plugin %q returned a nil score", binding.Name)
				case scored.ID() == "":
					result.err = fmt.Errorf("score plugin %q returned an empty node id", binding.Name)
				default:
					if _, exists := candidates[scored.ID()]; !exists {
						result.err = fmt.Errorf("score plugin %q returned non-candidate node %q", binding.Name, scored.ID())
					} else if _, duplicate := seen[scored.ID()]; duplicate {
						result.err = fmt.Errorf("score plugin %q returned duplicate node %q", binding.Name, scored.ID())
					} else if math.IsNaN(scored.Score) || math.IsInf(scored.Score, 0) {
						result.err = fmt.Errorf("score plugin %q returned invalid score for node %q", binding.Name, scored.ID())
					} else if binding.ForceEnabled && (scored.Score < 0 || scored.Score > 100) {
						result.err = fmt.Errorf("score plugin %q returned %v for node %q outside [0,100]", binding.Name, scored.Score, scored.ID())
					} else {
						seen[scored.ID()] = struct{}{}
					}
				}
				if result.err != nil {
					break
				}
			}
			if result.err == nil && binding.ForceEnabled && len(seen) != len(candidates) {
				result.err = fmt.Errorf("score plugin %q returned %d scores for %d candidates", binding.Name, len(seen), len(candidates))
			}
		}
		if result.err != nil {
			switch binding.Failure {
			case profile.ScoreFailClosed:
				return ret.Err(errorcode.ErrorCode_MasterInternalError, result.err.Error())
			case profile.ScoreDefaultScore:
				log.G(selCtx.Ctx).Warnf("scheduler score %q failed; using default score %.2f: %v", binding.Name, binding.DefaultScore, result.err)
				result.nodes = make(node.NodeScoreList, 0, len(selCtx.Nodes()))
				for _, candidate := range selCtx.Nodes() {
					result.nodes = append(result.nodes, &node.NodeScore{InsID: candidate.ID(), OrigNode: candidate, MvmNum: candidate.MvmNum, Score: binding.DefaultScore})
				}
			default: // legacy skip-on-error behavior
				continue
			}
		}
		if len(result.nodes) == 0 {
			continue
		}
		weight := binding.Weight
		if weight == 0 {
			weight = binding.Selector.Weight()
		}
		if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
			return ret.Errorf(errorcode.ErrorCode_MasterInternalError, "score plugin %q has invalid weight %v", binding.Name, weight)
		}
		totalWeight += weight
		for _, scored := range result.nodes {
			candidate := candidates[scored.ID()]
			if existing := aggregated[scored.ID()]; existing != nil {
				existing.Score += scored.Score * weight
			} else {
				aggregated[scored.ID()] = &node.NodeScore{
					InsID: scored.ID(), OrigNode: candidate, MvmNum: candidate.MvmNum, Score: scored.Score * weight,
				}
			}
		}
	}
	if len(aggregated) == 0 {
		if selCtx.Nodes().Len() == 0 {
			return ret.Err(errorcode.ErrorCode_SelectNodesNoRes, ErrNoRes.Error())
		}
		return nil
	}
	if totalWeight == 0 {
		totalWeight = 1
	}
	// 按总权重归一化，避免各插件权重比例影响排序；按候选节点顺序收集结果
	result := make(node.NodeScoreList, 0, len(aggregated))
	for _, candidate := range selCtx.Nodes() {
		if scored := aggregated[candidate.ID()]; scored != nil {
			scored.Score /= totalWeight
			result = append(result, scored)
		}
	}
	if scheduler.postScore != nil {
		_ = scheduler.postScore.PostedScore(selCtx, aggregated)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Score > result[j].Score })
	if log.IsDebug() {
		log.G(selCtx.Ctx).Debugf("runScoreFilter profile=%s:%v", selCtx.ProfileName, result.String())
	} else {
		log.G(selCtx.Ctx).Infof("runScoreFilter profile=%s:%v", selCtx.ProfileName, result.Len())
	}
	selCtx.SetNodeScoreList(result)
	if selCtx.Nodes().Len() == 0 {
		return ret.Err(errorcode.ErrorCode_SelectNodesNoRes, ErrNoRes.Error())
	}
	return nil
}
