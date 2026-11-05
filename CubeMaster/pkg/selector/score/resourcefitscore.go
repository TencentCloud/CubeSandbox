// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"errors"
	"fmt"
	"math"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

// Compile-time check that resourceFitScore implements the Selector interface.
var _ Selector = (*resourceFitScore)(nil)

// resourceFitScore scores nodes by the resources left after placing the
// request and by the resulting cpu/mem imbalance.
type resourceFitScore struct {
	weight           float64
	imbalancePenalty float64
	disable          bool
}

const (
	defaultResourceFitWeight           = 1.0
	defaultResourceFitImbalancePenalty = 0.5
)

// NewResourceFitScore creates the resource-fit score plugin from its Profile
// plugin config.
func NewResourceFitScore(
	conf config.SchedulerProfilePluginConf,
) (*resourceFitScore, error) {
	weight := conf.Weight
	if weight == 0 {
		weight = defaultResourceFitWeight
	}
	if weight < 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
		return nil, fmt.Errorf(
			"resource fit score: invalid weight %v",
			weight,
		)
	}

	imbalancePenalty := defaultResourceFitImbalancePenalty

	for name := range conf.Args {
		if name != "imbalance_penalty" {
			return nil, fmt.Errorf(
				"resource fit score: unknown argument %q",
				name,
			)
		}
	}

	if value, ok := conf.Args["imbalance_penalty"]; ok {
		parsed, err := numericProfileArg(value)
		if err != nil {
			return nil, fmt.Errorf(
				"resource fit score: invalid imbalance_penalty: %w",
				err,
			)
		}
		imbalancePenalty = parsed
	}

	if imbalancePenalty < 0 ||
		math.IsNaN(imbalancePenalty) ||
		math.IsInf(imbalancePenalty, 0) {
		return nil, fmt.Errorf(
			"resource fit score: invalid imbalance_penalty %v",
			imbalancePenalty,
		)
	}

	return newResourceFitScore(
		weight,
		imbalancePenalty,
		false,
	), nil
}

// numericProfileArg converts a numeric YAML Args entry to float64.
func numericProfileArg(value any) (float64, error) {
	switch number := value.(type) {
	case float64:
		return number, nil
	case float32:
		return float64(number), nil
	case int:
		return float64(number), nil
	case int32:
		return float64(number), nil
	case int64:
		return float64(number), nil
	case uint:
		return float64(number), nil
	case uint32:
		return float64(number), nil
	case uint64:
		return float64(number), nil
	default:
		return 0, fmt.Errorf(
			"expected a number, got %T",
			value,
		)
	}
}

// newResourceFitScore creates the resource-fit score plugin.
// It uses explicit arguments for now, avoiding a dependency on the
// not-yet-finalized Profile/Registry config shape.
func newResourceFitScore(
	weight float64,
	imbalancePenalty float64,
	disable bool,
) *resourceFitScore {
	return &resourceFitScore{
		weight:           weight,
		imbalancePenalty: imbalancePenalty,
		disable:          disable,
	}
}

func (l *resourceFitScore) ID() string {
	return constants.SelectorScoreID + "/" + "resource_fit_score"
}

func (l *resourceFitScore) String() string {
	return l.ID()
}

func (l *resourceFitScore) Weight() float64 {
	return l.weight
}

func (l *resourceFitScore) Disable() bool {
	return l.disable
}

