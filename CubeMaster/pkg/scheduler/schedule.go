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
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/profile"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
)

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
	selCtx.SetProfileName(pipeline.Name)
	// A SelectorCtx may be reused across handleCubelet retries; drop any
	// reject reason stamped by a previous attempt before this one starts.
	selCtx.SetRejectReason("")

	if err := runPreFilter(selCtx); err != nil {
		legacy := len(pipeline.Guards) == 0
		if pipeline.NoCandidate != profile.NoCandidateBackoff ||
			(legacy && pipelineHasTemplateGuard(selCtx, pipeline)) ||
			(!legacy && !isNoCandidateError(err)) {
			return nil, err
		}

		if len(pipeline.Guards) == 0 {
			if err = runBackoffFilter(selCtx); err != nil {
				return nil, err
			}
		} else {
			return backoffSelectWithPipeline(selCtx, pipeline)
		}
	}
	freezeSnapshot(selCtx, pipeline)

	// 主过滤：并行执行所有过滤插件，节点必须通过全部过滤
	if err := runProfileFilters(selCtx, pluginKindGuard, pipeline.Guards); err != nil {
		return nil, err
	}
	if err := runProfileFilters(selCtx, pluginKindFilter, pipeline.Filters); err != nil {
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

	if err := runProfileScores(selCtx, pipeline.Scores); err != nil {
		return nil, err
	}

	// 按 Profile 的选择策略（highest / spread / random）从评分结果中选出最终节点
	selected := selectNode(selCtx, pipeline)
	if selected == nil && pipeline.NoCandidate == profile.NoCandidateBackoff {
		if len(pipeline.Guards) == 0 {
			return BackoffSelect(selCtx)
		}
		return backoffSelectWithPipeline(selCtx, pipeline)
	}
	return selected, nil
}

// currentPipeline returns nil when InitScheduler has not stored a compiled
// profile set. Callers must treat that as a hard error: synthesizing a
// fallback pipeline here would schedule without any of the safety guards
// (node_safety/cpu/mem/disk/...) that only exist inside compiled profiles.
// Two attempts, not one and not unbounded: the config watcher installs the
// replacement with Swap *before* retiring the old set with Close, so a Load
// that raced a reload can Acquire a set that retired in between — the retry
// then observes the replacement and succeeds. More attempts are useless:
// Acquire only fails on a retired set, and a set left installed past
// retirement can never start succeeding, so the loop must terminate and fall
// through to the nil error path.
func currentPipeline(selCtx *selctx.SelectorCtx) (*profile.Pipeline, func()) {
	for attempt := 0; attempt < 2; attempt++ {
		profiles := scheduler.profiles.Load()
		if profiles == nil {
			return nil, nil
		}
		if pipeline, release, acquired := profiles.Acquire(selCtx); acquired {
			return pipeline, release
		}
		// Retired between Load and Acquire: the reload installed its
		// replacement before retiring, so the next Load observes it.
	}
	return nil, nil
}

func isNoCandidateError(err error) bool {
	status, _ := ret.FromError(err)
	return status != nil && status.Code() == errorcode.ErrorCode_SelectNodesNoRes
}

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
	switch pipeline.Selection {
	case profile.SelectionHighest:
		return selCtx.Nodes()[0]
	case profile.SelectionSpread:
		return spreadSelect(selCtx, pipeline.TopN)
	default:
		return selCtx.LeastRandomSelect(pipeline.TopN)
	}
}

// spreadSelect 实现摊平语义：在评分最高的前 topN 个候选中确定性选取当前运行
// 沙箱数最少的节点，占用相同时保持评分顺序。与 random（top_n 内按分数加权随机）
// 不同，spread 保证打分聚拢时放置仍然向空闲节点摊开。
func spreadSelect(selCtx *selctx.SelectorCtx, topN int) *node.Node {
	var candidates node.NodeList
	if scored := selCtx.LeastScoreNodes(topN); scored.Len() > 0 {
		candidates = make(node.NodeList, 0, scored.Len())
		for i := range scored {
			candidates = append(candidates, scored[i].OrigNode)
		}
	} else {
		candidates = selCtx.LeastNodes(topN)
	}
	var best *node.Node
	for i := range candidates {
		if candidates[i] == nil {
			continue
		}
		if best == nil || candidates[i].MvmNum < best.MvmNum {
			best = candidates[i]
		}
	}
	return best
}

