// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"context"
	"fmt"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/profile"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

type keepFilter struct{ extra node.NodeList }

func (keepFilter) ID() string { return "filter/keep" }

func (f keepFilter) Select(s *selctx.SelectorCtx) (node.NodeList, error) {
	if f.extra != nil {
		return append(node.NodeList(nil), f.extra...), nil
	}
	return s.Nodes(), nil
}

func filterBenchCtx(n int) *selctx.SelectorCtx {
	s := selctx.New("random")
	s.Ctx = context.Background()
	list := make(node.NodeList, n)
	for i := range list {
		list[i] = &node.Node{InsID: fmt.Sprintf("node-%d", i), IP: "10.0.0.1"}
	}
	s.SetNodes(list)
	return s
}

func sixKeepFilters() []profile.FilterPlugin {
	plugins := make([]profile.FilterPlugin, 6)
	for i := range plugins {
		plugins[i] = profile.FilterPlugin{Name: fmt.Sprintf("g%d", i), Selector: keepFilter{}, Failure: profile.FilterFailClosed}
	}
	return plugins
}

func TestRunProfileFiltersRejectsNonCandidate(t *testing.T) {
	s := filterBenchCtx(2)
	plugins := []profile.FilterPlugin{{
		Name:     "bad",
		Selector: keepFilter{extra: node.NodeList{{InsID: "not-a-candidate"}}},
		Failure:  profile.FilterFailClosed,
	}}
	if err := runProfileFilters(s, pluginKindFilter, plugins); err == nil {
		t.Fatal("non-candidate node must fail the filter stage")
	}
}

func TestRunProfileFiltersRejectsDuplicate(t *testing.T) {
	s := filterBenchCtx(1)
	dup := s.Nodes()[0]
	plugins := []profile.FilterPlugin{{
		Name:     "dup",
		Selector: keepFilter{extra: node.NodeList{dup, dup}},
		Failure:  profile.FilterFailClosed,
	}}
	if err := runProfileFilters(s, pluginKindFilter, plugins); err == nil {
		t.Fatal("duplicate node must fail the filter stage")
	}
}

func TestRunProfileFiltersAllocBound(t *testing.T) {
	plugins := sixKeepFilters()
	s := filterBenchCtx(300)
	list := s.Nodes()
	allocs := testing.AllocsPerRun(30, func() {
		s.SetNodes(list)
		if err := runProfileFilters(s, pluginKindGuard, plugins); err != nil {
			t.Fatalf("filter: %v", err)
		}
	})
	if allocs > 40 {
		t.Fatalf("runProfileFilters allocs = %.0f, want <= 40", allocs)
	}
}

func BenchmarkRunProfileFilters(b *testing.B) {
	plugins := sixKeepFilters()
	s := filterBenchCtx(300)
	list := s.Nodes()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.SetNodes(list)
		if err := runProfileFilters(s, pluginKindGuard, plugins); err != nil {
			b.Fatal(err)
		}
	}
}
