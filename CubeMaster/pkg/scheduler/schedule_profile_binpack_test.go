// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	sscore "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
)

func TestRunScoreFilterBinpackScorePrefersFullerNode(t *testing.T) {
	if runIsolatedSchedulerConfigTest(t) {
		return
	}

	origPostScore := scheduler.postScore
	defer func() {
		scheduler.postScore = origPostScore
	}()
	scheduler.postScore = nil

	configPath := filepath.Join(t.TempDir(), "cubemaster.yaml")
	content := `common: {}
log: {}
scheduler:
  ignore_redis_allocation: false
  overcommit_ratio:
    cpu_ratio: 1
    mem_ratio: 1
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        weight: 1
`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("CUBE_MASTER_CONFIG_PATH", configPath)
	if _, err := config.Init(); err != nil {
		t.Fatalf("config.Init(): %v", err)
	}

	empty := &node.Node{
		InsID: "node-empty", QuotaCpu: 1000, QuotaMem: 1000,
		QuotaCpuUsage: 100, QuotaMemUsage: 100, MvmNum: 1, MaxMvmLimit: 10,
	}
	full := &node.Node{
		InsID: "node-full", QuotaCpu: 1000, QuotaMem: 1000,
		QuotaCpuUsage: 800, QuotaMemUsage: 800, MvmNum: 8, MaxMvmLimit: 10,
	}
	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.SetNodes(node.NodeList{empty, full})

	err := runScoreFilter(selCtx, []sscore.Selector{
		sscore.NewBinpackScore(),
	})
	if err != nil {
		t.Fatalf("runScoreFilter() error = %v, want nil", err)
	}
	got := selCtx.LeastScoreNodes(-1)
	if got.Len() != 2 {
		t.Fatalf("len(score nodes) = %d, want 2", got.Len())
	}
	if got[0].ID() != "node-full" {
		t.Fatalf("highest score = %s, want node-full", got[0].ID())
	}
	if got[1].ID() != "node-empty" {
		t.Fatalf("lowest score = %s, want node-empty", got[1].ID())
	}
}

// TestRunScoreFilterBuiltinProfileOverlayChangesPlacementOrder is in-process only.
// It is not CubeAPI/Cubelet E2E, not multi-VM, not Prometheus E2E, and not real
// create latency. It proves scheduler.profile overlay reaches score.NewSelector
// and production runScoreFilter, changing candidate order vs empty profile.
func TestRunScoreFilterBuiltinProfileOverlayChangesPlacementOrder(t *testing.T) {
	if runIsolatedSchedulerConfigTest(t) {
		return
	}

	origPostScore := scheduler.postScore
	defer func() {
		scheduler.postScore = origPostScore
	}()
	scheduler.postScore = nil

	empty := &node.Node{
		InsID: "node-empty", QuotaCpu: 1000, QuotaMem: 1000,
		QuotaCpuUsage: 100, QuotaMemUsage: 100, MvmNum: 1, MaxMvmLimit: 10,
	}
	full := &node.Node{
		InsID: "node-full", QuotaCpu: 1000, QuotaMem: 1000,
		QuotaCpuUsage: 800, QuotaMemUsage: 800, MvmNum: 8, MaxMvmLimit: 10,
	}

	initSchedulerYAML(t, `common: {}
log: {}
scheduler:
  filter:
    enable_filters:
      - cpu
      - mem
  score:
    enable_scorers: []
`)
	baseline := sscore.NewSelector(context.Background())
	if len(baseline) != 0 {
		t.Fatalf("empty profile NewSelector len = %d, want 0", len(baseline))
	}
	baselineCtx := selctx.New("random")
	baselineCtx.Ctx = context.Background()
	baselineCtx.SetNodes(node.NodeList{empty, full})
	if err := runScoreFilter(baselineCtx, baseline); err != nil {
		t.Fatalf("baseline runScoreFilter() error = %v, want nil", err)
	}
	if baselineCtx.Nodes()[0].ID() != "node-empty" {
		t.Fatalf("baseline first candidate = %s, want node-empty (input order, no scorers)", baselineCtx.Nodes()[0].ID())
	}

	initSchedulerYAML(t, `common: {}
log: {}
scheduler:
  profile: binpack_utilization
`)
	enabled := sscore.NewSelector(context.Background())
	if len(enabled) != 1 {
		t.Fatalf("binpack_utilization NewSelector len = %d, want 1", len(enabled))
	}
	if enabled[0].ID() != "Score/binpack_score" {
		t.Fatalf("enabled selector ID = %s, want Score/binpack_score", enabled[0].ID())
	}
	enabledCtx := selctx.New("random")
	enabledCtx.Ctx = context.Background()
	enabledCtx.SetNodes(node.NodeList{empty, full})
	if err := runScoreFilter(enabledCtx, enabled); err != nil {
		t.Fatalf("enabled runScoreFilter() error = %v, want nil", err)
	}
	if enabledCtx.Nodes()[0].ID() != "node-full" {
		t.Fatalf("enabled first candidate = %s, want node-full", enabledCtx.Nodes()[0].ID())
	}
}

func initSchedulerYAML(t *testing.T, yamlBody string) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "cubemaster.yaml")
	if err := os.WriteFile(configPath, []byte(yamlBody), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("CUBE_MASTER_CONFIG_PATH", configPath)
	if _, err := config.Init(); err != nil {
		t.Fatalf("config.Init(): %v", err)
	}
}

const isolatedSchedulerConfigTestEnv = "CUBEMASTER_ISOLATED_SCHEDULER_CONFIG_TEST"

// runIsolatedSchedulerConfigTest runs config-mutating tests in a child test
// process because config exposes no setter that can restore its package-global
// pointer, including the original nil state.
func runIsolatedSchedulerConfigTest(t *testing.T) bool {
	t.Helper()
	if os.Getenv(isolatedSchedulerConfigTestEnv) == t.Name() {
		return false
	}

	originalConfig := config.GetConfig()
	t.Cleanup(func() {
		if got := config.GetConfig(); got != originalConfig {
			t.Errorf("global config changed in parent process: got %p, want %p", got, originalConfig)
		}
	})

	cmd := exec.Command(
		os.Args[0],
		"-test.run=^"+regexp.QuoteMeta(t.Name())+"$",
		"-test.count=1",
	)
	cmd.Env = append(os.Environ(), isolatedSchedulerConfigTestEnv+"="+t.Name())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated test process failed: %v\n%s", err, output)
	}
	return true
}