// Select computes the request-aware resource-fit score for every candidate node.
//
// CPU is in millicores and memory in MB; effective capacity and allocated
// usage follow the same rules as the existing CPU and memory filters.
func (l *resourceFitScore) Select(
	selCtx *selctx.SelectorCtx,
) (node.NodeScoreList, error) {
	if l == nil {
		return nil, errors.New("resourceFitScore is nil")
	}

	if l.Disable() {
		return nil, nil
	}

	if selCtx == nil {
		return nil, errors.New("resourceFitScore: selector context is nil")
	}

	cfg := config.GetConfig()
	if cfg == nil || cfg.Scheduler == nil {
		return nil, errors.New("resourceFitScore: scheduler config is nil")
	}

	cpuq := selCtx.GetResCpuFromCtx()
	memq := selCtx.GetResMemFromCtx()
	if cpuq == nil || memq == nil {
		return nil, errors.New(
			"resourceFitScore: cpu request or memory request is nil",
		)
	}

	cpuRequest := cpuq.MilliValue()
	memRequest := memq.Value() / 1024 / 1024

	inList := selCtx.Nodes()
	nodes := make(node.NodeScoreList, 0, inList.Len())

	for i := range inList {
		currentNode := inList[i]
		if currentNode == nil {
			return nil, errors.New(
				"resourceFitScore: candidate node is nil",
			)
		}

		cpuCapacity := currentNode.QuotaCpu
		cpuUsed := cfg.Scheduler.EffectiveAllocated(
			currentNode.QuotaCpuUsage,
		)

		memCapacity := currentNode.QuotaMem
		memUsed := cfg.Scheduler.EffectiveAllocated(
			currentNode.QuotaMemUsage,
		)

		fitScore := calculateResourceFitScore(
			cpuCapacity,
			cpuUsed,
			cpuRequest,
			memCapacity,
			memUsed,
			memRequest,
			l.imbalancePenalty,
		)

		nodes.Append(&node.NodeScore{
			InsID:    currentNode.ID(),
			Score:    fitScore,
			MvmNum:   currentNode.MvmNum,
			OrigNode: currentNode,
		})
	}

	return nodes, nil
}

// calculateResourceFitScore computes the resource-fit score after placing the
// request on the node.
//
// cpuCapacity, cpuUsed and cpuRequest must share a unit (e.g. millicores);
// memCapacity, memUsed and memRequest must share a unit (e.g. MB).
//
// The score has two parts:
//   - packingScore: the fewer resources remain after placement, the higher;
//   - imbalancePenalty: the larger the gap between the remaining cpu/mem
//     ratios, the larger the deduction.
//
// The result is in [0,100]; a higher score means the node fits the request
// better.
func calculateResourceFitScore(
	cpuCapacity int64,
	cpuUsed int64,
	cpuRequest int64,
	memCapacity int64,
	memUsed int64,
	memRequest int64,
	imbalancePenalty float64,
) float64 {
	if cpuCapacity <= 0 || memCapacity <= 0 {
		return 0
	}

	if cpuUsed < 0 || cpuRequest < 0 ||
		memUsed < 0 || memRequest < 0 {
		return 0
	}

	if math.IsNaN(imbalancePenalty) ||
		math.IsInf(imbalancePenalty, 0) ||
		imbalancePenalty < 0 {
		return 0
	}

	if cpuUsed > cpuCapacity || memUsed > memCapacity {
		return 0
	}

	// Subtraction-based check avoids integer overflow in cpuUsed + cpuRequest.
	if cpuRequest > cpuCapacity-cpuUsed {
		return 0
	}

	if memRequest > memCapacity-memUsed {
		return 0
	}

	cpuRemaining := cpuCapacity - cpuUsed - cpuRequest
	memRemaining := memCapacity - memUsed - memRequest

	cpuRemainingRatio :=
		float64(cpuRemaining) / float64(cpuCapacity)

	memRemainingRatio :=
		float64(memRemaining) / float64(memCapacity)

	averageRemainingRatio :=
		(cpuRemainingRatio + memRemainingRatio) / 2

	packingScore := (1 - averageRemainingRatio) * 100

	imbalance :=
		math.Abs(cpuRemainingRatio-memRemainingRatio) * 100

	score := packingScore - imbalancePenalty*imbalance

	if score < 0 {
		return 0
	}

	if score > 100 {
		return 100
	}

	return score
}
