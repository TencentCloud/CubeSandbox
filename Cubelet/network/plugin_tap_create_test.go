// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/tomlext"
	"github.com/tencentcloud/CubeSandbox/Cubelet/network/proto"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/networkagentclient"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

type fakeNetworkAgentClient struct {
	ensureCalled       bool
	lastEnsureRequest  *networkagentclient.EnsureNetworkRequest
	releaseCalled      bool
	lastReleaseRequest *networkagentclient.ReleaseNetworkRequest
	healthErrs         []error
	healthCalls        int
}

func (c *fakeNetworkAgentClient) EnsureNetwork(_ context.Context, req *networkagentclient.EnsureNetworkRequest) (*networkagentclient.EnsureNetworkResponse, error) {
	c.ensureCalled = true
	c.lastEnsureRequest = req
	return &networkagentclient.EnsureNetworkResponse{
		SandboxID:     "sandbox-1",
		NetworkHandle: "sandbox-1",
		Interfaces: []networkagentclient.Interface{
			{
				Name:    "z192.168.0.40",
				MAC:     "20:90:6f:fc:fc:fc",
				MTU:     1300,
				IPs:     []string{"169.254.68.6/30"},
				Gateway: "169.254.68.5",
			},
		},
		Routes: []networkagentclient.Route{
			{
				Gateway: "169.254.68.5",
				Device:  eth0,
			},
		},
		ARPNeighbors: []networkagentclient.ARPNeighbor{
			{
				IP:     "169.254.68.5",
				MAC:    "20:90:6f:cf:cf:cf",
				Device: eth0,
			},
		},
		PersistMetadata: map[string]string{
			"sandbox_ip":   "192.168.0.40",
			"gateway_ip":   "169.254.68.5",
			"mvm_inner_ip": "169.254.68.6",
		},
	}, nil
}

func (c *fakeNetworkAgentClient) ReleaseNetwork(_ context.Context, req *networkagentclient.ReleaseNetworkRequest) error {
	c.releaseCalled = true
	c.lastReleaseRequest = req
	return nil
}

func (c *fakeNetworkAgentClient) ReconcileNetwork(context.Context, *networkagentclient.ReconcileNetworkRequest) (*networkagentclient.ReconcileNetworkResponse, error) {
	return nil, nil
}

func (c *fakeNetworkAgentClient) GetNetwork(context.Context, *networkagentclient.GetNetworkRequest) (*networkagentclient.GetNetworkResponse, error) {
	return nil, nil
}

func (c *fakeNetworkAgentClient) Health(context.Context, *networkagentclient.HealthRequest) error {
	if c.healthCalls < len(c.healthErrs) {
		err := c.healthErrs[c.healthCalls]
		c.healthCalls++
		return err
	}
	c.healthCalls++
	return nil
}

func (c *fakeNetworkAgentClient) ListNetworks(_ context.Context, _ *networkagentclient.ListNetworksRequest) (*networkagentclient.ListNetworksResponse, error) {
	return &networkagentclient.ListNetworksResponse{}, nil
}

func TestTapCreateInNetworkAgentModeCallsEnsureNetwork(t *testing.T) {
	fakeClient := &fakeNetworkAgentClient{}
	l := &local{
		Config: &Config{
			EnableNetworkAgent: true,
			MVMMacAddr:         "20:90:6f:fc:fc:fc",
			MvmMtu:             1300,
			MvmGwDestIP:        "169.254.68.5",
			MVMInnerIP:         "169.254.68.6",
			MvmMask:            30,
		},
		cubeDev:            &proto.CubeDev{Index: 16},
		networkAgentClient: fakeClient,
	}

	req := &cubebox.RunCubeSandboxRequest{
		RequestID:    "req-1",
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: "sandbox-1",
		},
		ReqInfo: req,
	}

	err := l.Create(context.Background(), opts)

	if err == nil {
		t.Fatal("Create error=nil, want downstream register failure after EnsureNetwork")
	}
	if !strings.Contains(err.Error(), "register network-agent tap for pool failed") {
		t.Fatalf("Create error=%v, want register network-agent tap failure", err)
	}
	if !fakeClient.ensureCalled {
		t.Fatal("network-agent EnsureNetwork was not called")
	}
	if !fakeClient.releaseCalled {
		t.Fatal("network-agent ReleaseNetwork was not called after downstream register failure")
	}
	if fakeClient.lastReleaseRequest == nil || fakeClient.lastReleaseRequest.NetworkHandle != "sandbox-1" {
		t.Fatalf("ReleaseNetwork request invalid: %+v", fakeClient.lastReleaseRequest)
	}
	if fakeClient.lastReleaseRequest.IdempotencyKey != "req-1" {
		t.Fatalf("ReleaseNetwork idempotency key=%q, want req-1", fakeClient.lastReleaseRequest.IdempotencyKey)
	}
}

