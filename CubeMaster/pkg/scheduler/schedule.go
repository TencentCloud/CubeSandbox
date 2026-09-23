// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"math"
	"math/rand"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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

// scoreNonFiniteWeightWarnInterval bounds Warn spam when a scorer's Weight() is
// NaN/Inf on the create path. The Prometheus counter still increments every time.
const scoreNonFiniteWeightWarnInterval = time.Minute

var (
	scoreNonFiniteWeightTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cube_scheduler_score_non_finite_weight_total",
		Help: "Times runScoreFilter skipped blending a scorer because Weight() was NaN or Inf.",
	}, []string{"scorer"})
	scoreNonFiniteNodeScoreTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cube_scheduler_score_non_finite_score_total",
		Help: "Times runScoreFilter skipped blending a scorer because a returned per-node score was NaN or Inf.",
	}, []string{"scorer"})

	scoreNonFiniteWeightWarnMu   sync.Mutex
	scoreNonFiniteWeightLastWarn = map[string]time.Time{}
	// Test seam: counts Warn emissions after rate limiting (weight + node_score).
	scoreNonFiniteWeightWarnCount atomic.Uint64
)

func Select(selCtx *selctx.SelectorCtx) (nodes *node.Node, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.G(selCtx.Ctx).Fatalf("Select panic:%+v", string(debug.Stack()))
			err = ret.Errorf(errorcode.ErrorCode_MasterInternalError, "Select panic:%s", r)
		}
	}()

	if err := runPreFilter(selCtx); err != nil {
		if shouldSkipBackoffForTemplate(selCtx) {
			return nil, err
		}

		if err = runBackoffFilter(selCtx); err != nil {
			return nil, err
		}
	}

	if err := runFilter(selCtx, scheduler.filter); err != nil {
		if shouldSkipBackoffForTemplate(selCtx) {
			return nil, err
		}

		log.G(selCtx.Ctx).Errorf("scheduler_Select fail,try BackoffSelect")
		return BackoffSelect(selCtx)
	}

	if err := runScoreFilter(selCtx, scheduler.score); err != nil {
		return nil, err
	}

	return selCtx.LeastRandomSelect(config.GetConfig().Scheduler.PrioritySelectNum), nil
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
		// Sample Weight() once before Select so the weight folded into
		// totalPluginWeight matches the multiplier applied to every node score
		// in this pass (avoids disagreeing mid-pass Weight() reads). This does
		// not freeze the whole live-config generation for external_http_score:
		// that plugin may still re-read endpoint/timeout/policy inside Select.
		// Legacy factor/affinity scorers and
		// binpack_score already return Disable()==true at weight==0, so this
		// loop never reaches them. external_http_score keeps Disable()==false at
		// weight==0 (observability / staged inert no-op) and skips the HTTP
		// round-trip inside Select. Negative plugin weights fail config load for
		// every registered scorer; this path still guards NaN/Inf so they cannot
		// poison totalPluginWeight or node scores, while letting live-config
		// scorers emit their own invalid_weight observability via Select.
		// Per-node scores are checked the same way before fold — a single
		// NaN/Inf score would otherwise poison every node's final value via
		// the weighted average and make AllSortByScore order unspecified.
		// FailClosedError is type-based and scheduler-wide: any scorer that
		// returns it aborts the rest of Score (and create) immediately,
		// discarding already-blended contributions from earlier scorers.
		w := f.Weight()
		tmpResult, err := f.Select(selCtx)
		if err != nil {
			if score.IsFailClosed(err) {
				return err
			}
			continue
		}
		if len(tmpResult) == 0 {
			continue
		}
		if math.IsNaN(w) || math.IsInf(w, 0) {
			observeNonFiniteScoreWeight(selCtx, f.ID())
			continue
		}
		nonFiniteScore := false
		for _, n := range tmpResult {
			if math.IsNaN(n.Score) || math.IsInf(n.Score, 0) {
				nonFiniteScore = true
				break
			}
		}
		if nonFiniteScore {
			observeNonFiniteNodeScore(selCtx, f.ID(), w)
			continue
		}
		totalPluginWeight += w
		for _, n := range tmpResult {
			n.Score *= w
			if old, ok := resultMap[n.ID()]; ok {
				old.Score += n.Score
			} else {
				resultMap[n.ID()] = n
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

func observeNonFiniteScoreWeight(selCtx *selctx.SelectorCtx, scorerID string) {
	if scorerID == "" {
		scorerID = "unknown"
	}
	scoreNonFiniteWeightTotal.WithLabelValues(scorerID).Inc()
	observeNonFiniteBlendWarn(selCtx, scorerID, "weight",
		"runScoreFilter: skipping scorer %s with non-finite weight", scorerID)
}

func observeNonFiniteNodeScore(selCtx *selctx.SelectorCtx, scorerID string, weight float64) {
	if scorerID == "" {
		scorerID = "unknown"
	}
	scoreNonFiniteNodeScoreTotal.WithLabelValues(scorerID).Inc()
	// weight is known-finite here; include it so operators do not chase weight: config.
	observeNonFiniteBlendWarn(selCtx, scorerID, "node_score",
		"runScoreFilter: skipping scorer %s with non-finite node score (weight=%g)", scorerID, weight)
}

func observeNonFiniteBlendWarn(selCtx *selctx.SelectorCtx, scorerID, reason, format string, args ...any) {
	warnKey := scorerID + ":" + reason
	now := time.Now()
	scoreNonFiniteWeightWarnMu.Lock()
	last, ok := scoreNonFiniteWeightLastWarn[warnKey]
	if ok && now.Sub(last) < scoreNonFiniteWeightWarnInterval {
		scoreNonFiniteWeightWarnMu.Unlock()
		if selCtx != nil {
			log.G(selCtx.Ctx).Debugf(format, args...)
		}
		return
	}
	scoreNonFiniteWeightLastWarn[warnKey] = now
	scoreNonFiniteWeightWarnMu.Unlock()
	scoreNonFiniteWeightWarnCount.Add(1)
	if selCtx != nil {
		log.G(selCtx.Ctx).Warnf(format, args...)
	}
}

func resetScoreNonFiniteWeightWarnStateForTest() {
	scoreNonFiniteWeightWarnMu.Lock()
	scoreNonFiniteWeightLastWarn = map[string]time.Time{}
	scoreNonFiniteWeightWarnMu.Unlock()
	scoreNonFiniteWeightWarnCount.Store(0)
}
