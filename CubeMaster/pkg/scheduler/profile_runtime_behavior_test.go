// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	sscore "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
	"k8s.io/apimachinery/pkg/api/resource"
)

const imageStateSizeBytes int64 = 40000 * 1024 * 1024

// A20-R3 behavioral tests prove each builtin runtime Profile reaches the real
// runtime selector path and changes ranking. They are deterministic, in-process
// only: not live sandbox, not remote, not performance claims.

func TestA20R3BalancedSpreadRuntimeRanking(t *testing.T) {
	if runIsolatedSchedulerConfigTest(t) {
		return
	}
	origPostScore := scheduler.postScore
	defer func() { scheduler.postScore = origPostScore }()
	scheduler.postScore = nil

	quiet := &node.Node{
		InsID:               "node-quiet",
		QuotaCpu:            1000,
		QuotaMem:            1000,
		QuotaCpuUsage:       200,
		QuotaMemUsage:       200,
		MvmNum:              2,
		MaxMvmLimit:         10,
		CpuUtil:             20,
		CreateConcurrentNum: 10,
		RealTimeCreateNum:   1,
	}
	busy := &node.Node{
		InsID:               "node-busy",
		QuotaCpu:            1000,
		QuotaMem:            1000,
		QuotaCpuUsage:       200,
		QuotaMemUsage:       200,
		MvmNum:              2,
		MaxMvmLimit:         10,
		CpuUtil:             20,
		CreateConcurrentNum: 10,
		RealTimeCreateNum:   9,
	}

	profileOrder := rankWithSchedulerYAML(t, `common: {}
log: {}
scheduler:
  ignore_redis_allocation: false
  profile: balanced_spread
`, node.NodeList{busy, quiet}, balancedSpreadReqRes())
	explicitOrder := rankWithSchedulerYAML(t, `common: {}
log: {}
scheduler:
  ignore_redis_allocation: false
  score:
    enable_scorers:
      - real_time_weighted_average
    resource_weights:
      realtime_create_num: 2
      mvm_num: 2
      cpu_util: 1
      quota_cpu_usage: 1
      quota_mem_usage: 1
    plugin_conf:
      real_time_weighted_average:
        weight: 1
        enable_weight_factors:
          - realtime_create_num
          - mvm_num
          - cpu_util
          - quota_cpu_usage
          - quota_mem_usage
`, node.NodeList{busy, quiet}, balancedSpreadReqRes())

	if profileOrder[0] != "node-quiet" {
		t.Fatalf("balanced_spread first candidate = %s, want node-quiet (lower RealTimeCreateNum)", profileOrder[0])
	}
	if profileOrder[1] != "node-busy" {
		t.Fatalf("balanced_spread second candidate = %s, want node-busy", profileOrder[1])
	}
	assertSameOrder(t, "balanced_spread vs explicit RealTimeWeightedAverage", profileOrder, explicitOrder)
}

func TestA20R3TemplateLocalityFirstRuntimeRanking(t *testing.T) {
	if runIsolatedSchedulerConfigTest(t) {
		return
	}
	origPostScore := scheduler.postScore
	defer func() { scheduler.postScore = origPostScore }()
	scheduler.postScore = nil

	local := &node.Node{
		InsID:           "node-local",
		Healthy:         true,
		ReportedReady:   true,
		OssClusterLabel: "a20r3-cluster",
		QuotaCpu:        1000,
		QuotaMem:        1000,
		QuotaCpuUsage:   100,
		QuotaMemUsage:   100,
		MvmNum:          1,
		MaxMvmLimit:     10,
	}
	remote := &node.Node{
		InsID:           "node-remote",
		Healthy:         true,
		ReportedReady:   true,
		OssClusterLabel: "a20r3-cluster",
		QuotaCpu:        1000,
		QuotaMem:        1000,
		QuotaCpuUsage:   100,
		QuotaMemUsage:   100,
		MvmNum:          1,
		MaxMvmLimit:     10,
	}

	// Config must exist before UpsertNode touches instance-type helpers.
	initSchedulerYAML(t, `common: {}
log: {}
scheduler:
  profile: template_locality_first
`)
	localcache.UpsertNode(local)
	localcache.UpsertNode(remote)
	t.Cleanup(func() {
		localcache.DeregisterTemplateReplica("tpl-a20r3", "node-local")
	})
	localcache.RegisterTemplateReplica("tpl-a20r3", "node-local", imageStateSizeBytes)
	if state := localcache.GetImageStateByNode("tpl-a20r3", "node-local"); state == nil || state.ScaledImageScore <= 0 {
		t.Fatalf("local template state missing or zero score: %+v", state)
	}
	if state := localcache.GetImageStateByNode("tpl-a20r3", "node-remote"); state != nil {
		t.Fatalf("remote node unexpectedly has template state: %+v", state)
	}

	req := &selctx.RequestResource{
		Cpu:        resource.MustParse("100m"),
		Mem:        resource.MustParse("128Mi"),
		TemplateID: "tpl-a20r3",
	}

	profileOrder := rankWithCurrentConfig(t, node.NodeList{remote, local}, req)
	explicitOrder := rankWithSchedulerYAML(t, `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - image_score
    resource_weights:
      image_id: 1
      template_id: 2
    plugin_conf:
      image_score:
        weight: 1
        enable_weight_factors:
          - image_id
          - template_id
`, node.NodeList{remote, local}, req)

	if profileOrder[0] != "node-local" {
		t.Fatalf("template_locality_first first candidate = %s, want node-local", profileOrder[0])
	}
	if profileOrder[1] != "node-remote" {
		t.Fatalf("template_locality_first second candidate = %s, want node-remote", profileOrder[1])
	}
	assertSameOrder(t, "template_locality_first vs explicit ImageScore", profileOrder, explicitOrder)
}

