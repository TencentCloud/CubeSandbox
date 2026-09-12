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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

// 编译期检查 createConcurrencyScore 是否完整实现 Selector 接口。
var _ Selector = (*createConcurrencyScore)(nil)

// createConcurrencyScore 创建并发压力评分插件：
// 节点在途创建请求越少得分越高，用于 burst 场景把创建压力在节点间打散。
// real_time_weighted_average 的创建类因子（realtime_create_num /
// local_create_num）从全局 score.plugin_conf 读取，无法按 Profile 单独配置，
// 本插件以 Profile args 形式提供等效能力，供 burst_balance 等策略按需启用。
type createConcurrencyScore struct {
	weight float64
	// includeLocalCreate 为 true 时把本 Master 记录在途创建（按健康 Master
	// 数折算全局估计）与 Cubelet 上报的 realtime_create_num 叠加。两者在
	// 创建进行期间会对同一请求短暂重复计数，方向上一致地放大高压节点的
	// 惩罚，与 legacy 多因子评分同时启用两类因子的口径相同。
	includeLocalCreate bool
	disable            bool
}

const (
	defaultCreateConcurrencyWeight = 1.0
	// 节点与全局配置均未给出创建并发上限时的兜底值，
	// 与 localcache.CreateConcurrentLimit 的默认一致
	defaultCreateConcurrentLimit = 50
)

// NewCreateConcurrencyScore 根据 Profile 插件配置创建创建并发压力评分插件。
// 支持的 args：
//   - include_local_create（bool，默认 true）：是否叠加本 Master 侧在途创建计数
func NewCreateConcurrencyScore(
	conf config.SchedulerProfilePluginConf,
) (*createConcurrencyScore, error) {
	weight := conf.Weight
	if weight == 0 {
		weight = defaultCreateConcurrencyWeight
	}
	if weight < 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
		return nil, fmt.Errorf(
			"create concurrency score: invalid weight %v",
			weight,
		)
	}

	includeLocalCreate := true
	for name := range conf.Args {
		if name != "include_local_create" {
			return nil, fmt.Errorf(
				"create concurrency score: unknown argument %q",
				name,
			)
		}
	}
	if value, ok := conf.Args["include_local_create"]; ok {
		parsed, err := boolProfileArg(value)
		if err != nil {
			return nil, fmt.Errorf(
				"create concurrency score: invalid include_local_create: %w",
				err,
			)
		}
		includeLocalCreate = parsed
	}

	return &createConcurrencyScore{
		weight:             weight,
		includeLocalCreate: includeLocalCreate,
	}, nil
}

// boolProfileArg 将 YAML Args 中的布尔值转换为 bool。
func boolProfileArg(value any) (bool, error) {
	parsed, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("expected a boolean, got %T", value)
	}
	return parsed, nil
}

func (l *createConcurrencyScore) ID() string {
	return constants.SelectorScoreID + "/" + "create_concurrency_score"
}

func (l *createConcurrencyScore) String() string {
	return l.ID()
}

func (l *createConcurrencyScore) Weight() float64 {
	return l.weight
}

func (l *createConcurrencyScore) Disable() bool {
	return l.disable
}

// Select 为每个候选节点计算创建并发压力分数：在途创建占并发上限比例越低分越高。
// Profile 流水线对 fail-closed 插件强制 [0,100] 且必须覆盖全部候选节点，
// 分数经 clampFactorScore 收敛，候选为 nil 时按 0 分处理而不是中断调度。
func (l *createConcurrencyScore) Select(
	selCtx *selctx.SelectorCtx,
) (node.NodeScoreList, error) {
	if l == nil {
		return nil, errors.New("createConcurrencyScore is nil")
	}

	if l.Disable() {
		return nil, nil
	}

	if selCtx == nil {
		return nil, errors.New("createConcurrencyScore: selector context is nil")
	}

	inList := selCtx.Nodes()
	nodes := make(node.NodeScoreList, 0, inList.Len())
	for i := range inList {
		currentNode := inList[i]
		if currentNode == nil {
			return nil, errors.New(
				"createConcurrencyScore: candidate node is nil",
			)
		}
		nodes.Append(&node.NodeScore{
			InsID:    currentNode.ID(),
			Score:    l.scoreNode(currentNode),
			MvmNum:   currentNode.MvmNum,
			OrigNode: currentNode,
		})
	}
	return nodes, nil
}

// scoreNode 计算单节点创建并发压力分数：
// Cubelet 上报的在途创建数，加上（可选的）本 Master 在途创建按健康
// Master 数折算的全局估计，占创建并发上限的比例越低分越高
func (l *createConcurrencyScore) scoreNode(n *node.Node) float64 {
	limit := createConcurrentLimitOf(n)
	inFlight := n.RealTimeCreateNum
	if l.includeLocalCreate {
		inFlight += localcache.LocalCreateConcurrentLimit(n) * localcache.HealthyMasterNodes()
	}
	return clampFactorScore(100.0 - getReciprocal(inFlight, limit)*100.0)
}

// createConcurrentLimitOf 取节点生效的创建并发上限：
// 与 localcache.CreateConcurrentLimit 同口径（节点上报值与全局配置取大），
// 但全局配置未初始化时回退到节点值，避免空指针
func createConcurrentLimitOf(n *node.Node) int64 {
	if n == nil {
		return defaultCreateConcurrentLimit
	}
	limit := n.CreateConcurrentNum
	if cfg := config.GetConfig(); cfg != nil &&
		cfg.CubeletConf.CreateConcurrentLimit > limit {
		limit = cfg.CubeletConf.CreateConcurrentLimit
	}
	if limit <= 0 {
		limit = defaultCreateConcurrentLimit
	}
	return limit
}
