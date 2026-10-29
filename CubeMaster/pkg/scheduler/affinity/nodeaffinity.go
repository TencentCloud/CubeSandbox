// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package affinity

import (
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
)

// NodeLabels 节点标签接口：节点需能提供其标签集合用于匹配
type NodeLabels interface {
	Labels() map[string]string
}

// NodeSelector 节点选择器：判断节点是否满足亲和性约束
type NodeSelector interface {
	Match(node NodeLabels) bool
}

// PreferredSchedulingTerms 优先调度条款：为节点亲和性偏好打分
type PreferredSchedulingTerms interface {
	Score(node NodeLabels) int64
}

// NewNodeSelector 根据节点选择条款列表构造节点选择器
func NewNodeSelector(ns []NodeSelectorTerm) (NodeSelector, error) {
	wns := &wrapNodeSelector{}
	var err error
	wns.nodeselector, err = NewLazyErrorNodeSelector(ns)
	return wns, err
}

type wrapNodeSelector struct {
	nodeselector *lazyErrorNodeSelector
}

// Match 判断节点是否匹配：选择器或节点为空时视为匹配
func (w *wrapNodeSelector) Match(n NodeLabels) bool {
	if w.nodeselector == nil {
		return true
	}
	if n == nil {
		return true
	}
	return w.nodeselector.Match(labels.Set(n.Labels()))
}

// lazyErrorNodeSelector 延迟解析的节点选择器：仅在 Match 时才求值各条款
type lazyErrorNodeSelector struct {
	terms []NodeSelectorTerm
}

func NewLazyErrorNodeSelector(ns []NodeSelectorTerm) (*lazyErrorNodeSelector, error) {
	return &lazyErrorNodeSelector{
		terms: ns,
	}, nil
}

// Match 任一条款匹配即算匹配（条款之间是 OR 关系）
func (ns *lazyErrorNodeSelector) Match(nodeLabels labels.Set) bool {
	if nodeLabels == nil {
		return true
	}
	for _, term := range ns.terms {
		if term.Match(nodeLabels) {
			return true
		}
	}
	return false
}

// Match 条款内的所有表达式必须全部匹配（表达式之间是 AND 关系）
func (term NodeSelectorTerm) Match(nodeLabels labels.Set) bool {
	if len(term.MatchExpressions) == 0 {
		return true
	}

	for _, require := range term.MatchExpressions {
		if !require.Match(nodeLabels) {
			return false
		}
	}

	return true
}

// Match 按操作符匹配单个需求：
// In/NotIn 比较标签值是否在集合中；Exists/DoesNotExist 判断标签是否存在；
// Gt/Lt 仅支持内存大小与 CPU 核数两个标签键，按资源量大小比较
func (require NodeSelectorRequirement) Match(ls labels.Set) bool {
	switch require.Operator {
	case NodeSelectorOpIn:
		if !ls.Has(require.Key) {
			return false
		}
		_, ok := require.Values[ls.Get(require.Key)]
		return ok
	case NodeSelectorOpNotIn:
		if !ls.Has(require.Key) {
			return true
		}
		_, ok := require.Values[ls.Get(require.Key)]
		return !ok
	case NodeSelectorOpExists:
		return ls.Has(require.Key)
	case NodeSelectorOpDoesNotExist:
		return !ls.Has(require.Key)
	case NodeSelectorOpGt, NodeSelectorOpLt:
		if !ls.Has(require.Key) {
			return false
		}
		switch require.Key {
		case constants.AffinityKeyMemorySize, constants.AffinityKeyCPUCores:

			lValue, err := resource.ParseQuantity(ls.Get(require.Key))
			if err != nil {
				return false
			}
			if len(require.Values) != 1 {
				return false
			}

			rValue, err := resource.ParseQuantity(utils.FirstKey(require.Values))
			if err != nil {
				return false
			}
			// Gt 要求节点资源量 >= 阈值，Lt 要求节点资源量 <= 阈值
			return (require.Operator == NodeSelectorOpGt && lValue.Cmp(rValue) >= 0) ||
				(require.Operator == NodeSelectorOpLt && lValue.Cmp(rValue) <= 0)
		default:
			return false
		}
	default:
		return false
	}
}

// wrapPreferredSchedulingTerms 包装 k8s 官方的优先调度条款实现
type wrapPreferredSchedulingTerms struct {
	preferredSchedulingTerms *nodeaffinity.PreferredSchedulingTerms
}

// NewPreferredSchedulingTerms 根据 k8s 优先调度条款列表构造评分器
func NewPreferredSchedulingTerms(terms []v1.PreferredSchedulingTerm) (PreferredSchedulingTerms, error) {
	wrapPreferst := &wrapPreferredSchedulingTerms{}
	var err error
	wrapPreferst.preferredSchedulingTerms, err = nodeaffinity.NewPreferredSchedulingTerms(terms)
	return wrapPreferst, err
}

// Score 按节点标签计算亲和性偏好得分，无偏好或无标签时返回 0
func (w *wrapPreferredSchedulingTerms) Score(n NodeLabels) int64 {
	if w.preferredSchedulingTerms == nil {
		return 0
	}

	labels := n.Labels()
	if len(labels) == 0 {
		return 0
	}

	return w.preferredSchedulingTerms.Score(&v1.Node{ObjectMeta: metav1.ObjectMeta{Labels: labels}})
}
