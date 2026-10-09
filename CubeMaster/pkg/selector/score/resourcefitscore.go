// Copyright (c) 2024 Tencent Inc.
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

// 编译期检查 resourceFitScore 是否完整实现 Selector 接口。
var _ Selector = (*resourceFitScore)(nil)

// resourceFitScore 根据请求放入节点后的资源剩余量和比例失衡程度打分。
type resourceFitScore struct {
	weight           float64
	imbalancePenalty float64
	disable          bool
}

const (
	defaultResourceFitWeight           = 1.0
	defaultResourceFitImbalancePenalty = 0.5
)

// NewResourceFitScore 根据 Profile 插件配置创建资源适配评分插件。
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

// numericProfileArg 将 YAML Args 中的数值转换为 float64。
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

// newResourceFitScore 创建资源适配评分插件。
// 当前使用显式参数，避免依赖尚未确定的 Profile/Registry 配置结构。
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

// Select 为每个候选节点计算请求感知的资源适配分数。
//
// CPU 使用 millicore，内存使用 MB；有效容量和已分配量的计算方式
// 与现有 CPU、内存 Filter 保持一致。
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

// calculateResourceFitScore 计算请求放入节点后的资源适配分数。
//
// cpuCapacity、cpuUsed 和 cpuRequest 必须使用相同单位，例如 millicore；
// memCapacity、memUsed 和 memRequest 必须使用相同单位，例如 MB。
//
// 分数由两部分组成：
//   - packingScore：放入请求后剩余资源越少，分数越高；
//   - imbalancePenalty：CPU 和内存剩余比例差异越大，扣分越多。
//
// 返回值范围为 0～100，分数越高表示节点越适合当前请求。
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

	// 使用减法判断，避免 cpuUsed + cpuRequest 发生整数溢出。
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
