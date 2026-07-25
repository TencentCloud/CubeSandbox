// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package affinity

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

func TestRequiresLabelKeyExists(t *testing.T) {
	ns, err := NewNodeSelector([]NodeSelectorTerm{{
		MatchExpressions: []NodeSelectorRequirement{{
			Key:      "gpu",
			Operator: NodeSelectorOpExists,
		}},
	}})
	if err != nil {
		t.Fatalf("NewNodeSelector: %v", err)
	}
	if !RequiresLabelKey(ns, "gpu", []string{"true"}) {
		t.Fatal("expected gpu Exists to require gpu key")
	}
}

func TestRequiresLabelKeyWithClusterIDAnded(t *testing.T) {
	ns, err := NewNodeSelector([]NodeSelectorTerm{{
		MatchExpressions: []NodeSelectorRequirement{
			{
				Key:      constants.AffinityKeyClusterID,
				Operator: NodeSelectorOpIn,
				Values:   map[string]any{"cluster-a": struct{}{}},
			},
			{
				Key:      "gpu",
				Operator: NodeSelectorOpExists,
			},
		},
	}})
	if err != nil {
		t.Fatalf("NewNodeSelector: %v", err)
	}
	if !RequiresLabelKey(ns, "gpu", []string{"true"}) {
		t.Fatal("expected gpu Exists with cluster-id AND to require gpu key")
	}
}

func TestRequiresLabelKeyInOverlapsScarceValues(t *testing.T) {
	ns, err := NewNodeSelector([]NodeSelectorTerm{{
		MatchExpressions: []NodeSelectorRequirement{{
			Key:      "gpu",
			Operator: NodeSelectorOpIn,
			Values:   map[string]any{"h100": struct{}{}},
		}},
	}})
	if err != nil {
		t.Fatalf("NewNodeSelector: %v", err)
	}
	if !RequiresLabelKey(ns, "gpu", []string{"a100", "h100"}) {
		t.Fatal("expected gpu In [h100] to require gpu when h100 is a scarce value")
	}
	if RequiresLabelKey(ns, "gpu", []string{"a100"}) {
		t.Fatal("gpu In [h100] should not require gpu when h100 is not scarce")
	}
}

func TestRequiresLabelKeyDoesNotExist(t *testing.T) {
	ns, err := NewNodeSelector([]NodeSelectorTerm{{
		MatchExpressions: []NodeSelectorRequirement{{
			Key:      "gpu",
			Operator: NodeSelectorOpDoesNotExist,
		}},
	}})
	if err != nil {
		t.Fatalf("NewNodeSelector: %v", err)
	}
	if RequiresLabelKey(ns, "gpu", []string{"true"}) {
		t.Fatal("gpu DoesNotExist should not count as requiring gpu")
	}
}

type stubNodeSelector struct {
	terms []NodeSelectorTerm
}

func (s *stubNodeSelector) Match(_ NodeLabels) bool { return true }

func (s *stubNodeSelector) Terms() []NodeSelectorTerm { return s.terms }

func TestRequiresLabelKeyCustomImplementation(t *testing.T) {
	ns := &stubNodeSelector{terms: []NodeSelectorTerm{{
		MatchExpressions: []NodeSelectorRequirement{{
			Key:      "gpu",
			Operator: NodeSelectorOpExists,
		}},
	}}}
	if !RequiresLabelKey(ns, "gpu", []string{"true"}) {
		t.Fatal("expected custom NodeSelector implementation to be inspectable via Terms()")
	}
}

func TestTermsReturnsDefensiveCopy(t *testing.T) {
	ns, err := NewNodeSelector([]NodeSelectorTerm{{
		MatchExpressions: []NodeSelectorRequirement{{
			Key:      "gpu",
			Operator: NodeSelectorOpExists,
		}},
	}})
	if err != nil {
		t.Fatalf("NewNodeSelector: %v", err)
	}
	terms := ns.Terms()
	if len(terms) != 1 {
		t.Fatalf("expected 1 term, got %d", len(terms))
	}
	terms[0] = NodeSelectorTerm{}
	if len(ns.Terms()) != 1 {
		t.Fatal("Terms() should return a copy of the term slice")
	}
	terms = ns.Terms()
	terms[0].MatchExpressions = append(terms[0].MatchExpressions, NodeSelectorRequirement{
		Key:      "zone",
		Operator: NodeSelectorOpExists,
	})
	if len(ns.Terms()[0].MatchExpressions) != 1 {
		t.Fatal("Terms() should copy MatchExpressions so append does not affect internal state")
	}
	terms = ns.Terms()
	terms[0].MatchFields = append(terms[0].MatchFields, NodeSelectorRequirement{
		Key:      "metadata.name",
		Operator: NodeSelectorOpExists,
	})
	if len(ns.Terms()[0].MatchFields) != 0 {
		t.Fatal("Terms() should copy MatchFields so append does not affect internal state")
	}
}

func TestTermsCopiesPreexistingMatchFields(t *testing.T) {
	ns, err := NewNodeSelector([]NodeSelectorTerm{{
		MatchFields: []NodeSelectorRequirement{{
			Key:      "metadata.name",
			Operator: NodeSelectorOpExists,
		}},
	}})
	if err != nil {
		t.Fatalf("NewNodeSelector: %v", err)
	}
	terms := ns.Terms()
	terms[0].MatchFields = append(terms[0].MatchFields, NodeSelectorRequirement{
		Key:      "zone",
		Operator: NodeSelectorOpExists,
	})
	if len(ns.Terms()[0].MatchFields) != 1 {
		t.Fatal("Terms() should copy MatchFields so append does not affect internal state")
	}
}