func backoffSelectWithPipeline(selCtx *selctx.SelectorCtx, pipeline *profile.Pipeline) (*node.Node, error) {
	if err := runBackoffFilter(selCtx); err != nil {
		return nil, err
	}
	freezeSnapshot(selCtx, pipeline)
	if err := runProfileFilters(selCtx, pluginKindGuard, pipeline.Guards); err != nil {
		return nil, err
	}
	if err := runProfileFilters(selCtx, pluginKindFilter, pipeline.Filters); err != nil {
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

func freezeSnapshot(selCtx *selctx.SelectorCtx, pipeline *profile.Pipeline) {
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
	// Only gRPC external plugins consume SnapshotNodes; skip the per-attempt
	// O(len(candidates)) node deep clone when the pipeline has none.
	if pipelineReadsSnapshot(pipeline) {
		selCtx.FreezeSnapshot()
	} else {
		selCtx.FreezeMetadata()
	}
}

// pipelineReadsSnapshot reports whether any bound plugin serializes the frozen
// node set, i.e. is a gRPC external plugin (IDs are "filter/grpc/<name>" and
// "score/grpc/<name>", see pkg/selector/plugin/grpcplugin).
func pipelineReadsSnapshot(pipeline *profile.Pipeline) bool {
	if pipeline == nil {
		return false
	}
	for _, binding := range append(append([]profile.FilterPlugin(nil), pipeline.Guards...), pipeline.Filters...) {
		if binding.Selector != nil && strings.HasPrefix(binding.Selector.ID(), "filter/grpc/") {
			return true
		}
	}
	for _, binding := range pipeline.Scores {
		if binding.Selector != nil && strings.HasPrefix(binding.Selector.ID(), "score/grpc/") {
			return true
		}
	}
	return false
}

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

// runProfileFilters 并发执行 Profile 中的一组过滤插件：
// 每个插件独立运行并校验其返回的候选节点，只有被全部插件保留的节点才进入结果；
// 插件失败按 Failure 策略决定 fail-open（放行全部候选）或 fail-closed（直接报错）
// kind 区分 guard/filter 两类执行点，仅用于逐插件耗时指标的标签。
func runProfileFilters(selCtx *selctx.SelectorCtx, kind string, filters []profile.FilterPlugin) error {
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
			pluginStart := time.Now()
			results[index].nodes, results[index].err = filters[index].Selector.Select(selCtx)
			ObservePluginDuration(ProfileNameOf(selCtx), kind, filters[index].Name, time.Since(pluginStart))
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
			pluginStart := time.Now()
			results[index].nodes, results[index].err = selector.Select(selCtx)
			ObservePluginDuration(ProfileNameOf(selCtx), pluginKindScore, scores[index].Name, time.Since(pluginStart))
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
		if errors.Is(result.err, score.ErrNotApplicable) {
			// The plugin's scoring dimension explicitly does not apply to
			// this request:
			// TemplateID): skip it — no scores, no weight, and no failure
			// handling, even for a ForceEnabled plugin. This is the
			// contract-sanctioned alternative to an empty result, which a
			// ForceEnabled plugin must not return.
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
		log.G(selCtx.Ctx).Debugf("runProfileScores profile=%s:%v", selCtx.GetProfileName(), result.String())
	} else {
		log.G(selCtx.Ctx).Infof("runProfileScores profile=%s:%v", selCtx.GetProfileName(), result.Len())
	}
	selCtx.SetNodeScoreList(result)
	if selCtx.Nodes().Len() == 0 {
		return ret.Err(errorcode.ErrorCode_SelectNodesNoRes, ErrNoRes.Error())
	}
	return nil
}
