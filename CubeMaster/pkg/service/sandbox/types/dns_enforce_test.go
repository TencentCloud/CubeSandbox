// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

// TestCreateCubeSandboxReqUnmarshalDNSEnforceQuery verifies FastestJsoniter
// populates CubeNetworkConfig.DnsEnforceQuery from the camelCase wire key.
func TestCreateCubeSandboxReqUnmarshalDNSEnforceQuery(t *testing.T) {
	body := []byte(`{"requestID":"r1","cube_network_config":{"allowInternetAccess":false,"allowOut":["api.github.com"],"denyOut":["0.0.0.0/0"],"dnsEnforceQuery":true}}`)
	var req CreateCubeSandboxReq
	if err := req.UnmarshalJSON(body); err != nil {
		t.Fatalf("UnmarshalJSON error=%v", err)
	}
	if req.CubeNetworkConfig == nil {
		t.Fatal("CubeNetworkConfig=nil")
	}
	if req.CubeNetworkConfig.DnsEnforceQuery == nil || !*req.CubeNetworkConfig.DnsEnforceQuery {
		t.Fatalf("DnsEnforceQuery=%v, want true (deserialization dropped it)", req.CubeNetworkConfig.DnsEnforceQuery)
	}
	if len(req.CubeNetworkConfig.AllowOut) != 1 || req.CubeNetworkConfig.AllowOut[0] != "api.github.com" {
		t.Fatalf("AllowOut=%v, want [api.github.com]", req.CubeNetworkConfig.AllowOut)
	}
}

// TestCreateCubeSandboxReqFastestJsoniterDNSEnforceQuery mirrors the LIVE decode
// path (common.GetBodyReq uses FastestJsoniter.Unmarshal, not UnmarshalJSON).
func TestCreateCubeSandboxReqFastestJsoniterDNSEnforceQuery(t *testing.T) {
	body := []byte(`{"RequestID":"r1","instance_type":"cubebox","containers":[],"network_type":"tap","cube_network_config":{"allowInternetAccess":false,"allowOut":["api.github.com"],"denyOut":["0.0.0.0/0"],"dnsEnforceQuery":true}}`)
	req := &CreateCubeSandboxReq{}
	if err := FastestJsoniter.Unmarshal(body, req); err != nil {
		t.Fatalf("FastestJsoniter.Unmarshal error=%v", err)
	}
	if req.CubeNetworkConfig == nil || req.CubeNetworkConfig.DnsEnforceQuery == nil || !*req.CubeNetworkConfig.DnsEnforceQuery {
		t.Fatalf("DnsEnforceQuery=%v, want true (jsoniter live-path dropped it)", req.CubeNetworkConfig)
	}
}
