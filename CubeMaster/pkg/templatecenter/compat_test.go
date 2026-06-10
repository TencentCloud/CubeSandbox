// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import "testing"

func TestEvaluateCompat(t *testing.T) {
	tests := []struct {
		name    string
		replica ReplicaStatus
		guest   string
		agent   string
		kernel  string
		want    string
	}{
		{
			name: "all dimensions match",
			replica: ReplicaStatus{
				GuestImageVersion: "v1",
				AgentVersion:      "a1",
				KernelVersion:     "k1",
				CompatPolicy:      CompatPolicyStrict,
			},
			guest:  "v1",
			agent:  "a1",
			kernel: "k1",
			want:   CompatStatusOK,
		},
		{
			name: "guest mismatch is stale",
			replica: ReplicaStatus{
				GuestImageVersion: "v1",
				AgentVersion:      "a1",
				KernelVersion:     "k1",
				CompatPolicy:      CompatPolicyStrict,
			},
			guest:  "v2",
			agent:  "a1",
			kernel: "k1",
			want:   CompatStatusStale,
		},
		{
			name: "kernel mismatch does not require redo",
			replica: ReplicaStatus{
				GuestImageVersion: "v1",
				AgentVersion:      "a1",
				KernelVersion:     "k1",
				CompatPolicy:      CompatPolicyStrict,
			},
			guest:  "v1",
			agent:  "a1",
			kernel: "k2",
			want:   CompatStatusOK,
		},
		{
			name: "missing current agent is unknown, not ok",
			replica: ReplicaStatus{
				GuestImageVersion: "v1",
				AgentVersion:      "a1",
				KernelVersion:     "k1",
				CompatPolicy:      CompatPolicyStrict,
			},
			guest:  "v1",
			agent:  "",
			kernel: "k1",
			want:   CompatStatusUnknown,
		},
		{
			name: "unknown literal is treated as missing",
			replica: ReplicaStatus{
				GuestImageVersion: "v1",
				AgentVersion:      "unknown",
				KernelVersion:     "k1",
				CompatPolicy:      CompatPolicyStrict,
			},
			guest:  "v1",
			agent:  "a1",
			kernel: "k1",
			want:   CompatStatusUnknown,
		},
		{
			name: "guest only policy ignores agent mismatch",
			replica: ReplicaStatus{
				GuestImageVersion: "v1",
				AgentVersion:      "a1",
				KernelVersion:     "k1",
				CompatPolicy:      CompatPolicyGuestOnly,
			},
			guest:  "v1",
			agent:  "a2",
			kernel: "k1",
			want:   CompatStatusOK,
		},
		{
			name: "missing current kernel is still ok",
			replica: ReplicaStatus{
				GuestImageVersion: "v1",
				AgentVersion:      "a1",
				KernelVersion:     "k1",
				CompatPolicy:      CompatPolicyStrict,
			},
			guest:  "v1",
			agent:  "a1",
			kernel: "",
			want:   CompatStatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateCompat(tt.replica, tt.guest, tt.agent, tt.kernel)
			if got != tt.want {
				t.Fatalf("evaluateCompat()=%s, want %s", got, tt.want)
			}
		})
	}
}

func TestBindGuestVersionToReplica(t *testing.T) {
	replica := ReplicaStatus{}
	bindGuestVersionToReplica(&replica, " v1 ", "unknown", "k1")
	if replica.GuestImageVersion != "v1" {
		t.Fatalf("guest version=%q, want v1", replica.GuestImageVersion)
	}
	if replica.AgentVersion != "" {
		t.Fatalf("agent version=%q, want empty", replica.AgentVersion)
	}
	if replica.CompatStatus != CompatStatusUnknown {
		t.Fatalf("compat status=%s, want UNKNOWN", replica.CompatStatus)
	}
}
