// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/profile"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	sfilter "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
)

func TestPipelineHasTemplateGuard(t *testing.T) {
	locality := profile.FilterPlugin{Name: "template_locality", Selector: sfilter.NewTemplateLocalityFilter()}
	cpuGuard := profile.FilterPlugin{Name: "cpu", Selector: sfilter.NewCpuFilter()}

	tests := []struct {
		name     string
		ctx      *selctx.SelectorCtx
		pipeline *profile.Pipeline
		want     bool
	}{
		{
			name:     "nil selector context",
			ctx:      nil,
			pipeline: &profile.Pipeline{Filters: []profile.FilterPlugin{locality}},
			want:     false,
		},
		{
			name: "request without template",
			ctx: &selctx.SelectorCtx{
				ReqRes: &selctx.RequestResource{},
			},
			pipeline: &profile.Pipeline{Filters: []profile.FilterPlugin{locality}},
			want:     false,
		},
		{
			name: "nil pipeline",
			ctx: &selctx.SelectorCtx{
				ReqRes: &selctx.RequestResource{TemplateID: "tpl-1"},
			},
			pipeline: nil,
			want:     false,
		},
		{
			name: "request with template but filter disabled",
			ctx: &selctx.SelectorCtx{
				ReqRes: &selctx.RequestResource{TemplateID: "tpl-1"},
			},
			pipeline: &profile.Pipeline{Filters: []profile.FilterPlugin{cpuGuard}},
			want:     false,
		},
		{
			name: "request with template and filter enabled",
			ctx: &selctx.SelectorCtx{
				ReqRes: &selctx.RequestResource{TemplateID: "tpl-1"},
			},
			pipeline: &profile.Pipeline{Filters: []profile.FilterPlugin{locality}},
			want:     true,
		},
		{
			name: "request with template and locality as mandatory guard",
			ctx: &selctx.SelectorCtx{
				ReqRes: &selctx.RequestResource{TemplateID: "tpl-1"},
			},
			pipeline: &profile.Pipeline{Guards: []profile.FilterPlugin{locality}},
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pipelineHasTemplateGuard(tt.ctx, tt.pipeline); got != tt.want {
				t.Fatalf("pipelineHasTemplateGuard() = %v, want %v", got, tt.want)
			}
		})
	}
}
