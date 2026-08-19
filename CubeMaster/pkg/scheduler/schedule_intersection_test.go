// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
)

type stubFilter struct {
	id  string
	out node.NodeList
}

func (f *stubFilter) Select(_ *selctx.SelectorCtx) (node.NodeList, error) { return f.out, nil }
func (f *stubFilter) ID() string                                          { return f.id }

func stubNode(id string) *node.Node {
	n := &node.Node{}
	n.InsID = id
	return n
}

func idsOf(list node.NodeList) []string {
	out := make([]string, 0, list.Len())
	for _, n := range list {
		out = append(out, n.ID())
	}
	return out
}

func TestParallelRunFiltersExcludesNodeRejectedByAnyFilter(t *testing.T) {
	good, rejected := stubNode("node-good"), stubNode("node-rejected")

	ctx := &selctx.SelectorCtx{Ctx: context.Background()}
	ctx.SetNodes(node.NodeList{good, rejected})

	filters := []filter.Selector{
		&stubFilter{id: "a", out: node.NodeList{good, rejected, rejected}},
		&stubFilter{id: "b", out: node.NodeList{good}},
	}

	got, err := parallelRunFilters(ctx, filters)
	if err != nil {
		t.Fatalf("parallelRunFilters: %v", err)
	}

	ids := idsOf(got)
	if len(ids) != 1 || ids[0] != "node-good" {
		t.Fatalf("intersection = %v, want [node-good]; a node rejected by filter b survived", ids)
	}
}

func TestParallelRunFiltersKeepsNodeAcceptedByAll(t *testing.T) {
	a, b := stubNode("node-a"), stubNode("node-b")

	ctx := &selctx.SelectorCtx{Ctx: context.Background()}
	ctx.SetNodes(node.NodeList{a, b})

	filters := []filter.Selector{
		&stubFilter{id: "a", out: node.NodeList{a, b}},
		&stubFilter{id: "b", out: node.NodeList{b, a}},
	}

	got, err := parallelRunFilters(ctx, filters)
	if err != nil {
		t.Fatalf("parallelRunFilters: %v", err)
	}
	if got.Len() != 2 {
		t.Fatalf("intersection = %v, want both nodes", idsOf(got))
	}
}

func TestParallelRunFiltersDuplicatesDoNotAdmitOnTheirOwn(t *testing.T) {
	only := stubNode("node-only")

	ctx := &selctx.SelectorCtx{Ctx: context.Background()}
	ctx.SetNodes(node.NodeList{only})

	filters := []filter.Selector{
		&stubFilter{id: "a", out: node.NodeList{only, only, only}},
		&stubFilter{id: "b", out: node.NodeList{}},
		&stubFilter{id: "c", out: node.NodeList{only}},
	}

	got, err := parallelRunFilters(ctx, filters)
	if err != nil {
		t.Fatalf("parallelRunFilters: %v", err)
	}
	if got.Len() != 0 {
		t.Fatalf("intersection = %v, want empty; filter b returned nothing", idsOf(got))
	}
}