func TestA20R3BinpackUtilizationRuntimeRanking(t *testing.T) {
	if runIsolatedSchedulerConfigTest(t) {
		return
	}
	origPostScore := scheduler.postScore
	defer func() { scheduler.postScore = origPostScore }()
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
  ignore_redis_allocation: false
  profile: binpack_utilization
`)
	binpack := config.GetConfig().Scheduler.Score.ScorePluginConf.BinpackScore
	if binpack == nil {
		t.Fatal("binpack_utilization did not inject BinpackScore defaults")
	}
	if binpack.CPUWeight != 1 || binpack.MemWeight != 1 || binpack.MvmWeight != 1 {
		t.Fatalf("binpack default weights = cpu:%v mem:%v mvm:%v, want 1/1/1",
			binpack.CPUWeight, binpack.MemWeight, binpack.MvmWeight)
	}

	profileOrder := rankWithCurrentConfig(t, node.NodeList{empty, full}, nil)
	explicitOrder := rankWithSchedulerYAML(t, `common: {}
log: {}
scheduler:
  ignore_redis_allocation: false
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        weight: 1
        cpu_weight: 1
        mem_weight: 1
        mvm_weight: 1
`, node.NodeList{empty, full}, nil)

	if profileOrder[0] != "node-full" {
		t.Fatalf("binpack_utilization first candidate = %s, want node-full", profileOrder[0])
	}
	if profileOrder[1] != "node-empty" {
		t.Fatalf("binpack_utilization second candidate = %s, want node-empty", profileOrder[1])
	}
	assertSameOrder(t, "binpack_utilization vs explicit BinpackScore", profileOrder, explicitOrder)
}

func balancedSpreadReqRes() *selctx.RequestResource {
	return &selctx.RequestResource{
		Cpu: resource.MustParse("100m"),
		Mem: resource.MustParse("128Mi"),
	}
}

func rankWithSchedulerYAML(t *testing.T, yamlBody string, candidates node.NodeList, req *selctx.RequestResource) []string {
	t.Helper()
	initSchedulerYAML(t, yamlBody)
	return rankWithCurrentConfig(t, candidates, req)
}

func rankWithCurrentConfig(t *testing.T, candidates node.NodeList, req *selctx.RequestResource) []string {
	t.Helper()
	selectors := sscore.NewSelector(context.Background())
	if len(selectors) == 0 {
		t.Fatal("NewSelector returned no scorers")
	}
	selCtx := selctx.New("random")
	selCtx.Ctx = context.Background()
	selCtx.ReqRes = req
	// Copy the candidate slice so each ranking starts from the same input order.
	copied := make(node.NodeList, len(candidates))
	copy(copied, candidates)
	selCtx.SetNodes(copied)
	if err := runScoreFilter(selCtx, selectors); err != nil {
		t.Fatalf("runScoreFilter() error = %v", err)
	}
	got := selCtx.LeastScoreNodes(-1)
	if got.Len() != len(candidates) {
		t.Fatalf("ranked len = %d, want %d", got.Len(), len(candidates))
	}
	order := make([]string, 0, got.Len())
	for i := range got {
		order = append(order, got[i].ID())
	}
	return order
}

func assertSameOrder(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: order len = %d, want %d (%v vs %v)", label, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: order = %v, want %v", label, got, want)
		}
	}
}
