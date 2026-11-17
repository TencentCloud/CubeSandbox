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

// NodeLabels is the node-label interface: nodes expose their label set for matching.
type NodeLabels interface {
	Labels() map[string]string
}

// NodeSelector reports whether a node satisfies an affinity constraint.
type NodeSelector interface {
	Match(node NodeLabels) bool
}

// PreferredSchedulingTerms scores node-affinity preferences.
type PreferredSchedulingTerms interface {
	Score(node NodeLabels) int64
}

// NewNodeSelector builds a node selector from a list of selector terms.
func NewNodeSelector(ns []NodeSelectorTerm) (NodeSelector, error) {
	wns := &wrapNodeSelector{}
	var err error
	wns.nodeselector, err = NewLazyErrorNodeSelector(ns)
	return wns, err
}

type wrapNodeSelector struct {
	nodeselector *lazyErrorNodeSelector
}

// Match treats an empty selector or an empty node as a match.
func (w *wrapNodeSelector) Match(n NodeLabels) bool {
	if w.nodeselector == nil {
		return true
	}
	if n == nil {
		return true
	}
	return w.nodeselector.Match(labels.Set(n.Labels()))
}

// lazyErrorNodeSelector defers term evaluation until Match is called.
type lazyErrorNodeSelector struct {
	terms []NodeSelectorTerm
}

func NewLazyErrorNodeSelector(ns []NodeSelectorTerm) (*lazyErrorNodeSelector, error) {
	return &lazyErrorNodeSelector{
		terms: ns,
	}, nil
}

// Match succeeds if any term matches (terms are OR-ed).
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

// Match requires every expression in the term to match (expressions are AND-ed).
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

// Match applies a single requirement by operator; Gt/Lt support only the
// memory-size and CPU-cores label keys, compared by resource amount.
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

// NewPreferredSchedulingTerms builds a scorer from k8s preferred scheduling terms.
func NewPreferredSchedulingTerms(terms []v1.PreferredSchedulingTerm) (PreferredSchedulingTerms, error) {
	wrapPreferst := &wrapPreferredSchedulingTerms{}
	var err error
	wrapPreferst.preferredSchedulingTerms, err = nodeaffinity.NewPreferredSchedulingTerms(terms)
	return wrapPreferst, err
}

// Score computes the affinity preference score from node labels; 0 when there
// are no preferences or no labels.
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
