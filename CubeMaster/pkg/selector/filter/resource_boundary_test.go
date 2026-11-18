// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package filter

import (
	"context"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestCPUQuotaExactFit(t *testing.T) {
	patches := gomonkey.ApplyFunc(config.GetConfig, func() *config.Config {
		return &config.Config{Scheduler: &config.WrapperSchedulerConf{SchedulerConf: config.SchedulerConf{NodeMaxCpuUtil: 80}}}
	})
	defer patches.Reset()
	for _, tc := range []struct {
		cpu  string
		want int
	}{{"999m", 1}, {"1", 1}, {"1001m", 0}} {
		ctx := selctx.New("random")
		ctx.Ctx = context.Background()
		ctx.ReqRes = &selctx.RequestResource{Cpu: resource.MustParse(tc.cpu)}
		ctx.SetNodes(node.NodeList{&node.Node{InsID: "last-slot", QuotaCpu: 64000, QuotaCpuUsage: 63000}})
		got, err := NewCpuFilter().Select(ctx)
		require.NoError(t, err)
		require.Len(t, got, tc.want, tc.cpu)
	}
}

func TestMemoryQuotaExactFitAndBytePrecision(t *testing.T) {
	patches := gomonkey.ApplyFunc(config.GetConfig, func() *config.Config {
		return &config.Config{Scheduler: &config.WrapperSchedulerConf{}}
	})
	defer patches.Reset()
	for _, tc := range []struct {
		mem  string
		want int
	}{{"1073741823", 1}, {"1Gi", 1}, {"1073741825", 0}} {
		ctx := selctx.New("random")
		ctx.Ctx = context.Background()
		ctx.ReqRes = &selctx.RequestResource{Mem: resource.MustParse(tc.mem)}
		ctx.SetNodes(node.NodeList{&node.Node{InsID: "last-slot", QuotaMem: 65536, QuotaMemUsage: 64512, MemMBTotal: 65536}})
		got, err := NewMemFilter().Select(ctx)
		require.NoError(t, err)
		require.Len(t, got, tc.want, tc.mem)
	}
}
