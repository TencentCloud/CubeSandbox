// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

func TestGetResourceWithOverheadUsesHostCPUBurstAnnotation(t *testing.T) {
	req := &cubebox.RunCubeSandboxRequest{
		InstanceType: cubebox.InstanceType_cubebox.String(),
		Annotations: map[string]string{
			constants.MasterAnnotationsHostCPUBurst: "2",
		},
		Containers: []*cubebox.ContainerConfig{
			{
				Name: "runtime-sidecar",
				Resources: &cubebox.Resource{
					Cpu: "1",
					Mem: "128Mi",
				},
			},
		},
	}

	rq, err := (&OverheadConfig{}).GetResourceWithOverhead(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("GetResourceWithOverhead returned error: %v", err)
	}

	if got := rq.HostCpuQ.String(); got != "1" {
		t.Fatalf("HostCpuQ = %q, want %q", got, "1")
	}
	if got := rq.HostCpuBurstQ.String(); got != "2" {
		t.Fatalf("HostCpuBurstQ = %q, want %q", got, "2")
	}
	if got := rq.VmCpuQ.String(); got != "1" {
		t.Fatalf("VmCpuQ = %q, want %q", got, "1")
	}
}

func TestGetResourceWithOverheadRejectsInvalidHostCPUBurstAnnotation(t *testing.T) {
	req := &cubebox.RunCubeSandboxRequest{
		InstanceType: cubebox.InstanceType_cubebox.String(),
		Annotations: map[string]string{
			constants.MasterAnnotationsHostCPUBurst: "not-a-cpu",
		},
		Containers: []*cubebox.ContainerConfig{
			{
				Name: "runtime-sidecar",
				Resources: &cubebox.Resource{
					Cpu: "1",
					Mem: "128Mi",
				},
			},
		},
	}

	_, err := (&OverheadConfig{}).GetResourceWithOverhead(context.Background(), req, nil)
	if err == nil {
		t.Fatal("GetResourceWithOverhead returned nil error, want error")
	}
	if !strings.Contains(err.Error(), "parse host cpu burst") {
		t.Fatalf("error = %q, want host cpu burst parse error", err.Error())
	}
}

func TestGetResourceWithOverheadRejectsExcessiveHostCPUBurstAnnotation(t *testing.T) {
	req := &cubebox.RunCubeSandboxRequest{
		InstanceType: cubebox.InstanceType_cubebox.String(),
		Annotations: map[string]string{
			constants.MasterAnnotationsHostCPUBurst: "3",
		},
		Containers: []*cubebox.ContainerConfig{
			{
				Name: "runtime-sidecar",
				Resources: &cubebox.Resource{
					Cpu: "1",
					Mem: "128Mi",
				},
			},
		},
	}

	_, err := (&OverheadConfig{}).GetResourceWithOverhead(context.Background(), req, nil)
	if err == nil {
		t.Fatal("GetResourceWithOverhead returned nil error, want error")
	}
	if !strings.Contains(err.Error(), "host cpu burst extra must be <= host cpu quota") {
		t.Fatalf("error = %q, want host cpu burst range error", err.Error())
	}
}
