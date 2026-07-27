// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package networkagentclient

import (
	"testing"

	networkagentv1 "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/networkagentclient/pb"
)

// TestMapCubeNetworkConfigToProtoDNSEnforceQuery proves the cubelet→network-agent
// proto hop carries dns_enforce_query onto the wire.
func TestMapCubeNetworkConfigToProtoDNSEnforceQuery(t *testing.T) {
	t.Run("enforce flows to proto", func(t *testing.T) {
		enforce := true
		out := mapCubeNetworkConfigToProto(&CubeNetworkConfig{
			AllowOut:        []string{"api.example.com"},
			DnsEnforceQuery: &enforce,
		})
		if out == nil || out.DnsEnforceQuery == nil || !*out.DnsEnforceQuery {
			t.Fatalf("proto DnsEnforceQuery=%v, want true", out)
		}
	})
	t.Run("nil stays nil", func(t *testing.T) {
		out := mapCubeNetworkConfigToProto(&CubeNetworkConfig{
			AllowOut: []string{"api.example.com"},
		})
		if out == nil || out.DnsEnforceQuery != nil {
			t.Fatalf("proto DnsEnforceQuery=%v, want nil", out)
		}
	})
	_ = networkagentv1.CubeNetworkConfig{}
}
