// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sim

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/profile"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/backofffilter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin/expr"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin/grpcplugin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/postscore"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/prefilter"
)

// perf.go implements schedsim's performance mode: the same request replay as
// the quality engine, but every scheduling decision is driven through a
// sim-side staged replica of scheduler.Select so the wall-clock cost of each
// pipeline stage (prefilter / guards / filter / score / pick) is measurable
// without modifying the scheduler core.
//
// Fidelity contract: selectStaged replicates scheduler.Select (schedule.go)
// stage by stage — same plugins (compiled from the same config via
// profile.Compile), same parallel fan-out, same intersection/aggregation
// semantics, same backoff and no-candidate policies. The heavy validation of
// plugin results (duplicate-id / non-candidate detection) is kept only where
// it changes outcomes; the sim trusts the builtin plugins it compiles itself.
// engine_test.go cross-checks that quality and performance modes produce
// identical placement-quality summaries on deterministic scenarios.

// PerfStageKeys lists the measured pipeline stages in execution order.
// "total" is the whole decision (sum of the stages plus routing/lease
// bookkeeping); "prefilter" includes the candidate-snapshot freeze.
var PerfStageKeys = []string{"prefilter", "guards", "filter", "score", "pick", "total"}

// StageStats summarizes one stage's per-request wall-clock cost (ms).
type StageStats struct {
	Count  int64   `json:"count"`
	MeanMs float64 `json:"mean_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
}

// PerfSummary is one round's (or an aggregated campaign's) performance record.
type PerfSummary struct {
	// Stages maps PerfStageKeys to per-request latency stats.
	Stages map[string]*StageStats `json:"stages"`
	// WallSeconds is the real-clock duration of the event replay.
	WallSeconds float64 `json:"wall_seconds"`
	// ThroughputRPS is requests (success + failure) per wall second — the
	// standalone scheduling-core throughput under zero queueing.
	ThroughputRPS float64 `json:"throughput_rps"`
	// Overhead is the schedsim process's own CPU/memory cost over the replay
	// window (procfs + runtime.ReadMemStats, see procoverhead.go).
	Overhead *ProcessOverhead `json:"overhead,omitempty"`
}

// stageTimes carries one request's per-stage latency in milliseconds.
type stageTimes struct {
	prefilter float64
	guards    float64
	filter    float64
	score     float64
	pick      float64
	total     float64
}

func (st *stageTimes) byKey(key string) float64 {
	switch key {
	case "prefilter":
		return st.prefilter
	case "guards":
		return st.guards
	case "filter":
		return st.filter
	case "score":
		return st.score
	case "pick":
		return st.pick
	default:
		return st.total
	}
}

// stageSamples accumulates per-request stage timings of one round.
type stageSamples struct {
	byStage map[string][]float64
}

func newStageSamples() *stageSamples {
	s := &stageSamples{byStage: make(map[string][]float64, len(PerfStageKeys))}
	for _, k := range PerfStageKeys {
		s.byStage[k] = nil
	}
	return s
}

func (s *stageSamples) add(st stageTimes) {
	for _, k := range PerfStageKeys {
		s.byStage[k] = append(s.byStage[k], st.byKey(k))
	}
}

// summarize folds the samples into per-stage percentiles. wall is the
// replay's real-clock duration; requests is the number of scheduling
// decisions (success + failure).
func (s *stageSamples) summarize(wall time.Duration, requests int) *PerfSummary {
	perf := &PerfSummary{
		Stages:      make(map[string]*StageStats, len(PerfStageKeys)),
		WallSeconds: wall.Seconds(),
	}
	if wall > 0 {
		perf.ThroughputRPS = float64(requests) / wall.Seconds()
	}
	for _, k := range PerfStageKeys {
		vals := s.byStage[k]
		st := &StageStats{
			Count:  int64(len(vals)),
			MeanMs: meanOf(vals),
			P50Ms:  Percentile(vals, 50),
			P95Ms:  Percentile(vals, 95),
			P99Ms:  Percentile(vals, 99),
		}
		perf.Stages[k] = st
	}
	return perf
}

// AggregatePerf pools per-round perf summaries into a campaign-level summary:
// stage percentiles are the mean of per-round values (percentiles do not
// compose; the mean-of-rounds is the cross-seed central estimate), wall time
// sums, and throughput is recomputed from the totals.
func AggregatePerf(rounds []*RoundResult, totalRequests int) *PerfSummary {
	var wall float64
	var present int
	for _, r := range rounds {
		if r.Perf != nil {
			wall += r.Perf.WallSeconds
			present++
		}
	}
	if present == 0 {
		return nil
	}
	out := &PerfSummary{Stages: make(map[string]*StageStats, len(PerfStageKeys)), WallSeconds: wall}
	if wall > 0 {
		out.ThroughputRPS = float64(totalRequests*present) / wall
	}
	for _, k := range PerfStageKeys {
		st := &StageStats{}
		var n int
		for _, r := range rounds {
			if r.Perf == nil || r.Perf.Stages[k] == nil {
				continue
			}
			rs := r.Perf.Stages[k]
			st.Count += rs.Count
			st.MeanMs += rs.MeanMs
			st.P50Ms += rs.P50Ms
			st.P95Ms += rs.P95Ms
			st.P99Ms += rs.P99Ms
			n++
		}
		if n > 0 {
			st.MeanMs /= float64(n)
			st.P50Ms /= float64(n)
			st.P95Ms /= float64(n)
			st.P99Ms /= float64(n)
		}
		out.Stages[k] = st
	}
	out.Overhead = aggregateOverhead(rounds, wall)
	return out
}

// aggregateOverhead pools per-round process overhead: CPU seconds sum across
// rounds, average cores recompute from the pooled totals (wall already sums),
// peak RSS takes the max (it is a high-water mark), and the Go heap gauges
// average over the rounds that reported them.
func aggregateOverhead(rounds []*RoundResult, wall float64) *ProcessOverhead {
	var out ProcessOverhead
	var present int
	for _, r := range rounds {
		if r.Perf == nil || r.Perf.Overhead == nil {
			continue
		}
		oh := r.Perf.Overhead
		out.CPUSeconds += oh.CPUSeconds
		if oh.PeakRSSMiB > out.PeakRSSMiB {
			out.PeakRSSMiB = oh.PeakRSSMiB
		}
		out.HeapAllocMiB += oh.HeapAllocMiB
		out.HeapSysMiB += oh.HeapSysMiB
		present++
	}
	if present == 0 {
		return nil
	}
	out.HeapAllocMiB /= float64(present)
	out.HeapSysMiB /= float64(present)
	if wall > 0 {
		out.AvgCPUCores = out.CPUSeconds / wall
	}
	return &out
}

func meanOf(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// perfDriver executes the scheduling pipeline stage by stage with per-stage
// wall-clock timing. It owns a privately compiled profile.Set (built from the
// same config the scheduler core initialized from) so stage boundaries are
// observable without touching scheduler internals.
type perfDriver struct {
	set     *profile.Set
	pre     filter.Selector
	backoff filter.Selector
	post    postscore.Selector
}

// newPerfDriver compiles the pipeline exactly the way scheduler.InitScheduler
// does — same registry providers, same profile.Compile — minus the parts the
// driver never uses (config watchers, async score loop, gauge collectors).
func newPerfDriver(ctx context.Context) (*perfDriver, error) {
	registry := plugin.NewRegistry()
	if err := plugin.RegisterBuiltins(registry); err != nil {
		return nil, fmt.Errorf("sim perf: register builtins: %w", err)
	}
	if err := plugin.ApplyGoExtensions(registry); err != nil {
		return nil, fmt.Errorf("sim perf: apply go extensions: %w", err)
	}
	if err := registry.RegisterFilterProvider(plugin.TypeExpression, expr.NewFilter); err != nil {
		return nil, fmt.Errorf("sim perf: register expr filter: %w", err)
	}
	if err := registry.RegisterScoreProvider(plugin.TypeExpression, expr.NewScore); err != nil {
		return nil, fmt.Errorf("sim perf: register expr score: %w", err)
	}
	if err := registry.RegisterFilterProvider(plugin.TypeGRPC, grpcplugin.NewFilter); err != nil {
		return nil, fmt.Errorf("sim perf: register grpc filter: %w", err)
	}
	if err := registry.RegisterScoreProvider(plugin.TypeGRPC, grpcplugin.NewScore); err != nil {
		return nil, fmt.Errorf("sim perf: register grpc score: %w", err)
	}
	set, err := profile.Compile(ctx, config.GetConfig(), registry)
	if err != nil {
		return nil, fmt.Errorf("sim perf: compile profiles: %w", err)
	}
	return &perfDriver{
		set:     set,
		pre:     prefilter.NewPreFilter(),
		backoff: backofffilter.NewBackoffFilter(),
		post:    postscore.NewSelector(),
	}, nil
}

// close releases external plugin connections held by the compiled set.
func (d *perfDriver) close() {
	if d.set != nil {
		_ = d.set.Close()
	}
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t)) / float64(time.Millisecond)
}

// selectStaged replicates scheduler.Select with per-stage timing. It must stay
// semantically aligned with schedule.go; when in doubt, prefer the scheduler
// behavior over a shortcut.
func (d *perfDriver) selectStaged(selCtx *selctx.SelectorCtx) (selected *node.Node, st stageTimes, err error) {
	startAll := time.Now()
	defer func() {
		if r := recover(); r != nil {
			selected = nil
			err = ret.Errorf(errorcode.ErrorCode_MasterInternalError, "Select panic:%v", r)
		}
		st.total = msSince(startAll)
	}()

	pipeline, release, acquired := d.set.Acquire(selCtx)
	if !acquired || pipeline == nil {
		return nil, st, ret.Err(errorcode.ErrorCode_MasterInternalError, "scheduler profile is not initialized")
	}
	defer release()
	selCtx.SetProfileName(pipeline.Name)

	legacy := len(pipeline.Guards) == 0

	// Stage: prefilter (candidate enumeration) + snapshot freeze.
	t := time.Now()
	preErr := d.runPreFilter(selCtx)
	st.prefilter += msSince(t)
	if preErr != nil {
		if pipeline.NoCandidate != profile.NoCandidateBackoff ||
			(legacy && d.hasTemplateGuard(selCtx, pipeline)) ||
			(!legacy && !isNoCandidateError(preErr)) {
			return nil, st, preErr
		}
		if legacy {
			t = time.Now()
			if err := d.runBackoff(selCtx); err != nil {
				st.prefilter += msSince(t)
				return nil, st, err
			}
			st.prefilter += msSince(t)
		} else {
			return d.backoffWithPipeline(selCtx, pipeline, st, startAll)
		}
	}
	t = time.Now()
	freezeSnapshotFacts(selCtx)
	st.prefilter += msSince(t)

	// Stage: guards (mandatory safety/capacity filters).
	t = time.Now()
	if err := d.runFilters(selCtx, pipeline.Guards); err != nil {
		st.guards += msSince(t)
		return nil, st, err
	}
	st.guards += msSince(t)

	// Stage: optional filters.
	t = time.Now()
	filterErr := d.runFilters(selCtx, pipeline.Filters)
	st.filter += msSince(t)
	if filterErr != nil {
		if pipeline.NoCandidate != profile.NoCandidateBackoff ||
			(legacy && d.hasTemplateGuard(selCtx, pipeline)) ||
			(!legacy && !isNoCandidateError(filterErr)) {
			return nil, st, filterErr
		}
		if legacy {
			return d.legacyBackoffPick(selCtx, st, startAll)
		}
		return d.backoffWithPipeline(selCtx, pipeline, st, startAll)
	}

	// Stage: scoring.
	t = time.Now()
	if err := d.runScores(selCtx, pipeline.Scores); err != nil {
		st.score += msSince(t)
		return nil, st, err
	}
	st.score += msSince(t)

	// Stage: final pick.
	t = time.Now()
	selected = pickNode(selCtx, pipeline)
	st.pick += msSince(t)
	if selected == nil && pipeline.NoCandidate == profile.NoCandidateBackoff {
		if legacy {
			return d.legacyBackoffPick(selCtx, st, startAll)
		}
		return d.backoffWithPipeline(selCtx, pipeline, st, startAll)
	}
	return selected, st, nil
}

// legacyBackoffPick replicates schedule.go's legacy fallback: BackoffSelect
// re-enumerates candidates with relaxed conditions and picks one uniformly at
// random, without any scoring.
func (d *perfDriver) legacyBackoffPick(selCtx *selctx.SelectorCtx, st stageTimes, startAll time.Time) (*node.Node, stageTimes, error) {
	t := time.Now()
	if err := d.runBackoff(selCtx); err != nil {
		st.prefilter += msSince(t)
		st.total = msSince(startAll)
		return nil, st, err
	}
	st.prefilter += msSince(t)
	st.pick += 0 // the legacy backoff pick is O(1); fold its cost into total
	st.total = msSince(startAll)
	return selCtx.Nodes()[rand.Intn(selCtx.Nodes().Len())], st, nil
}

// backoffWithPipeline replicates backoffSelectWithPipeline: relaxed
// re-enumeration, then the full guard/filter/score/pick pipeline again. Stage
// timings accumulate onto the stages that ran before the fallback.
func (d *perfDriver) backoffWithPipeline(selCtx *selctx.SelectorCtx, pipeline *profile.Pipeline, st stageTimes, startAll time.Time) (*node.Node, stageTimes, error) {
	t := time.Now()
	if err := d.runBackoff(selCtx); err != nil {
		st.prefilter += msSince(t)
		st.total = msSince(startAll)
		return nil, st, err
	}
	st.prefilter += msSince(t)

	t = time.Now()
	freezeSnapshotFacts(selCtx)
	st.prefilter += msSince(t) // freeze folds into prefilter

	t = time.Now()
	if err := d.runFilters(selCtx, pipeline.Guards); err != nil {
		st.guards += msSince(t)
		st.total = msSince(startAll)
		return nil, st, err
	}
	st.guards += msSince(t)

	t = time.Now()
	if err := d.runFilters(selCtx, pipeline.Filters); err != nil {
		st.filter += msSince(t)
		st.total = msSince(startAll)
		return nil, st, err
	}
	st.filter += msSince(t)

	t = time.Now()
	if err := d.runScores(selCtx, pipeline.Scores); err != nil {
		st.score += msSince(t)
		st.total = msSince(startAll)
		return nil, st, err
	}
	st.score += msSince(t)

	t = time.Now()
	selected := pickNode(selCtx, pipeline)
	st.pick += msSince(t)
	st.total = msSince(startAll)
	if selected == nil {
		return nil, st, ret.Err(errorcode.ErrorCode_SelectNodesNoRes, "no more resource")
	}
	return selected, st, nil
}

func (d *perfDriver) runPreFilter(selCtx *selctx.SelectorCtx) error {
	result, err := d.pre.Select(selCtx)
	if err != nil {
		return ret.Err(errorcode.ErrorCode_SelectNodesFailed, err.Error())
	}
	selCtx.SetNodes(result)
	if selCtx.Nodes().Len() == 0 {
		return ret.Err(errorcode.ErrorCode_SelectNodesNoRes, "no more resource")
	}
	return nil
}

func (d *perfDriver) runBackoff(selCtx *selctx.SelectorCtx) error {
	result, err := d.backoff.Select(selCtx)
	if err != nil {
		return ret.Err(errorcode.ErrorCode_SelectNodesFailed, err.Error())
	}
	selCtx.SetNodes(result)
	if selCtx.Nodes().Len() == 0 {
		return ret.Err(errorcode.ErrorCode_SelectNodesNoRes, "no more resource")
	}
	return nil
}

// hasTemplateGuard replicates pipelineHasTemplateGuard: with the
// template_locality filter active, locality is a hard constraint and the
// relaxed backoff path must not engage.
func (d *perfDriver) hasTemplateGuard(selCtx *selctx.SelectorCtx, pipeline *profile.Pipeline) bool {
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

func isNoCandidateError(err error) bool {
	status, _ := ret.FromError(err)
	return status != nil && status.Code() == errorcode.ErrorCode_SelectNodesNoRes
}

// runFilters replicates runProfileFilters: plugins run in parallel and a
// candidate must be kept by every plugin. fail-open plugins pass all
// candidates on error; fail-closed plugins abort the request.
func (d *perfDriver) runFilters(selCtx *selctx.SelectorCtx, filters []profile.FilterPlugin) error {
	if len(filters) == 0 {
		return nil
	}
	results := make([]node.NodeList, len(filters))
	errs := make([]error, len(filters))
	eg, _ := errgroup.WithContext(selCtx.Ctx)
	for index := range filters {
		index := index
		eg.Go(func() error {
			defer func() {
				if recovered := recover(); recovered != nil {
					errs[index] = fmt.Errorf("filter plugin %q panic: %v", filters[index].Name, recovered)
				}
			}()
			if filters[index].Selector == nil {
				errs[index] = fmt.Errorf("filter plugin %q is nil", filters[index].Name)
				return nil
			}
			results[index], errs[index] = filters[index].Selector.Select(selCtx)
			return nil
		})
	}
	_ = eg.Wait()

	counts := make(map[string]int, len(selCtx.Nodes()))
	for index := range filters {
		if errs[index] != nil {
			if filters[index].Failure == profile.FilterFailOpen {
				for _, candidate := range selCtx.Nodes() {
					counts[candidate.ID()]++
				}
				continue
			}
			return ret.Err(errorcode.ErrorCode_MasterInternalError, errs[index].Error())
		}
		for _, candidate := range results[index] {
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
		return ret.Err(errorcode.ErrorCode_SelectNodesNoRes, "no more resource")
	}
	selCtx.SetNodes(result)
	return nil
}

// runScores replicates runProfileScores: plugins run in parallel; per-plugin
// errors follow the binding's failure policy (skip / default-score /
// fail-closed); weighted scores aggregate, normalize by total weight and sort
// descending before the final pick.
func (d *perfDriver) runScores(selCtx *selctx.SelectorCtx, scores []profile.ScorePlugin) error {
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
		eg.Go(func() error {
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

	candidates := make(map[string]*node.Node, len(selCtx.Nodes()))
	for _, candidate := range selCtx.Nodes() {
		if candidate == nil {
			continue
		}
		candidates[candidate.ID()] = candidate
	}
	totalWeight := 0.0
	aggregated := make(map[string]*node.NodeScore, len(candidates))
	for index, result := range results {
		binding := scores[index]
		if result.skip {
			continue
		}
		if result.err == nil {
			for _, scored := range result.nodes {
				if scored == nil || scored.ID() == "" {
					result.err = fmt.Errorf("score plugin %q returned an invalid score entry", binding.Name)
					break
				}
				if _, exists := candidates[scored.ID()]; !exists {
					result.err = fmt.Errorf("score plugin %q returned non-candidate node %q", binding.Name, scored.ID())
					break
				}
				if math.IsNaN(scored.Score) || math.IsInf(scored.Score, 0) {
					result.err = fmt.Errorf("score plugin %q returned invalid score for node %q", binding.Name, scored.ID())
					break
				}
				if binding.ForceEnabled && (scored.Score < 0 || scored.Score > 100) {
					result.err = fmt.Errorf("score plugin %q returned %v for node %q outside [0,100]", binding.Name, scored.Score, scored.ID())
					break
				}
			}
		}
		if result.err != nil {
			switch binding.Failure {
			case profile.ScoreFailClosed:
				return ret.Err(errorcode.ErrorCode_MasterInternalError, result.err.Error())
			case profile.ScoreDefaultScore:
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
			return ret.Err(errorcode.ErrorCode_SelectNodesNoRes, "no more resource")
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
	if d.post != nil {
		_ = d.post.PostedScore(selCtx, aggregated)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Score > result[j].Score })
	selCtx.SetNodeScoreList(result)
	if selCtx.Nodes().Len() == 0 {
		return ret.Err(errorcode.ErrorCode_SelectNodesNoRes, "no more resource")
	}
	return nil
}

// pickNode replicates selectNode: highest takes the top-scored candidate,
// spread takes the least-loaded node among the top-N, anything else is the
// score-weighted random pick.
func pickNode(selCtx *selctx.SelectorCtx, pipeline *profile.Pipeline) *node.Node {
	if selCtx == nil || pipeline == nil || selCtx.Nodes().Len() == 0 {
		return nil
	}
	switch pipeline.Selection {
	case profile.SelectionHighest:
		return selCtx.Nodes()[0]
	case profile.SelectionSpread:
		return spreadPick(selCtx, pipeline.TopN)
	default:
		return selCtx.LeastRandomSelect(pipeline.TopN)
	}
}

// spreadPick replicates spreadSelect: among the top-N scored candidates pick
// the node running the fewest sandboxes, keeping score order on ties.
func spreadPick(selCtx *selctx.SelectorCtx, topN int) *node.Node {
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

// freezeSnapshotFacts replicates freezeSnapshot (schedule.go) minus the
// snapshot-storage branch, which sim requests never enable
// (RequestResource.EnforceSnapshotStorage stays false).
func freezeSnapshotFacts(selCtx *selctx.SelectorCtx) {
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
		facts[candidate.ID()] = value
	}
	selCtx.SetSnapshotFacts(facts)
	selCtx.FreezeSnapshot()
}
