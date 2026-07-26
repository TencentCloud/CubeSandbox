// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

func TestCubeNetworkConfigDeepCopy(t *testing.T) {
	allowInternet := false
	allowPublic := false
	mask := "localhost:${PORT}"
	host := "api.example.com"
	audit := "metadata"
	format := "Bearer ${SECRET}"
	original := &CubeNetworkConfig{
		AllowInternetAccess: &allowInternet,
		AllowPublicTraffic:  &allowPublic,
		MaskRequestHost:     &mask,
		AllowOut:            []string{"api.example.com"},
		DenyOut:             []string{"0.0.0.0/0"},
		Rules: []*EgressRule{{
			Name: "api",
			Match: &EgressRuleMatch{
				Host:   &host,
				Method: []string{"GET"},
			},
			Action: &EgressRuleAction{
				Allow: true,
				Audit: &audit,
				Inject: []*EgressRuleInject{{
					Header: "Authorization",
					Secret: "secret",
					Format: &format,
				}},
			},
		}},
	}

	cloned := original.DeepCopy()
	if cloned == nil {
		t.Fatal("DeepCopy returned nil")
	}
	if cloned.AllowInternetAccess == original.AllowInternetAccess ||
		cloned.AllowPublicTraffic == original.AllowPublicTraffic ||
		cloned.MaskRequestHost == original.MaskRequestHost {
		t.Fatal("top-level pointers were not cloned")
	}
	if &cloned.AllowOut[0] == &original.AllowOut[0] || &cloned.DenyOut[0] == &original.DenyOut[0] {
		t.Fatal("CIDR slices were not cloned")
	}
	if cloned.Rules[0] == original.Rules[0] ||
		cloned.Rules[0].Match == original.Rules[0].Match ||
		cloned.Rules[0].Match.Host == original.Rules[0].Match.Host ||
		cloned.Rules[0].Action == original.Rules[0].Action ||
		cloned.Rules[0].Action.Audit == original.Rules[0].Action.Audit ||
		cloned.Rules[0].Action.Inject[0] == original.Rules[0].Action.Inject[0] ||
		cloned.Rules[0].Action.Inject[0].Format == original.Rules[0].Action.Inject[0].Format {
		t.Fatal("nested rule values were not deeply cloned")
	}
}
