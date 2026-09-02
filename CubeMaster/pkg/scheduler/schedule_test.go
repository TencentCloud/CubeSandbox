// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	sfilter "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
)

// staticNodesFilter serves a fixed candidate set, standing in for the
// pre/backoff selectors in Select-level tests.
type staticNodesFilter struct {
	id    string
	nodes node.NodeList
	err   error
}

func (f staticNodesFilter) ID() string { return f.id }
func (f staticNodesFilter) Select(*selctx.SelectorCtx) (node.NodeList, error) {
	return f.nodes, f.err
}

func TestShouldSkipBackoffForTemplate(t *testing.T) {
	origFilters := scheduler.filter
	defer func() {
		scheduler.filter = origFilters
	}()

	tests := []struct {
		name    string
		ctx     *selctx.SelectorCtx
		filters []sfilter.Selector
		want    bool
	}{
		{
			name: "nil selector context",
			ctx:  nil,
			filters: []sfilter.Selector{
				sfilter.NewTemplateLocalityFilter(),
			},
			want: false,
		},
		{
			name: "request without template",
			ctx: &selctx.SelectorCtx{
				ReqRes: &selctx.RequestResource{},
			},
			filters: []sfilter.Selector{
				sfilter.NewTemplateLocalityFilter(),
			},
			want: false,
		},
		{
			name: "request with template but filter disabled",
			ctx: &selctx.SelectorCtx{
				ReqRes: &selctx.RequestResource{TemplateID: "tpl-1"},
			},
			filters: nil,
			want:    false,
		},
		{
			name: "request with template and filter enabled",
			ctx: &selctx.SelectorCtx{
				ReqRes: &selctx.RequestResource{TemplateID: "tpl-1"},
			},
			filters: []sfilter.Selector{
				sfilter.NewTemplateLocalityFilter(),
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheduler.filter = tt.filters
			if got := shouldSkipBackoffForTemplate(tt.ctx); got != tt.want {
				t.Fatalf("shouldSkipBackoffForTemplate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSelectFailsClosedBeforeInit(t *testing.T) {
	// With no compiled profile set and the singleton's selector lists empty
	// (the production state before InitScheduler completes), Select must fail
	// closed rather than silently schedule with an unguarded random pipeline.
	origFilters, origScores := scheduler.filter, scheduler.score
	oldProfiles := scheduler.profiles.Swap(nil)
	defer func() {
		scheduler.filter, scheduler.score = origFilters, origScores
		scheduler.profiles.Store(oldProfiles)
	}()
	scheduler.filter, scheduler.score = nil, nil

	if _, err := Select(selctx.New("random")); err == nil {
		t.Fatal("Select before InitScheduler must fail closed")
	}
}

func TestSelectLegacyFilterErrorFailsInsteadOfBackoff(t *testing.T) {
	// On the legacy default pipeline a genuine filter error (a plugin or
	// validation failure, as opposed to an empty candidate set) must fail the
	// request: BackoffSelect must not silently rescue a defective plugin
	// while the relaxed pool is non-empty.
	origFilters, origScores := scheduler.filter, scheduler.score
	origPre, origBackoff := scheduler.preSelector, scheduler.backoffSelector
	oldProfiles := scheduler.profiles.Swap(nil)
	defer func() {
		scheduler.filter, scheduler.score = origFilters, origScores
		scheduler.preSelector, scheduler.backoffSelector = origPre, origBackoff
		scheduler.profiles.Store(oldProfiles)
	}()
	candidates := node.NodeList{{InsID: "n1"}, {InsID: "n2"}}
	scheduler.filter = []sfilter.Selector{executorFilter{id: "broken", err: errors.New("boom")}}
	scheduler.score = nil
	scheduler.preSelector = staticNodesFilter{id: "pre", nodes: candidates}
	scheduler.backoffSelector = staticNodesFilter{id: "backoff", nodes: candidates}

	ctx := selctx.New("random")
	ctx.Ctx = context.Background()
	selected, err := Select(ctx)
	if err == nil || selected != nil {
		t.Fatalf("Select = %v, %v; want the filter error, not a backoff rescue", selected, err)
	}
	if isNoCandidateError(err) {
		t.Fatal("a plugin failure must not be classified as an empty candidate set")
	}
}
