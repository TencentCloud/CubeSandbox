// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package profile_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/profile"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin"
)

// Importing scheduler installs the plugin-duration resolver before Compile.
var _ = scheduler.DefaultProfile

func TestCompiledBindingsCarryPluginObservers(t *testing.T) {
	set := compileFactory(t)
	t.Cleanup(func() { _ = set.Close() })

	pipeline := set.Match(&selctx.SelectorCtx{})
	if pipeline == nil {
		t.Fatal("factory default pipeline is nil")
	}
	assertObservers(t, pipeline)

	legacy := compileLegacy(t)
	t.Cleanup(func() { _ = legacy.Close() })
	legacyPipeline := legacy.Match(&selctx.SelectorCtx{})
	if legacyPipeline == nil || len(legacyPipeline.Filters) == 0 {
		t.Fatalf("legacy pipeline = %+v", legacyPipeline)
	}
	assertObservers(t, legacyPipeline)
}

func TestObserverWritesTheSameSeriesAsLabelLookup(t *testing.T) {
	set := compileFactory(t)
	t.Cleanup(func() { _ = set.Close() })
	pipeline := set.Match(&selctx.SelectorCtx{})
	if pipeline == nil || len(pipeline.Guards) == 0 || pipeline.Guards[0].Observer == nil {
		t.Fatal("compiled guard has no observer")
	}
	guard := pipeline.Guards[0]
	before := seriesCount(t, pipeline.Name, guard.Name, profile.PluginKindGuard)
	guard.Observer.Observe(0.001)
	after := seriesCount(t, pipeline.Name, guard.Name, profile.PluginKindGuard)
	if after != before+1 {
		t.Fatalf("series count = %d, want %d", after, before+1)
	}
}

func assertObservers(t *testing.T, pipeline *profile.Pipeline) {
	t.Helper()
	for _, binding := range pipeline.Guards {
		if binding.Observer == nil {
			t.Fatalf("guard %q on profile %q has no observer", binding.Name, pipeline.Name)
		}
	}
	for _, binding := range pipeline.Filters {
		if binding.Observer == nil {
			t.Fatalf("filter %q on profile %q has no observer", binding.Name, pipeline.Name)
		}
	}
	for _, binding := range pipeline.Scores {
		if binding.Observer == nil {
			t.Fatalf("score %q on profile %q has no observer", binding.Name, pipeline.Name)
		}
	}
}

func compileFactory(t *testing.T) *profile.Set {
	t.Helper()
	initSchedulerConfig(t, "common: {}\nlog:\n  module: observer-factory\n  path: "+t.TempDir()+"\n")
	registry := plugin.NewRegistry()
	if err := plugin.RegisterBuiltins(registry); err != nil {
		t.Fatal(err)
	}
	set, err := profile.Compile(context.Background(), config.GetConfig(), registry)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func compileLegacy(t *testing.T) *profile.Set {
	t.Helper()
	dir := t.TempDir()
	conf := "common: {}\nlog:\n  module: observer-legacy\n  path: " + dir + "\n" +
		"scheduler:\n  disable_factory_profiles: true\n  priority_select_num: 1\n" +
		"  filter:\n    enable_filters: [cpu]\n"
	initSchedulerConfig(t, conf)
	registry := plugin.NewRegistry()
	if err := plugin.RegisterBuiltins(registry); err != nil {
		t.Fatal(err)
	}
	set, err := profile.Compile(context.Background(), config.GetConfig(), registry)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func initSchedulerConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "conf.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CUBE_MASTER_CONFIG_PATH", path)
	if _, err := config.Init(); err != nil {
		t.Fatal(err)
	}
}

func seriesCount(t *testing.T, profileName, pluginName, kind string) uint64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "scheduler_plugin_duration_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			if label(metric, "profile") == profileName && label(metric, "plugin") == pluginName && label(metric, "kind") == kind {
				return metric.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

func label(metric *dto.Metric, name string) string {
	for _, pair := range metric.GetLabel() {
		if pair.GetName() == name {
			return pair.GetValue()
		}
	}
	return ""
}
