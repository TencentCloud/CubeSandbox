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
				MTU:     1500,
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
			MvmMtu:             1500,
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

func TestTapCreateInNetworkAgentModeAddsDNSAllowOutCIDRsForDomainAllow(t *testing.T) {
	fakeClient := &fakeNetworkAgentClient{}
	block := false
	l := &local{
		Config: &Config{
			EnableNetworkAgent: true,
			MVMMacAddr:         "20:90:6f:fc:fc:fc",
			MvmMtu:             1500,
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
		CubeNetworkConfig: &cubebox.CubeNetworkConfig{
			AllowInternetAccess: &block,
			AllowOut:            []string{"172.67.0.0/16", "api.example.com"},
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
	if fakeClient.lastEnsureRequest == nil || fakeClient.lastEnsureRequest.CubeNetworkConfig == nil {
		t.Fatal("EnsureNetwork request missing CubeNetworkConfig")
	}
	wantAllowOut := []string{"172.67.0.0/16", "api.example.com", "1.1.1.1/32", "8.8.8.8/32"}
	if strings.Join(fakeClient.lastEnsureRequest.CubeNetworkConfig.AllowOut, ",") != strings.Join(wantAllowOut, ",") {
		t.Fatalf("AllowOut=%v, want %v", fakeClient.lastEnsureRequest.CubeNetworkConfig.AllowOut, wantAllowOut)
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

func TestShouldAppendDNSAllowOut(t *testing.T) {
	block := false
	allow := true
	host := "api.example.com:443"
	sni := "*.example.com"

	tests := []struct {
		name string
		cfg  *networkagentclient.CubeNetworkConfig
		want bool
	}{
		{
			name: "nil config",
			want: false,
		},
		{
			name: "allow_out domain with disabled internet access",
			cfg: &networkagentclient.CubeNetworkConfig{
				AllowInternetAccess: &block,
				AllowOut:            []string{"172.67.0.0/16", "api.example.com"},
			},
			want: true,
		},
		{
			name: "allow_out domain with open internet access",
			cfg: &networkagentclient.CubeNetworkConfig{
				AllowInternetAccess: &allow,
				AllowOut:            []string{"api.example.com"},
			},
			want: true,
		},
		{
			name: "allow_out domain with default internet access",
			cfg: &networkagentclient.CubeNetworkConfig{
				AllowOut: []string{"api.example.com"},
			},
			want: true,
		},
		{
			name: "l7 host domain with disabled internet access",
			cfg: &networkagentclient.CubeNetworkConfig{
				AllowInternetAccess: &block,
				Rules: []*networkagentclient.EgressRule{
					{Match: &networkagentclient.EgressRuleMatch{Host: &host}},
				},
			},
			want: true,
		},
		{
			name: "l7 sni wildcard domain with disabled internet access",
			cfg: &networkagentclient.CubeNetworkConfig{
				AllowInternetAccess: &block,
				Rules: []*networkagentclient.EgressRule{
					{Match: &networkagentclient.EgressRuleMatch{SNI: &sni}},
				},
			},
			want: true,
		},
		{
			name: "l7 host domain with default internet access",
			cfg: &networkagentclient.CubeNetworkConfig{
				Rules: []*networkagentclient.EgressRule{
					{Match: &networkagentclient.EgressRuleMatch{Host: &host}},
				},
			},
			want: true,
		},
		{
			name: "disabled internet access without domain target",
			cfg: &networkagentclient.CubeNetworkConfig{
				AllowInternetAccess: &block,
				AllowOut:            []string{"172.67.0.0/16"},
			},
			want: false,
		},
		{
			name: "default internet access without domain target",
			cfg: &networkagentclient.CubeNetworkConfig{
				AllowOut: []string{"172.67.0.0/16"},
			},
			want: false,
		},
		{
			name: "open internet without domain target",
			cfg: &networkagentclient.CubeNetworkConfig{
				AllowInternetAccess: &allow,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAppendDNSAllowOut(tt.cfg); got != tt.want {
				t.Fatalf("shouldAppendDNSAllowOut()=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestMergeDNSAllowOutCIDRsForAllowOutDomain(t *testing.T) {
	block := false
	cfg := &networkagentclient.CubeNetworkConfig{
		AllowInternetAccess: &block,
		AllowOut:            []string{"172.67.0.0/16", "api.example.com"},
	}

	got, dnsCIDRs := mergeDNSAllowOutCIDRs(context.Background(), cfg, []string{"1.1.1.1", "2001:4860:4860::8888", "1.1.1.1"})
	if got == nil {
		t.Fatal("mergeDNSAllowOutCIDRs returned nil config")
	}
	if len(dnsCIDRs) != 2 {
		t.Fatalf("dnsCIDRs=%v, want duplicate-preserving IPv4 cidrs for logging", dnsCIDRs)
	}
	wantAllowOut := []string{"172.67.0.0/16", "api.example.com", "1.1.1.1/32"}
	if strings.Join(got.AllowOut, ",") != strings.Join(wantAllowOut, ",") {
		t.Fatalf("AllowOut=%v, want %v", got.AllowOut, wantAllowOut)
	}
}

func TestMergeDNSAllowOutCIDRsSkipsWithoutDomainAllow(t *testing.T) {
	block := false
	cfg := &networkagentclient.CubeNetworkConfig{
		AllowInternetAccess: &block,
		DenyOut:             []string{"0.0.0.0/0"},
	}

	got, dnsCIDRs := mergeDNSAllowOutCIDRs(context.Background(), cfg, []string{"1.1.1.1"})
	if got != cfg {
		t.Fatal("expected original config to be reused when no domain is allowed")
	}
	if len(dnsCIDRs) != 0 {
		t.Fatalf("dnsCIDRs=%v, want empty", dnsCIDRs)
	}
	if len(got.AllowOut) != 0 {
		t.Fatalf("AllowOut=%v, want empty", got.AllowOut)
	}
}

func TestMergeDNSAllowOutCIDRsForL7DomainRule(t *testing.T) {
	block := false
	host := "api.example.com:443"
	cfg := &networkagentclient.CubeNetworkConfig{
		AllowInternetAccess: &block,
		AllowOut:            []string{"172.67.0.0/16"},
		Rules: []*networkagentclient.EgressRule{
			{Match: &networkagentclient.EgressRuleMatch{Host: &host}},
		},
	}

	got, dnsCIDRs := mergeDNSAllowOutCIDRs(context.Background(), cfg, []string{"8.8.8.8"})
	if got == nil {
		t.Fatal("mergeDNSAllowOutCIDRs returned nil config")
	}
	if len(dnsCIDRs) != 1 || dnsCIDRs[0] != "8.8.8.8/32" {
		t.Fatalf("dnsCIDRs=%v, want [8.8.8.8/32]", dnsCIDRs)
	}
	wantAllowOut := []string{"172.67.0.0/16", "8.8.8.8/32"}
	if strings.Join(got.AllowOut, ",") != strings.Join(wantAllowOut, ",") {
		t.Fatalf("AllowOut=%v, want %v", got.AllowOut, wantAllowOut)
	}
}

func TestMergeDNSAllowOutCIDRsForL7WildcardRules(t *testing.T) {
	block := false
	host := "*.moonshot.cn"
	sni := "*.example.com"
	cfg := &networkagentclient.CubeNetworkConfig{
		AllowInternetAccess: &block,
		Rules: []*networkagentclient.EgressRule{
			{Match: &networkagentclient.EgressRuleMatch{Host: &host}},
			{Match: &networkagentclient.EgressRuleMatch{SNI: &sni}},
		},
	}

	got, dnsCIDRs := mergeDNSAllowOutCIDRs(context.Background(), cfg, []string{"119.29.29.29"})
	if got == nil {
		t.Fatal("mergeDNSAllowOutCIDRs returned nil config")
	}
	if len(dnsCIDRs) != 1 || dnsCIDRs[0] != "119.29.29.29/32" {
		t.Fatalf("dnsCIDRs=%v, want [119.29.29.29/32]", dnsCIDRs)
	}
	wantAllowOut := []string{"119.29.29.29/32"}
	if strings.Join(got.AllowOut, ",") != strings.Join(wantAllowOut, ",") {
		t.Fatalf("AllowOut=%v, want %v", got.AllowOut, wantAllowOut)
	}
}

func TestMergeDNSAllowOutCIDRsSkipsOpenInternetContext(t *testing.T) {
	allow := true
	cfg := &networkagentclient.CubeNetworkConfig{AllowInternetAccess: &allow}

	got, dnsCIDRs := mergeDNSAllowOutCIDRs(context.Background(), cfg, []string{"1.1.1.1"})
	if got != cfg {
		t.Fatal("expected original config to be reused for open internet access")
	}
	if len(dnsCIDRs) != 0 {
		t.Fatalf("dnsCIDRs=%v, want empty", dnsCIDRs)
	}
}

func TestDNSServerToCIDR(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{name: "ipv4", in: "1.1.1.1", want: "1.1.1.1/32", ok: true},
		{name: "trimmed ipv4", in: " 8.8.8.8 ", want: "8.8.8.8/32", ok: true},
		{name: "ipv6 unsupported by cubevs allow_out", in: "2001:4860:4860::8888", ok: false},
		{name: "invalid", in: "not-an-ip", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := dnsServerToCIDR(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("dnsServerToCIDR(%q)=(%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestIsIPv6DNSServer(t *testing.T) {
	if !isIPv6DNSServer("2001:4860:4860::8888") {
		t.Fatal("expected IPv6 DNS server to be detected")
	}
	if isIPv6DNSServer("1.1.1.1") {
		t.Fatal("did not expect IPv4 DNS server to be detected as IPv6")
	}
	if isIPv6DNSServer("not-an-ip") {
		t.Fatal("did not expect invalid DNS server to be detected as IPv6")
	}
}
