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

type NodeLabels interface {
	Labels() map[string]string
}

type NodeSelector interface {
	Match(node NodeLabels) bool
	// Terms returns a copy of selector terms for inspection (for example
	// scarce-resource detection). The outer term slice and each
	// MatchExpressions / MatchFields slice are copied; NodeSelectorRequirement.Values
	// maps are shared and must be treated as read-only by callers.
	Terms() []NodeSelectorTerm
}

type PreferredSchedulingTerms interface {
	Score(node NodeLabels) int64
}

func NewNodeSelector(ns []NodeSelectorTerm) (NodeSelector, error) {
	wns := &wrapNodeSelector{}
	var err error
	wns.nodeselector, err = NewLazyErrorNodeSelector(ns)
	return wns, err
}

type wrapNodeSelector struct {
	nodeselector *lazyErrorNodeSelector
}

func (w *wrapNodeSelector) Match(n NodeLabels) bool {
	if w.nodeselector == nil {
		return true
	}
	if n == nil {
		return true
	}
	return w.nodeselector.Match(labels.Set(n.Labels()))
}

func (w *wrapNodeSelector) Terms() []NodeSelectorTerm {
	if w.nodeselector == nil || len(w.nodeselector.terms) == 0 {
		return nil
	}
	return cloneNodeSelectorTerms(w.nodeselector.terms)
}

func cloneNodeSelectorTerms(terms []NodeSelectorTerm) []NodeSelectorTerm {
	out := make([]NodeSelectorTerm, len(terms))
	copy(out, terms)
	for i := range out {
		out[i].MatchExpressions = cloneNodeSelectorRequirements(out[i].MatchExpressions)
		out[i].MatchFields = cloneNodeSelectorRequirements(out[i].MatchFields)
	}
	return out
}

func cloneNodeSelectorRequirements(reqs []NodeSelectorRequirement) []NodeSelectorRequirement {
	if len(reqs) == 0 {
		return nil
	}
	out := make([]NodeSelectorRequirement, len(reqs))
	copy(out, reqs)
	return out
}

// RequiresLabelKey reports whether ns contains a positive requirement for key:
// Exists on key, or In on key with a value in scarceValues (any In on key when
// scarceValues is empty).
func RequiresLabelKey(ns NodeSelector, key string, scarceValues []string) bool {
	if ns == nil || key == "" {
		return false
	}
	for _, term := range ns.Terms() {
		for _, req := range term.MatchExpressions {
			if expressionRequiresLabelKey(req, key, scarceValues) {
				return true
			}
		}
	}
	return false
}

func expressionRequiresLabelKey(req NodeSelectorRequirement, key string, scarceValues []string) bool {
	if req.Key != key {
		return false
	}
	switch req.Operator {
	case NodeSelectorOpExists:
		return true
	case NodeSelectorOpIn:
		if len(scarceValues) == 0 {
			return len(req.Values) > 0
		}
		for _, want := range scarceValues {
			if _, ok := req.Values[want]; ok {
				return true
			}
		}
		return false
	case NodeSelectorOpGt, NodeSelectorOpLt:
		// TODO: Gt/Lt are not treated as positive scarce-resource requirements.
		// Match() only supports these operators on memory-size/cpu-cores; a selector
		// like gpu Gt "0" would not bypass SRA until explicitly implemented.
		return false
	default:
		return false
	}
}

type lazyErrorNodeSelector struct {
	terms []NodeSelectorTerm
}

func NewLazyErrorNodeSelector(ns []NodeSelectorTerm) (*lazyErrorNodeSelector, error) {
	return &lazyErrorNodeSelector{
		terms: ns,
	}, nil
}

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
			return (require.Operator == NodeSelectorOpGt && lValue.Cmp(rValue) >= 0) ||
				(require.Operator == NodeSelectorOpLt && lValue.Cmp(rValue) <= 0)
		default:
			return false
		}
	default:
		return false
	}
}

type wrapPreferredSchedulingTerms struct {
	preferredSchedulingTerms *nodeaffinity.PreferredSchedulingTerms
}

func NewPreferredSchedulingTerms(terms []v1.PreferredSchedulingTerm) (PreferredSchedulingTerms, error) {
	wrapPreferst := &wrapPreferredSchedulingTerms{}
	var err error
	wrapPreferst.preferredSchedulingTerms, err = nodeaffinity.NewPreferredSchedulingTerms(terms)
	return wrapPreferst, err
}

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
