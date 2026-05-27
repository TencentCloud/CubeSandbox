// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
)

func TestDescribeProbe(t *testing.T) {
	tests := []struct {
		name string
		probe *cubebox.Probe
		want  string
	}{
		{
			name:  "nil probe",
			probe: nil,
			want:  "no probe configured",
		},
		{
			name:  "nil handler",
			probe: &cubebox.Probe{},
			want:  "no probe handler",
		},
		{
			name: "tcp socket probe",
			probe: &cubebox.Probe{
				ProbeHandler: &cubebox.ProbeHandler{
					TcpSocket: &cubebox.TCPSocketAction{
						Port: 49983,
					},
				},
			},
			want: "tcp_socket:49983",
		},
		{
			name: "http get probe",
			probe: &cubebox.Probe{
				ProbeHandler: &cubebox.ProbeHandler{
					HttpGet: &cubebox.HTTPGetAction{
						Port: 8080,
						Path: "/health",
					},
				},
			},
			want: "http_get::8080/health",
		},
		{
			name: "http get probe with host",
			probe: &cubebox.Probe{
				ProbeHandler: &cubebox.ProbeHandler{
					HttpGet: &cubebox.HTTPGetAction{
						Host: "10.0.0.1",
						Port: 9000,
						Path: "/ready",
					},
				},
			},
			want: "http_get:10.0.0.1:9000/ready",
		},
		{
			name: "unknown handler type",
			probe: &cubebox.Probe{
				ProbeHandler: &cubebox.ProbeHandler{},
			},
			want: "unknown probe handler",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := describeProbe(tt.probe)
			if got != tt.want {
				t.Errorf("describeProbe() = %q, want %q", got, tt.want)
			}
		})
	}
}
