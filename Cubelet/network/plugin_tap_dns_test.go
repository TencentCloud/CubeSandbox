// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package network

import (
	"testing"

	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/networkagentclient"
)

// TestMapRunRequestCubeNetworkConfigDNSEnforceQuery proves the cubelet hop
// forwards dns_enforce_query from the CubeMaster proto into the
// networkagentclient struct handed to network-agent.
func TestMapRunRequestCubeNetworkConfigDNSEnforceQuery(t *testing.T) {
	t.Run("enforce flows through", func(t *testing.T) {
		enforce := true
		out := mapRunRequestCubeNetworkConfig(&cubebox.CubeNetworkConfig{
			AllowOut:        []string{"api.example.com"},
			DnsEnforceQuery: &enforce,
		})
		if out == nil || out.DnsEnforceQuery == nil || !*out.DnsEnforceQuery {
			t.Fatalf("DnsEnforceQuery=%v, want true", out)
		}
	})
	t.Run("nil stays nil", func(t *testing.T) {
		out := mapRunRequestCubeNetworkConfig(&cubebox.CubeNetworkConfig{
			AllowOut: []string{"api.example.com"},
		})
		if out == nil || out.DnsEnforceQuery != nil {
			t.Fatalf("DnsEnforceQuery=%v, want nil", out)
		}
	})
}

// TestCloneNetworkAgentCubeNetworkConfigDNSEnforceQuery proves the clone
// (used by mergeDNSAllowOutCIDRs) copies, not shares, the enforce pointer.
func TestCloneNetworkAgentCubeNetworkConfigDNSEnforceQuery(t *testing.T) {
	enforce := true
	src := &networkagentclient.CubeNetworkConfig{
		AllowOut:        []string{"api.example.com"},
		DnsEnforceQuery: &enforce,
	}
	cloned := cloneNetworkAgentCubeNetworkConfig(src)
	if cloned == nil || cloned.DnsEnforceQuery == nil || !*cloned.DnsEnforceQuery {
		t.Fatalf("cloned DnsEnforceQuery=%v, want true", cloned)
	}
	*cloned.DnsEnforceQuery = false
	if !enforce {
		t.Fatalf("clone shared pointer with source; mutation escaped to src")
	}
}
