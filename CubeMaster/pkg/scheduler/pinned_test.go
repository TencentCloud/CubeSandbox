// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/profile"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/backofffilter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/plugin/expr"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/prefilter"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// staysOnOrigin reports whether the selected node is the origin.
func staysOnOrigin(origin, selected *node.Node, err error) bool {
	if err != nil || selected == nil || origin == nil {
		return false
	}
	return selected.ID() == origin.ID() || (origin.HostIP() != "" && selected.HostIP() == origin.HostIP())
}

func installPinnedFixture(t testing.TB) {
	t.Helper()
	dir := t.TempDir()
	confPath := filepath.Join(dir, "conf.yaml")
	content := []byte("common: {}\nlog:\n  module: pinned-test\n  level: error\n  path: " + filepath.Join(dir, "log") + "\n")
	if err := os.WriteFile(confPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CUBE_MASTER_CONFIG_PATH", confPath)
	if _, err := config.Init(); err != nil {
		t.Fatalf("config init: %v", err)
	}
	prevLevel := CubeLog.GetLevel()
	CubeLog.SetLevel(CubeLog.ERROR)
	t.Cleanup(func() { CubeLog.SetLevel(prevLevel) })
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
	set, err := profile.Compile(context.Background(), config.GetConfig(), registry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })

	prevProfiles := scheduler.profiles.Load()
	prevPre := scheduler.preSelector
	prevBackoff := scheduler.backoffSelector
	scheduler.profiles.Store(set)
	scheduler.preSelector = prefilter.NewPreFilter()
	scheduler.backoffSelector = backofffilter.NewBackoffFilter()
	t.Cleanup(func() {
		scheduler.profiles.Store(prevProfiles)
		scheduler.preSelector = prevPre
		scheduler.backoffSelector = prevBackoff
	})
}

func pinnedFleet(n int) node.NodeList {
	now := time.Now()
	nodes := make(node.NodeList, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("n%d", i)
		nodes = append(nodes, &node.Node{
			Index: i + 1, InsID: id, IP: fmt.Sprintf("10.0.0.%d", i+1),
			Healthy: true, MvmNum: 1,
			CpuTotal: 64000, CpuLoadUsage: 1000,
			QuotaCpu: 64000, QuotaCpuUsage: 8000,
			CpuUtil:  10,
			QuotaMem: 131072, QuotaMemUsage: 8192,
			MemMBTotal: 131072, MemUsage: 8192,
			StorageDiskUsagePer: 10, SysDiskUsagePer: 10, DataDiskUsagePer: 10,
			MetricUpdate: now, MetricLocalUpdateAt: now,
		})
	}
	return nodes
}

func originRequest(origin *node.Node) *selctx.RequestResource {
	return &selctx.RequestResource{
		Cpu: resource.MustParse("1"), Mem: resource.MustParse("1Gi"),
		AllowNonLocalTemplate: true,
		TemplateNodeScope:     []string{origin.ID(), origin.HostIP()},
	}
}

func newPinnedCtx(origin *node.Node) *selctx.SelectorCtx {
	sel := selctx.New("random")
	sel.Ctx = context.Background()
	sel.InstanceType = "cubebox"
	sel.ReqRes = originRequest(origin)
	return sel
}

func TestSelectPinnedAgreesWithSelect(t *testing.T) {
	installPinnedFixture(t)

	cases := []struct {
		name       string
		mutate     func(*node.Node)
		scope      []string
		wantOrigin bool
	}{
		{name: "healthy", wantOrigin: true},
		{name: "unhealthy", mutate: func(n *node.Node) { n.Healthy = false }},
		{name: "cpu", mutate: func(n *node.Node) { n.QuotaCpuUsage = n.QuotaCpu }},
		{name: "mem", mutate: func(n *node.Node) { n.QuotaMemUsage = n.QuotaMem }},
		{name: "disk", mutate: func(n *node.Node) { n.StorageDiskUsagePer = 100 }},
		{name: "mvm", mutate: func(n *node.Node) { n.MvmNum = 100000 }},
		{name: "stale metric", mutate: func(n *node.Node) { n.MetricUpdate = time.Now().Add(-2 * time.Hour) }},
		{name: "realtime", mutate: func(n *node.Node) { n.RealTimeCreateNum = 1000 }},
		{name: "outside scope", scope: []string{"someone-else"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fleet := pinnedFleet(8)
			origin := fleet[0]
			if tc.mutate != nil {
				tc.mutate(origin)
			}
			restore := prefilter.SetSchedulableSourceForTest(func(int, string) node.NodeList { return fleet })
			defer restore()

			fullCtx := newPinnedCtx(origin)
			pinnedCtx := newPinnedCtx(origin)
			if tc.scope != nil {
				fullCtx.ReqRes.TemplateNodeScope = tc.scope
				pinnedCtx.ReqRes.TemplateNodeScope = append([]string(nil), tc.scope...)
			}
			fullNode, fullErr := Select(fullCtx)
			pinnedNode, pinnedErr := SelectPinned(pinnedCtx, node.NodeList{origin})
			fullStay := staysOnOrigin(origin, fullNode, fullErr)
			pinnedStay := staysOnOrigin(origin, pinnedNode, pinnedErr)
			if fullStay != pinnedStay || fullStay != tc.wantOrigin {
				t.Fatalf("Select stay=%v (%v) SelectPinned stay=%v (%v), want %v",
					fullStay, fullErr, pinnedStay, pinnedErr, tc.wantOrigin)
			}
		})
	}
}

func BenchmarkOriginCheck(b *testing.B) {
	installPinnedFixture(b)
	fleet := pinnedFleet(200)
	origin := fleet[0]
	restore := prefilter.SetSchedulableSourceForTest(func(int, string) node.NodeList { return fleet })
	defer restore()

	b.Run("select", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = Select(newPinnedCtx(origin))
		}
	})
	b.Run("pinned", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = SelectPinned(newPinnedCtx(origin), node.NodeList{origin})
		}
	})
}
