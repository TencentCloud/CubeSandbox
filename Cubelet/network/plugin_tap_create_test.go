// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/tomlext"
	networkruntime "github.com/tencentcloud/CubeSandbox/Cubelet/network/runtime"
	networktypes "github.com/tencentcloud/CubeSandbox/Cubelet/network/types"
	networkstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/network"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

type fakeNetworkRuntime struct {
	ensureCalled       bool
	ensureErr          error
	lastEnsureRequest  *networkruntime.EnsureNetworkRequest
	releaseCalled      bool
	lastReleaseRequest *networkruntime.ReleaseNetworkRequest

	lastUpdatePolicyRequest *networkruntime.UpdateNetworkPolicyRequest
	updatePolicyErr         error

	listTaps         []networkruntime.TapState
	dumpPolicies     map[string]map[string]any
	healthErrs       []error
	healthCalls      int
	tapFiles         []*os.File
	getTapFileCalls  int
	lastTapSandboxID string
	lastTapName      string
}

func (c *fakeNetworkRuntime) EnsureNetwork(_ context.Context, req *networkruntime.EnsureNetworkRequest) (*networkruntime.EnsureNetworkResponse, error) {
	c.ensureCalled = true
	c.lastEnsureRequest = req
	if c.ensureErr != nil {
		return nil, c.ensureErr
	}
	return &networkruntime.EnsureNetworkResponse{
		SandboxID:     "sandbox-1",
		NetworkHandle: "sandbox-1",
		Interfaces: []networkruntime.Interface{
			{
				Name:    "z192.168.0.40",
				MAC:     "20:90:6f:fc:fc:fc",
				MTU:     1500,
				IPs:     []string{"169.254.68.6/30"},
				Gateway: "169.254.68.5",
			},
		},
		Routes: []networkruntime.Route{
			{
				Gateway: "169.254.68.5",
				Device:  eth0,
			},
		},
		ARPNeighbors: []networkruntime.ARPNeighbor{
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

func (c *fakeNetworkRuntime) ReleaseNetwork(_ context.Context, req *networkruntime.ReleaseNetworkRequest) (*networkruntime.ReleaseNetworkResponse, error) {
	c.releaseCalled = true
	c.lastReleaseRequest = req
	return &networkruntime.ReleaseNetworkResponse{Released: true, PersistMetadata: req.PersistMetadata}, nil
}

func (c *fakeNetworkRuntime) UpdateNetworkPolicy(_ context.Context, req *networkruntime.UpdateNetworkPolicyRequest) error {
	c.lastUpdatePolicyRequest = req
	return c.updatePolicyErr
}

func (c *fakeNetworkRuntime) ListTaps(_ context.Context, _ *networkruntime.ListTapsRequest) (*networkruntime.ListTapsResponse, error) {
	stateCounts := map[string]int{}
	for _, tap := range c.listTaps {
		stateCounts[tap.State]++
	}
	return &networkruntime.ListTapsResponse{Taps: append([]networkruntime.TapState(nil), c.listTaps...), StateCounts: stateCounts}, nil
}

func (c *fakeNetworkRuntime) Health(context.Context) error {
	if c.healthCalls < len(c.healthErrs) {
		err := c.healthErrs[c.healthCalls]
		c.healthCalls++
		return err
	}
	c.healthCalls++
	return nil
}

func (c *fakeNetworkRuntime) GetTapFile(sandboxID, tapName string) (*os.File, error) {
	c.getTapFileCalls++
	c.lastTapSandboxID = sandboxID
	c.lastTapName = tapName
	if len(c.tapFiles) > 0 {
		file := c.tapFiles[0]
		c.tapFiles = c.tapFiles[1:]
		return file, nil
	}
	return nil, errors.New("tap fd unavailable")
}

func (c *fakeNetworkRuntime) DumpEgressPolicies(context.Context) (map[string]map[string]any, error) {
	if c.dumpPolicies != nil {
		return c.dumpPolicies, nil
	}
	return map[string]map[string]any{}, nil
}

func TestTapCreateWithNetworkRuntimeCallsEnsureNetwork(t *testing.T) {
	fakeClient := &fakeNetworkRuntime{}
	l := &local{
		Config: &Config{
			MVMMacAddr:  "20:90:6f:fc:fc:fc",
			MvmMtu:      1500,
			MvmGwDestIP: "169.254.68.5",
			MVMInnerIP:  "169.254.68.6",
			MvmMask:     30,
		},

		networkRuntime: fakeClient,
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

	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if !fakeClient.ensureCalled {
		t.Fatal("network runtime EnsureNetwork was not called")
	}
	if fakeClient.getTapFileCalls != 0 {
		t.Fatalf("Create called GetTapFile %d times, want lazy shim fd handoff", fakeClient.getTapFileCalls)
	}
	if fakeClient.releaseCalled {
		t.Fatal("network runtime ReleaseNetwork was called despite successful Create")
	}
	shimInfo, ok := opts.NetworkInfo.(*networktypes.ShimNetReq)
	if !ok || shimInfo == nil || len(shimInfo.Interfaces) != 1 || shimInfo.Interfaces[0].Name != "z192.168.0.40" {
		t.Fatalf("NetworkInfo not populated from runtime response: %+v", opts.NetworkInfo)
	}
}

func TestTapCreateReleasesRuntimeAfterCommittedEnsureError(t *testing.T) {
	fakeClient := &fakeNetworkRuntime{ensureErr: errors.Join(
		networkruntime.ErrEnsureNetworkCommitted,
		errors.New("success commit outcome unknown"),
	)}
	l := &local{
		Config: &Config{
			MVMMacAddr:  "20:90:6f:fc:fc:fc",
			MvmMtu:      1500,
			MvmGwDestIP: "169.254.68.5",
			MVMInnerIP:  "169.254.68.6",
			MvmMask:     30,
		},
		networkRuntime: fakeClient,
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "sandbox-ensure-error"},
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			RequestID:    "ensure-error-request",
			InstanceType: cubebox.InstanceType_cubebox.String(),
		},
	}

	if err := l.Create(context.Background(), opts); err == nil {
		t.Fatal("Create succeeded despite runtime EnsureNetwork error")
	}
	if !fakeClient.releaseCalled {
		t.Fatal("Create did not issue idempotent ReleaseNetwork after EnsureNetwork error")
	}
	if fakeClient.lastReleaseRequest == nil ||
		fakeClient.lastReleaseRequest.SandboxID != "sandbox-ensure-error" ||
		fakeClient.lastReleaseRequest.NetworkHandle != "sandbox-ensure-error" ||
		fakeClient.lastReleaseRequest.IdempotencyKey != "ensure-error-request" {
		t.Fatalf("ReleaseNetwork request after EnsureNetwork error = %+v", fakeClient.lastReleaseRequest)
	}
}

func TestTapCreateDoesNotReleaseRuntimeAfterPreCommitEnsureError(t *testing.T) {
	fakeClient := &fakeNetworkRuntime{ensureErr: errors.New("invalid create request")}
	l := &local{
		Config: &Config{
			MVMMacAddr:  "20:90:6f:fc:fc:fc",
			MvmMtu:      1500,
			MvmGwDestIP: "169.254.68.5",
			MVMInnerIP:  "169.254.68.6",
			MvmMask:     30,
		},
		networkRuntime: fakeClient,
	}
	opts := &workflow.CreateContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "sandbox-precommit-error"},
		ReqInfo: &cubebox.RunCubeSandboxRequest{
			RequestID:    "precommit-error-request",
			InstanceType: cubebox.InstanceType_cubebox.String(),
		},
	}

	if err := l.Create(context.Background(), opts); err == nil {
		t.Fatal("Create succeeded despite runtime EnsureNetwork error")
	}
	if fakeClient.releaseCalled {
		t.Fatal("pre-commit EnsureNetwork error triggered a cross-request ReleaseNetwork")
	}
}