func TestTapCreateInNetworkAgentModeAddsDNSAllowOutCIDRs(t *testing.T) {
	fakeClient := &fakeNetworkAgentClient{}
	block := false
	l := &local{
		Config: &Config{
			EnableNetworkAgent: true,
			MVMMacAddr:         "20:90:6f:fc:fc:fc",
			MvmMtu:             1300,
			MvmGwDestIP:        "169.254.68.5",
			MVMInnerIP:         "169.254.68.6",
			MvmMask:            30,
		},
		cubeDev:            &proto.CubeDev{Index: 16},
		networkAgentClient: fakeClient,
	}

	req := &cubebox.RunCubeSandboxRequest{
		RequestID: "req-dns",
		Containers: []*cubebox.ContainerConfig{
			{
				Name:      "app",
				DnsConfig: &cubebox.DNSConfig{Servers: []string{"1.1.1.1", "8.8.8.8"}},
			},
		},
		CubevsContext: &cubebox.CubeVSContext{
			AllowInternetAccess: &block,
			AllowOut:            []string{"172.67.0.0/16"},
		},
		InstanceType: cubebox.InstanceType_cubebox.String(),
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: "sandbox-dns",
		},
		ReqInfo: req,
	}

	err := l.Create(context.Background(), opts)
	if err == nil {
		t.Fatal("Create error=nil, want downstream register failure after EnsureNetwork")
	}
	if fakeClient.lastEnsureRequest == nil || fakeClient.lastEnsureRequest.CubeVSContext == nil {
		t.Fatal("EnsureNetwork request missing CubeVSContext")
	}
	wantAllowOut := []string{"172.67.0.0/16", "1.1.1.1/32", "8.8.8.8/32"}
	if strings.Join(fakeClient.lastEnsureRequest.CubeVSContext.AllowOut, ",") != strings.Join(wantAllowOut, ",") {
		t.Fatalf("AllowOut=%v, want %v", fakeClient.lastEnsureRequest.CubeVSContext.AllowOut, wantAllowOut)
	}
}

func TestWaitForNetworkAgentReadyRetriesUntilSuccess(t *testing.T) {
	fakeClient := &fakeNetworkAgentClient{
		healthErrs: []error{
			errors.New("connection refused"),
			errors.New("transport is closing"),
		},
	}
	l := &local{
		Config: &Config{
			NetworkAgentEndpoint:      "grpc+unix:///tmp/cube/network-agent-grpc.sock",
			NetworkAgentInitTimeout:   tomlext.FromStdTime(200 * time.Millisecond),
			NetworkAgentRetryInterval: tomlext.FromStdTime(10 * time.Millisecond),
		},
		networkAgentClient: fakeClient,
	}

	if err := l.waitForNetworkAgentReady(context.Background()); err != nil {
		t.Fatalf("waitForNetworkAgentReady error=%v", err)
	}
	if fakeClient.healthCalls < 3 {
		t.Fatalf("healthCalls=%d, want at least 3", fakeClient.healthCalls)
	}
}

func TestMergeDNSAllowOutCIDRs(t *testing.T) {
	block := false
	ctx := &networkagentclient.CubeVSContext{
		AllowInternetAccess: &block,
		AllowOut:            []string{"172.67.0.0/16"},
	}

	got, dnsCIDRs := mergeDNSAllowOutCIDRs(ctx, []string{"1.1.1.1", "2001:4860:4860::8888", "1.1.1.1"})
	if got == nil {
		t.Fatal("mergeDNSAllowOutCIDRs returned nil context")
	}
	if len(dnsCIDRs) != 3 {
		t.Fatalf("dnsCIDRs=%v, want duplicate-preserving raw cidrs for logging", dnsCIDRs)
	}
	wantAllowOut := []string{"172.67.0.0/16", "1.1.1.1/32", "2001:4860:4860::8888/128"}
	if strings.Join(got.AllowOut, ",") != strings.Join(wantAllowOut, ",") {
		t.Fatalf("AllowOut=%v, want %v", got.AllowOut, wantAllowOut)
	}
}

func TestMergeDNSAllowOutCIDRsSkipsOpenInternetContext(t *testing.T) {
	allow := true
	ctx := &networkagentclient.CubeVSContext{AllowInternetAccess: &allow}

	got, dnsCIDRs := mergeDNSAllowOutCIDRs(ctx, []string{"1.1.1.1"})
	if got != ctx {
		t.Fatal("expected original context to be reused for open internet access")
	}
	if len(dnsCIDRs) != 0 {
		t.Fatalf("dnsCIDRs=%v, want empty", dnsCIDRs)
	}
}
