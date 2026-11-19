// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package config

import (
	"math"
	"testing"
)

func TestValidateHostOvercommitFactorBounds(t *testing.T) {
	cases := []struct {
		name    string
		cpu     float64
		mem     float64
		wantErr bool
	}{
		{name: "zero means default", cpu: 0, mem: 0},
		{name: "historical defaults", cpu: 2.0, mem: 1.25},
		{name: "at cpu ceiling", cpu: maxHostCPUOvercommitFactor, mem: 1.0},
		{name: "at mem ceiling", cpu: 1.0, mem: maxHostMemOvercommitFactor},
		{name: "cpu above ceiling", cpu: maxHostCPUOvercommitFactor + 0.1, mem: 1.0, wantErr: true},
		{name: "mem above ceiling", cpu: 1.0, mem: maxHostMemOvercommitFactor + 0.1, wantErr: true},
		{name: "cpu negative", cpu: -0.1, mem: 1.0, wantErr: true},
		{name: "mem negative", cpu: 1.0, mem: -1, wantErr: true},
		{name: "cpu NaN", cpu: math.NaN(), mem: 1.0, wantErr: true},
		{name: "mem Inf", cpu: 1.0, mem: math.Inf(1), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(&Config{
				HostConf: &HostConf{
					Quota: HostConfigQuota{
						CpuOvercommitFactor: tc.cpu,
						MemOvercommitFactor: tc.mem,
					},
				},
			})
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