func TestGetTapFileForShimGetsFreshRuntimeFDForEveryRequest(t *testing.T) {
	oldDNM := dnm
	defer func() {
		dnm = oldDNM
	}()
	firstFile, err := os.CreateTemp(t.TempDir(), "tap-fd-first-*")
	if err != nil {
		t.Fatal(err)
	}
	defer firstFile.Close()
	secondFile, err := os.CreateTemp(t.TempDir(), "tap-fd-second-*")
	if err != nil {
		t.Fatal(err)
	}
	defer secondFile.Close()
	fakeClient := &fakeNetworkRuntime{
		tapFiles: []*os.File{firstFile, secondFile},
	}
	dnm = &delegateNetworkManager{tapPlugin: &local{networkRuntime: fakeClient}}

	file, err := GetTapFileForShim("sandbox-lazy", "tap-lazy")
	if err != nil {
		t.Fatal(err)
	}
	if file != firstFile {
		t.Fatalf("file=%#v, want %#v", file, firstFile)
	}
	if fakeClient.getTapFileCalls != 1 || fakeClient.lastTapSandboxID != "sandbox-lazy" || fakeClient.lastTapName != "tap-lazy" {
		t.Fatalf("GetTapFile calls=%d sandbox=%q tap=%q", fakeClient.getTapFileCalls, fakeClient.lastTapSandboxID, fakeClient.lastTapName)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close first caller-owned fd: %v", err)
	}

	again, err := GetTapFileForShim("sandbox-lazy", "tap-lazy")
	if err != nil {
		t.Fatal(err)
	}
	if again != secondFile || fakeClient.getTapFileCalls != 2 {
		t.Fatalf("second request did not get a fresh runtime fd: again=%#v calls=%d", again, fakeClient.getTapFileCalls)
	}
}

