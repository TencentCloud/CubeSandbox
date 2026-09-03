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

func Select(selCtx *selctx.SelectorCtx) (nodes *node.Node, err error) {
	if selCtx == nil {
		return nil, ret.Err(errorcode.ErrorCode_MasterInternalError, "selector context is nil")
	}
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
	freezeSnapshot(selCtx)

	// Mandatory guards are fail-closed by design: a guard failure (including
	// an emptied candidate set) always fails fast and never consults the
	// pipeline's no_candidate policy — backoff below only applies to the
	// optional filters and final selection. The guard re-run inside
	// backoffSelectWithPipeline is the backoff attempt's own safety net on
	// the relaxed candidate set, not a retry of this pass.
	if err := runProfileFilters(selCtx, pipeline.Guards); err != nil {
		return nil, err
	}
	if err := runProfileFilters(selCtx, pipeline.Filters); err != nil {
		legacy := len(pipeline.Guards) == 0
		// Only an empty candidate set may fall through to backoff, on every
		// pipeline — legacy included. A plugin/validation error fails the
		// request; otherwise BackoffSelect would silently rescue a defective
		// plugin whenever the relaxed pool is non-empty.
		if pipeline.NoCandidate != profile.NoCandidateBackoff ||
			(legacy && pipelineHasTemplateGuard(selCtx, pipeline)) ||
			!isNoCandidateError(err) {
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

	selected := selectNode(selCtx, pipeline)
	if selected == nil {
		if pipeline.NoCandidate == profile.NoCandidateBackoff {
			if len(pipeline.Guards) == 0 {
				return BackoffSelect(selCtx)
			}
			return backoffSelectWithPipeline(selCtx, pipeline)
		}
		// selectNode found nothing and the policy does not allow backoff:
		// fail with no-resource instead of returning (nil, nil), which
		// callers would read as a successful selection.
		return nil, ret.Err(errorcode.ErrorCode_SelectNodesNoRes, ErrNoRes.Error())
	}
	return selected, nil
}

func currentPipeline(selCtx *selctx.SelectorCtx) (*profile.Pipeline, func()) {
	for profiles := scheduler.profiles.Load(); profiles != nil; profiles = scheduler.profiles.Load() {
		if pipeline, release, acquired := profiles.Acquire(selCtx); acquired {
			return pipeline, release
		}
	}
	// InitScheduler never populates the singleton's selector lists (it stores
	// a compiled profile.Set instead), so reaching this fallback with both
	// lists empty means Select raced startup: fail closed via the nil
	// pipeline rather than silently scheduling with an unguarded random pick.
	// Tests and legacy embedders that populate the singleton directly still
	// get the permissive path below.
	if len(scheduler.filter) == 0 && len(scheduler.score) == 0 {
		return nil, nil
	}
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
	// spread deliberately aliases random today: both pick uniformly from the
	// top-N scored nodes. Note that a custom profile without an explicit
	// top_n defaults to 1, i.e. deterministic best-node selection.
	return selCtx.LeastRandomSelect(pipeline.TopN)
}

func backoffSelectWithPipeline(selCtx *selctx.SelectorCtx, pipeline *profile.Pipeline) (*node.Node, error) {
	if err := runBackoffFilter(selCtx); err != nil {
		return nil, err
	}
	// The backoff selector replaced the candidate set wholesale, so the
	// snapshot is frozen a second time: RefreezeSnapshot re-arms the
	// idempotent freeze for exactly this whole-pool replacement (never for
	// mere narrowing), producing a new SnapshotVersion and, for gRPC plugins,
	// another sync. This double snapshot on the backoff path is intentional
	// and a known per-request overhead.
	selCtx.RefreezeSnapshot()
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

// runFilter, parallelRunFilters, runScoreFilter and shouldSkipBackoffForTemplate
// are the PRE-profile executor helpers. Select no longer calls them — the
// profile path (runProfileFilters/runProfileScores) replaced them — and
// InitScheduler no longer populates the package globals they read. They are
// retained only for existing tests and out-of-tree embedders; note they
// subtly differ from the profile path (any filter error is fatal here, and
// scorers run sequentially).
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

// runProfileFilters runs every filter plugin of the stage concurrently and
// intersects their results, applying each plugin's failure policy. The whole
// stage always runs to completion: a fail-closed plugin that errors early
// does NOT short-circuit the others, so a slow sibling bounds the stage's
// latency (fail-fast applies to the request outcome, not to elapsed time).
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
	// The stage deliberately runs every plugin to completion, so no derived
	// cancellation context is needed; a plain group avoids implying one.
	var eg errgroup.Group
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

func parallelRunFilters(selCtx *selctx.SelectorCtx, filters []filter.Selector) (node.NodeList, error) {
	// The stage deliberately runs every plugin to completion, so no derived
	// cancellation context is needed; a plain group avoids implying one.
	var eg errgroup.Group
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

func runScoreFilter(selCtx *selctx.SelectorCtx, scores []score.Selector) error {
	bindings := make([]profile.ScorePlugin, 0, len(scores))
	for _, selector := range scores {
		bindings = append(bindings, profile.ScorePlugin{
			Name: selector.ID(), Selector: selector, Weight: selector.Weight(), Failure: profile.ScoreSkip,
		})
	}
	return runProfileScores(selCtx, bindings)
}

// runProfileScores runs the stage's scorers concurrently and aggregates
// weighted scores. Like runProfileFilters, the stage runs to completion even
// after a fail-closed failure is already determined.
func runProfileScores(selCtx *selctx.SelectorCtx, scores []profile.ScorePlugin) error {
	if len(scores) == 0 {
		return nil
	}
	type scoreResult struct {
		nodes node.NodeScoreList
		err   error
		skip  bool
		// defect marks defect-class failures (a panic or a nil binding) as
		// opposed to transport/invocation errors: a defective plugin must
		// fail closed even under the default-score policy.
		defect bool
	}
	results := make([]scoreResult, len(scores))
	// The stage deliberately runs every plugin to completion, so no derived
	// cancellation context is needed; a plain group avoids implying one.
	var eg errgroup.Group
	for index := range scores {
		index := index
		eg.Go(func() (err error) {
			defer func() {
				if recovered := recover(); recovered != nil {
					results[index].err = fmt.Errorf("score plugin %q panic: %v", scores[index].Name, recovered)
					results[index].defect = true
				}
			}()
			selector := scores[index].Selector
			if selector == nil {
				results[index].err = fmt.Errorf("score plugin %q is nil", scores[index].Name)
				results[index].defect = true
				return nil
			}
			// The scorer-level Disable() switch is honored for every binding,
			// profile-compiled ones included: the expr/gRPC plugin selectors
			// report Disable()==false unconditionally, and a disabled built-in
			// go scorer self-gates to an empty result, which would otherwise
			// hard-fail the completeness check on every profile request.
			if selector.Disable() {
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
		// validationErr marks defect-class errors: malformed output caught by
		// the validation below, plus panics/nil bindings flagged in the
		// goroutine — as opposed to a transport/invocation failure from
		// calling the plugin.
		validationErr := result.defect
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
				// An empty result from a built-in go scorer means "not
				// applicable" (e.g. affinity_score when the request has no
				// node-preference affinity); mirror the legacy path and skip
				// the plugin instead of failing or substituting default
				// scores. Partial coverage (0 < seen < candidates) stays an
				// error.
				if !(len(seen) == 0 && binding.AllowEmpty) {
					result.err = fmt.Errorf("score plugin %q returned %d scores for %d candidates", binding.Name, len(seen), len(candidates))
				}
			}
			validationErr = result.err != nil
		}
		if result.err != nil {
			switch binding.Failure {
			case profile.ScoreFailClosed:
				return ret.Err(errorcode.ErrorCode_MasterInternalError, result.err.Error())
			case profile.ScoreDefaultScore:
				if validationErr {
					// default-score absorbs transport/invocation failures
					// only. A defect-class failure (malformed output, panic,
					// nil binding) means the plugin is defective: fail closed
					// rather than mask the defect behind a constant score
					// for every candidate.
					return ret.Err(errorcode.ErrorCode_MasterInternalError, result.err.Error())
				}
				// When no other binding contributes real scores this request,
				// the substitution silently degenerates the ranking to
				// candidate order — log it louder than a partial degradation.
				live := 0
				for j := range results {
					if j != index && !results[j].skip && results[j].err == nil {
						live++
					}
				}
				if live == 0 {
					log.G(selCtx.Ctx).Errorf("scheduler score %q is the only active scorer and failed; "+
						"substituting default score %.2f degenerates the ranking to candidate order for this request: %v",
						binding.Name, binding.DefaultScore, result.err)
				} else {
					log.G(selCtx.Ctx).Warnf("scheduler score %q failed; using default score %.2f: %v", binding.Name, binding.DefaultScore, result.err)
				}
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
		if weight == 0 && binding.Selector != nil {
			// Compile-time weight validation makes this fallback unreachable
			// for profile-compiled bindings; it exists for hand-built ones.
			// Guard on Selector: a nil selector already recorded an error
			// above, and dereferencing it here would panic instead.
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
