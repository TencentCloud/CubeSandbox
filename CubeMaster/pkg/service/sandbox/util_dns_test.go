// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"testing"

	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// TestMapCubeNetworkConfigDNSEnforceQuery proves the CubeMaster hop forwards
// dns_enforce_query from the HTTP API struct into the Cubelet gRPC proto.
func TestMapCubeNetworkConfigDNSEnforceQuery(t *testing.T) {
	t.Run("enforce flows to proto", func(t *testing.T) {
		out := mapCubeNetworkConfig(&types.CubeNetworkConfig{
			AllowOut:        []string{"api.example.com"},
			DnsEnforceQuery: boolPtrTrue(),
		})
		if out == nil || out.DnsEnforceQuery == nil || !*out.DnsEnforceQuery {
			t.Fatalf("proto DnsEnforceQuery=%v, want true", out)
		}
	})
	t.Run("nil stays nil", func(t *testing.T) {
		out := mapCubeNetworkConfig(&types.CubeNetworkConfig{
			AllowOut: []string{"api.example.com"},
		})
		if out == nil || out.DnsEnforceQuery != nil {
			t.Fatalf("proto DnsEnforceQuery=%v, want nil", out)
		}
	})
	_ = cubebox.CubeNetworkConfig{}
}

func boolPtrTrue() *bool { b := true; return &b }
