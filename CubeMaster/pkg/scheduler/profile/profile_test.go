// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package profile

import (
	"context"
	"io"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin/expr"
)

type trackingCloser struct{ closed bool }

func (c *trackingCloser) Close() error {
	c.closed = true
	return nil
}

var _ io.Closer = (*trackingCloser)(nil)

func profileRegistry(t *testing.T) *plugin.Registry {
	t.Helper()
	registry := plugin.NewRegistry()
	if err := plugin.RegisterBuiltins(registry); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterFilterProvider(plugin.TypeExpression, expr.NewFilter); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterScoreProvider(plugin.TypeExpression, expr.NewScore); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestProfileRoutingAndLegacyFallback(t *testing.T) {
	cfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{
		PrioritySelectNum:     3,
		ProfileRouteLabelKeys: []string{"workload"},
		Profiles: []config.SchedulerProfileConf{{
			Name:      "burst",
			Route:     config.SchedulerProfileRouteConf{InstanceTypes: []string{"S.*"}, Labels: map[string]string{"workload": "burst"}},
			Scores:    []config.SchedulerProfilePluginConf{{Name: "idle", Type: "expr", Expr: "100.0 - node.cpu_util", Weight: 2}},
			Selection: config.SchedulerSelectionConf{TopN: 5, Method: "spread"},
		}},
	}}}
	set, err := Compile(context.Background(), cfg, profileRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })

	matched := set.Match(&selctx.SelectorCtx{InstanceType: "S2", RequestLabels: map[string]string{"workload": "burst"}})
	if matched.Name != "burst" || matched.TopN != 5 || len(matched.Guards) != len(mandatoryGuardNames) {
		t.Fatalf("matched pipeline = %+v", matched)
	}
	fallback := set.Match(&selctx.SelectorCtx{InstanceType: "L1"})
	if fallback.Name != "default" || fallback.TopN != 3 || len(fallback.Guards) != 0 {
		t.Fatalf("fallback pipeline = %+v", fallback)
	}
}

func TestProfileValidationRejectsUncontrolledLabelsAndUnknownPlugins(t *testing.T) {
	tests := []struct {
		name    string
		profile config.SchedulerProfileConf
	}{
		{
			name: "uncontrolled label",
			profile: config.SchedulerProfileConf{
				Name: "bad", Route: config.SchedulerProfileRouteConf{Labels: map[string]string{"tenant": "x"}},
			},
		},
		{
			name: "unknown plugin",
			profile: config.SchedulerProfileConf{
				Name: "bad", Route: config.SchedulerProfileRouteConf{InstanceTypes: []string{".*"}},
				Filters: []config.SchedulerProfilePluginConf{{Name: "missing"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{Profiles: []config.SchedulerProfileConf{test.profile}}}}
			if _, err := Compile(context.Background(), cfg, profileRegistry(t)); err == nil {
				t.Fatal("invalid profile must be rejected")
			}
		})
	}
}

func TestCustomDefaultDoesNotCompileUnusedLegacyPlugins(t *testing.T) {
	cfg := &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{
		Filter: &config.SchedulerFilterConf{EnableFilters: []string{"removed-legacy-plugin"}},
		Profiles: []config.SchedulerProfileConf{{
			Name: "custom-default", Default: true,
		}},
	}}}
	set, err := Compile(context.Background(), cfg, profileRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	if got := set.Match(&selctx.SelectorCtx{}); got.Name != "custom-default" {
		t.Fatalf("default profile = %q", got.Name)
	}
}

func TestProfileSetDefersCloseUntilLeaseRelease(t *testing.T) {
	closer := &trackingCloser{}
	set := &Set{fallback: &Pipeline{Name: "default"}, closers: []io.Closer{closer}}
	pipeline, release, ok := set.Acquire(&selctx.SelectorCtx{})
	if !ok || pipeline == nil || pipeline.Name != "default" {
		t.Fatalf("acquire = (%+v, %v)", pipeline, ok)
	}
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	if closer.closed {
		t.Fatal("active lease must keep plugin connection open")
	}
	if _, _, ok := set.Acquire(&selctx.SelectorCtx{}); ok {
		t.Fatal("retired profile set accepted a new lease")
	}
	release()
	if !closer.closed {
		t.Fatal("last lease release must close plugin connection")
	}
}
