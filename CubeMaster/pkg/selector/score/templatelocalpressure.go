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

// 编译期检查 templateLocalPressureScore 是否完整实现 Selector 接口。
var _ Selector = (*templateLocalPressureScore)(nil)

// templateLocalPressureScore 同模板创建压力评分插件：
// 同模板在该节点上的在途创建数越少得分越高，用于 template_reuse 策略
// 在"模板本地命中"之外增加"同模板创建压力分散"维度，避免同模板突发创建
// 在单个副本节点上排队（羊群）。计数来自创建路径登记的
// localcache (node, template) 在途计数（见 IncrNodeTemplateCreate）。
type templateLocalPressureScore struct {
	weight  float64
	disable bool
}

const defaultTemplateLocalPressureWeight = 1.0

// NewTemplateLocalPressureScore 根据 Profile 插件配置创建同模板创建压力评分插件。
// 当前不支持 args，配置未知参数时报错而不是静默忽略。
func NewTemplateLocalPressureScore(
	conf config.SchedulerProfilePluginConf,
) (*templateLocalPressureScore, error) {
	weight := conf.Weight
	if weight == 0 {
		weight = defaultTemplateLocalPressureWeight
	}
	if weight < 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
		return nil, fmt.Errorf(
			"template local pressure score: invalid weight %v",
			weight,
		)
	}
	for name := range conf.Args {
		return nil, fmt.Errorf(
			"template local pressure score: unknown argument %q",
			name,
		)
	}
	return &templateLocalPressureScore{weight: weight}, nil
}

func (l *templateLocalPressureScore) ID() string {
	return constants.SelectorScoreID + "/" + "template_local_pressure"
}

func (l *templateLocalPressureScore) String() string {
	return l.ID()
}

func (l *templateLocalPressureScore) Weight() float64 {
	return l.weight
}

func (l *templateLocalPressureScore) Disable() bool {
	return l.disable
}

// Select 为每个候选节点计算同模板创建压力分数。
// 请求不带模板 ID 时没有同模板压力维度可评估，返回空列表跳过本插件
// （Profile 流水线会忽略空结果，不参与加权聚合）。
func (l *templateLocalPressureScore) Select(
	selCtx *selctx.SelectorCtx,
) (node.NodeScoreList, error) {
	if l == nil {
		return nil, errors.New("templateLocalPressureScore is nil")
	}

	if l.Disable() {
		return nil, nil
	}

	if selCtx == nil {
		return nil, errors.New(
			"templateLocalPressureScore: selector context is nil",
		)
	}

	if selCtx.ReqRes == nil || selCtx.ReqRes.TemplateID == "" {
		return nil, nil
	}
	templateID := selCtx.ReqRes.TemplateID

	inList := selCtx.Nodes()
	nodes := make(node.NodeScoreList, 0, inList.Len())
	for i := range inList {
		currentNode := inList[i]
		if currentNode == nil {
			return nil, errors.New(
				"templateLocalPressureScore: candidate node is nil",
			)
		}
		nodes.Append(&node.NodeScore{
			InsID:    currentNode.ID(),
			Score:    templateLocalPressureNodeScore(templateID, currentNode),
			MvmNum:   currentNode.MvmNum,
			OrigNode: currentNode,
		})
	}
	return nodes, nil
}

// templateLocalPressureNodeScore 计算单节点同模板创建压力分数：
// 同模板在途创建的全局估计占节点创建并发上限的比例越低分越高，
// 分数经 clampFactorScore 收敛到 [0,100]
func templateLocalPressureNodeScore(templateID string, n *node.Node) float64 {
	limit := createConcurrentLimitOf(n)
	load := localcache.NodeTemplateCreateLoad(n.ID(), templateID)
	return clampFactorScore(100.0 - getReciprocal(load, limit)*100.0)
}
