// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestGetReqResourceRejectsUnparseableQuantities(t *testing.T) {
	cases := []struct {
		name    string
		cpu     string
		mem     string
		wantSub string
	}{
		{"unparseable cpu", "not-a-quantity", "512Mi", "cpu limit"},
		{"unparseable mem", "500m", "10 GB", "mem limit"},
		{"both unparseable", "abc", "def", "cpu limit"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &types.CreateCubeSandboxReq{
				Containers: []*types.Container{{
					Name:      "c1",
					Resources: &types.Resource{Cpu: tc.cpu, Mem: tc.mem},
				}},
			}

			_, _, err := getReqResource(req)
			if err == nil {
				t.Fatalf("getReqResource(cpu=%q, mem=%q) returned no error", tc.cpu, tc.mem)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantSub)
			}
			if !strings.Contains(err.Error(), "c1") {
				t.Fatalf("err = %v, want it to name the offending container", err)
			}
		})
	}
}

func TestGetReqResourceSumsValidQuantities(t *testing.T) {
	req := &types.CreateCubeSandboxReq{
		Containers: []*types.Container{
			{Name: "c1", Resources: &types.Resource{Cpu: "500m", Mem: "256Mi"}},
			{Name: "c2", Resources: &types.Resource{Cpu: "250m", Mem: "256Mi"}},
		},
	}

	cpu, mem, err := getReqResource(req)
	if err != nil {
		t.Fatalf("getReqResource: %v", err)
	}
	if cpu.String() != "750m" {
		t.Fatalf("cpu = %s, want 750m", cpu.String())
	}
	if mem.String() != "512Mi" {
		t.Fatalf("mem = %s, want 512Mi", mem.String())
	}
}