func TestTapCreateWithNetworkRuntimeAddsDNSAllowOutCIDRsForDomainAllow(t *testing.T) {
	fakeClient := &fakeNetworkRuntime{}
	block := false
	l := &local{
		Config: &Config{
			MVMMacAddr:  "20:90:6f:fc:fc:fc",
			MvmMtu:      1500,
			MvmGwDestIP: "169.254.68.5",
			MVMInnerIP:  "169.254.68.6",
			MvmMask:     30,
		},

		networkRuntime: fakeClient,
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
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if fakeClient.getTapFileCalls != 0 {
		t.Fatalf("Create called GetTapFile %d times, want lazy shim fd handoff", fakeClient.getTapFileCalls)
	}
	if fakeClient.lastEnsureRequest == nil || fakeClient.lastEnsureRequest.CubeNetworkConfig == nil {
		t.Fatal("EnsureNetwork request missing CubeNetworkConfig")
	}
	wantAllowOut := []string{"172.67.0.0/16", "api.example.com", "1.1.1.1/32", "8.8.8.8/32"}
	if strings.Join(fakeClient.lastEnsureRequest.CubeNetworkConfig.AllowOut, ",") != strings.Join(wantAllowOut, ",") {
		t.Fatalf("AllowOut=%v, want %v", fakeClient.lastEnsureRequest.CubeNetworkConfig.AllowOut, wantAllowOut)
	}
}

func TestWaitForNetworkRuntimeReadyReturnsHealthError(t *testing.T) {
	fakeClient := &fakeNetworkRuntime{
		healthErrs: []error{errors.New("runtime not ready")},
	}
	l := &local{
		Config:         &Config{},
		networkRuntime: fakeClient,
	}

	if err := l.waitForNetworkRuntimeReady(context.Background()); err == nil {
		t.Fatal("waitForNetworkRuntimeReady error=nil, want health error")
	}
	if fakeClient.healthCalls != 1 {
		t.Fatalf("healthCalls=%d, want 1", fakeClient.healthCalls)
	}
}

func TestDelegateNetworkManagerRegistersHTTPHandlers(t *testing.T) {
	manager := &delegateNetworkManager{tapPlugin: &local{networkRuntime: &fakeNetworkRuntime{}}}
	var service interface {
		RegisterHTTP(map[string]http.Handler) error
	} = manager

	handlers := map[string]http.Handler{}
	if err := service.RegisterHTTP(handlers); err != nil {
		t.Fatalf("RegisterHTTP returned error: %v", err)
	}
	if handlers[egressPolicyDumpPath] == nil || handlers[networkTapsPath] == nil {
		t.Fatalf("handlers not registered through delegateNetworkManager: %+v", handlers)
	}
}

func TestRegisterHTTPExposesNetworkRuntimeTaps(t *testing.T) {
	fakeClient := &fakeNetworkRuntime{
		listTaps: []networkruntime.TapState{
			{
				SandboxID:      "sandbox-http",
				TapName:        "z192.168.0.80",
				TapIfIndex:     80,
				SandboxIP:      "192.168.0.80",
				State:          string(networkruntime.TapPoolCleaning),
				OwnerSandboxID: "sandbox-http",
				RetryCount:     2,
				LastError:      "cubeegress verify failed",
				PortMappings: []networkruntime.PortMapping{
					{Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 20080, ContainerPort: 8080},
				},
			},
		},
	}
	l := &local{networkRuntime: fakeClient}
	handlers := map[string]http.Handler{}
	if err := l.RegisterHTTP(handlers); err != nil {
		t.Fatalf("RegisterHTTP returned error: %v", err)
	}
	handler := handlers["/v1/network/taps"]
	if handler == nil {
		t.Fatal("/v1/network/taps handler not registered")
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/network/taps", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var resp networkruntime.ListTapsResponse
	if err := json.NewDecoder(recorder.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Taps) != 1 || resp.Taps[0].SandboxID != "sandbox-http" || resp.Taps[0].PortMappings[0].HostPort != 20080 {
		t.Fatalf("unexpected taps response: %+v", resp)
	}
	if resp.Taps[0].State != string(networkruntime.TapPoolCleaning) || resp.Taps[0].RetryCount != 2 || resp.Taps[0].LastError == "" {
		t.Fatalf("taps diagnostic fields missing: %+v", resp.Taps[0])
	}
	if resp.StateCounts[string(networkruntime.TapPoolCleaning)] != 1 {
		t.Fatalf("stateCounts=%v, want one Cleaning tap", resp.StateCounts)
	}
}

func TestNetworkTapsRejectsNonLoopbackClients(t *testing.T) {
	l := &local{networkRuntime: &fakeNetworkRuntime{
		listTaps: []networkruntime.TapState{
			{SandboxID: "sandbox-secret", TapName: "z192.168.0.81", SandboxIP: "192.168.0.81"},
		},
	}}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/network/taps", nil)
	request.RemoteAddr = "192.0.2.10:12345"

	l.handleListNetworkTaps(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want forbidden", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "sandbox-secret") {
		t.Fatalf("tap diagnostics leaked to non-loopback client: %s", recorder.Body.String())
	}
}

func TestRegisterHTTPExposesEgressPoliciesDumpWithBootstrapShape(t *testing.T) {
	fakeClient := &fakeNetworkRuntime{
		dumpPolicies: map[string]map[string]any{
			"192.168.0.10": {
				"policy_id": "policy-1",
				"rules": []any{
					map[string]any{"type": "domain", "value": "example.com"},
				},
			},
		},
	}
	l := &local{networkRuntime: fakeClient}
	handlers := map[string]http.Handler{}
	if err := l.RegisterHTTP(handlers); err != nil {
		t.Fatalf("RegisterHTTP returned error: %v", err)
	}
	handler := handlers["/v1/policies/dump"]
	if handler == nil {
		t.Fatal("/v1/policies/dump handler not registered")
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/policies/dump", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var resp struct {
		Policies map[string]map[string]any `json:"policies"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	policy := resp.Policies["192.168.0.10"]
	if policy == nil {
		t.Fatalf("policies wrapper missing sandbox policy: %+v", resp)
	}
	if policy["policy_id"] != "policy-1" {
		t.Fatalf("policy_id=%v, want policy-1", policy["policy_id"])
	}
}

func TestEgressPoliciesDumpRejectsNonLoopbackClients(t *testing.T) {
	l := &local{networkRuntime: &fakeNetworkRuntime{
		dumpPolicies: map[string]map[string]any{
			"192.168.0.10": {"secret": "must-not-leak"},
		},
	}}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/policies/dump", nil)
	request.RemoteAddr = "192.0.2.10:12345"

	l.handleDumpEgressPolicies(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want forbidden", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "must-not-leak") {
		t.Fatalf("policy secret leaked to non-loopback client: %s", recorder.Body.String())
	}
}

func TestDelegateDestroyCallsTapRuntimeWhenAllocationMissing(t *testing.T) {
	fakeClient := &fakeNetworkRuntime{}
	manager := &delegateNetworkManager{
		tapPlugin: &local{
			Config:          &Config{},
			allocationStore: networkstore.NewStore(nil),
			networkRuntime:  fakeClient,
		},
		allocationStore: networkstore.NewStore(nil),
	}

	err := manager.Destroy(context.Background(), &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "sandbox-missing-allocation"},
		DestroyInfo: &cubebox.DestroyCubeSandboxRequest{
			SandboxID: "sandbox-missing-allocation",
			RequestID: "destroy-missing-allocation",
		},
	})
	if err != nil {
		t.Fatalf("Destroy returned error: %v", err)
	}
	if !fakeClient.releaseCalled {
		t.Fatal("network runtime ReleaseNetwork was not called when allocation metadata was missing")
	}
	if fakeClient.lastReleaseRequest == nil || fakeClient.lastReleaseRequest.SandboxID != "sandbox-missing-allocation" {
		t.Fatalf("ReleaseNetwork request invalid: %+v", fakeClient.lastReleaseRequest)
	}
	if fakeClient.lastReleaseRequest.IdempotencyKey != "destroy-missing-allocation" {
		t.Fatalf("ReleaseNetwork idempotency key=%q, want destroy-missing-allocation", fakeClient.lastReleaseRequest.IdempotencyKey)
	}
}

func TestTapDestroyCallsNetworkRuntimeEvenWhenLocalFDCacheMissing(t *testing.T) {
	fakeClient := &fakeNetworkRuntime{}
	allocationStore := networkstore.NewStore(nil)
	allocationStore.Add(networkstore.NetworkAllocation{
		SandboxID:          "sandbox-destroy",
		NetworkType:        cubebox.NetworkType_tap.String(),
		PersistentMetadata: (&networktypes.ShimNetReq{}).GetPersistMetadata(),
	})
	l := &local{
		Config:          &Config{},
		allocationStore: allocationStore,
		networkRuntime:  fakeClient,
	}

	err := l.Destroy(context.Background(), &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{SandboxID: "sandbox-destroy"},
		DestroyInfo: &cubebox.DestroyCubeSandboxRequest{
			SandboxID: "sandbox-destroy",
			RequestID: "destroy-req",
		},
	})
	if err != nil {
		t.Fatalf("Destroy returned error: %v", err)
	}
	if !fakeClient.releaseCalled {
		t.Fatal("network runtime ReleaseNetwork was not called")
	}
	if fakeClient.lastReleaseRequest == nil || fakeClient.lastReleaseRequest.SandboxID != "sandbox-destroy" {
		t.Fatalf("ReleaseNetwork request invalid: %+v", fakeClient.lastReleaseRequest)
	}
	if fakeClient.lastReleaseRequest.IdempotencyKey != "destroy-req" {
		t.Fatalf("ReleaseNetwork idempotency key=%q, want destroy-req", fakeClient.lastReleaseRequest.IdempotencyKey)
	}
}

func TestNetworkRuntimeConfigFromPluginConfigMapsEmbeddedRuntimeSettings(t *testing.T) {
	cfg := networkRuntimeConfigFromPluginConfig(&Config{
		EthName:               "eth-test",
		ObjectDir:             "/tmp/cubevs",
		CIDR:                  "10.1.0.0/24",
		MVMInnerIP:            "169.254.68.10",
		MVMMacAddr:            "20:90:6f:fc:fc:aa",
		MvmGwDestIP:           "169.254.68.9",
		MvmGwMacAddr:          "20:90:6f:cf:cf:aa",
		MvmMask:               30,
		MvmMtu:                1400,
		TapInitNum:            3,
		CubeEgressAdminURL:    "http://127.0.0.1:19090",
		CubeEgressPushTimeout: tomlext.FromStdTime(3 * time.Second),
		CubeRouterEnable:      true,
		CubeRouterCIDR:        "10.254.0.0/24",
		CubeRouterMacAddr:     "22:90:6f:cf:cf:aa",
	})

	if cfg.CubeEgressAdminURL != "http://127.0.0.1:19090" {
		t.Fatalf("CubeEgressAdminURL=%q", cfg.CubeEgressAdminURL)
	}
	if cfg.CubeEgressPushTimeout != 3*time.Second {
		t.Fatalf("CubeEgressPushTimeout=%v", cfg.CubeEgressPushTimeout)
	}
	if !cfg.CubeRouterEnable || cfg.CubeRouterCIDR != "10.254.0.0/24" || cfg.CubeRouterMacAddr != "22:90:6f:cf:cf:aa" {
		t.Fatalf("CubeRouter config not mapped: enable=%v cidr=%q mac=%q", cfg.CubeRouterEnable, cfg.CubeRouterCIDR, cfg.CubeRouterMacAddr)
	}
}

func TestNetworkRuntimeConfigFromPluginConfigAllowsCubeEgressDisable(t *testing.T) {
	cfg := networkRuntimeConfigFromPluginConfig(&Config{CubeEgressAdminURL: ""})
	if cfg.CubeEgressAdminURL != "" {
		t.Fatalf("CubeEgressAdminURL=%q, want disabled", cfg.CubeEgressAdminURL)
	}
}

func TestShouldAppendDNSAllowOut(t *testing.T) {
	block := false
	allow := true
	host := "api.example.com:443"
	sni := "*.example.com"

	tests := []struct {
		name string
		cfg  *networkruntime.CubeNetworkConfig
		want bool
	}{
		{
			name: "nil config",
			want: false,
		},
		{
			name: "allow_out domain with disabled internet access",
			cfg: &networkruntime.CubeNetworkConfig{
				AllowInternetAccess: &block,
				AllowOut:            []string{"172.67.0.0/16", "api.example.com"},
			},
			want: true,
		},
		{
			name: "allow_out domain with open internet access",
			cfg: &networkruntime.CubeNetworkConfig{
				AllowInternetAccess: &allow,
				AllowOut:            []string{"api.example.com"},
			},
			want: true,
		},
		{
			name: "allow_out domain with default internet access",
			cfg: &networkruntime.CubeNetworkConfig{
				AllowOut: []string{"api.example.com"},
			},
			want: true,
		},
		{
			name: "l7 host domain with disabled internet access",
			cfg: &networkruntime.CubeNetworkConfig{
				AllowInternetAccess: &block,
				Rules: []*networkruntime.EgressRule{
					{Match: &networkruntime.EgressRuleMatch{Host: &host}},
				},
			},
			want: true,
		},
		{
			name: "l7 sni wildcard domain with disabled internet access",
			cfg: &networkruntime.CubeNetworkConfig{
				AllowInternetAccess: &block,
				Rules: []*networkruntime.EgressRule{
					{Match: &networkruntime.EgressRuleMatch{SNI: &sni}},
				},
			},
			want: true,
		},
		{
			name: "l7 host domain with default internet access",
			cfg: &networkruntime.CubeNetworkConfig{
				Rules: []*networkruntime.EgressRule{
					{Match: &networkruntime.EgressRuleMatch{Host: &host}},
				},
			},
			want: true,
		},
		{
			name: "disabled internet access without domain target",
			cfg: &networkruntime.CubeNetworkConfig{
				AllowInternetAccess: &block,
				AllowOut:            []string{"172.67.0.0/16"},
			},
			want: false,
		},
		{
			name: "default internet access without domain target",
			cfg: &networkruntime.CubeNetworkConfig{
				AllowOut: []string{"172.67.0.0/16"},
			},
			want: false,
		},
		{
			name: "open internet without domain target",
			cfg: &networkruntime.CubeNetworkConfig{
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
	cfg := &networkruntime.CubeNetworkConfig{
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
	cfg := &networkruntime.CubeNetworkConfig{
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
	cfg := &networkruntime.CubeNetworkConfig{
		AllowInternetAccess: &block,
		AllowOut:            []string{"172.67.0.0/16"},
		Rules: []*networkruntime.EgressRule{
			{Match: &networkruntime.EgressRuleMatch{Host: &host}},
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
	cfg := &networkruntime.CubeNetworkConfig{
		AllowInternetAccess: &block,
		Rules: []*networkruntime.EgressRule{
			{Match: &networkruntime.EgressRuleMatch{Host: &host}},
			{Match: &networkruntime.EgressRuleMatch{SNI: &sni}},
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
	cfg := &networkruntime.CubeNetworkConfig{AllowInternetAccess: &allow}

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
