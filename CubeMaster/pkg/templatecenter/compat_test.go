// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"testing"
)

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
				ShimVersion:       "s1",
				CompatPolicy:      CompatPolicyStrict,
			},
			guest:  "v1",
			agent:  "a1",
			kernel: "k1",
			want:   CompatStatusOK,
		},
		{
			name: "pinned replica stays ok when live guest differs",
			replica: ReplicaStatus{
				GuestImageVersion: "v1",
				AgentVersion:      "a1",
				KernelVersion:     "k1",
				ShimVersion:       "s1",
				CompatPolicy:      CompatPolicyStrict,
			},
			guest:  "v2",
			agent:  "a1",
			kernel: "k1",
			want:   CompatStatusOK,
		},
		{
			name: "unpinned replica is unknown",
			replica: ReplicaStatus{
				CompatPolicy: CompatPolicyStrict,
			},
			guest:  "v2",
			agent:  "a2",
			kernel: "k2",
			want:   CompatStatusUnknown,
		},
		{
			name: "empty guest pin is unknown even when agent is set",
			replica: ReplicaStatus{
				AgentVersion:  "a1",
				KernelVersion: "k1",
				ShimVersion:   "s1",
				CompatPolicy:  CompatPolicyStrict,
			},
			guest:  "v2",
			agent:  "a1",
			kernel: "k1",
			want:   CompatStatusUnknown,
		},
		{
			name: "pre-multiversion guest agent kernel without shim is unknown",
			replica: ReplicaStatus{
				GuestImageVersion: "v0.6.0",
				AgentVersion:      "v0.6.0",
				KernelVersion:     "k1",
				CompatPolicy:      CompatPolicyStrict,
			},
			guest:  "v0.6.0-test7",
			agent:  "v0.6.0-test7",
			kernel: "k2",
			want:   CompatStatusUnknown,
		},
		{
			name: "guest agent shim without kernel is unknown",
			replica: ReplicaStatus{
				GuestImageVersion: "v1",
				AgentVersion:      "a1",
				ShimVersion:       "s1",
				CompatPolicy:      CompatPolicyStrict,
			},
			guest:  "v2",
			agent:  "a2",
			kernel: "k2",
			want:   CompatStatusUnknown,
		},
		{
			name: "kernel mismatch does not require redo",
			replica: ReplicaStatus{
				GuestImageVersion: "v1",
				AgentVersion:      "a1",
				KernelVersion:     "k1",
				ShimVersion:       "s1",
				CompatPolicy:      CompatPolicyStrict,
			},
			guest:  "v1",
			agent:  "a1",
			kernel: "k2",
			want:   CompatStatusOK,
		},
		{
			name: "pinned replica stays ok when current versions are empty",
			replica: ReplicaStatus{
				GuestImageVersion: "v1",
				AgentVersion:      "a1",
				KernelVersion:     "k1",
				ShimVersion:       "s1",
				CompatPolicy:      CompatPolicyStrict,
			},
			guest:  "",
			agent:  "",
			kernel: "",
			want:   CompatStatusOK,
		},
		{
			name: "unknown literal is treated as missing",
			replica: ReplicaStatus{
				GuestImageVersion: "v1",
				AgentVersion:      "unknown",
				KernelVersion:     "k1",
				ShimVersion:       "s1",
				CompatPolicy:      CompatPolicyStrict,
			},
			guest:  "v1",
			agent:  "a1",
			kernel: "k1",
			want:   CompatStatusUnknown,
		},
		{
			name: "guest only policy with restore pins stays ok when agent differs",
			replica: ReplicaStatus{
				GuestImageVersion: "v1",
				AgentVersion:      "a1",
				KernelVersion:     "k1",
				ShimVersion:       "s1",
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
				ShimVersion:       "s1",
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
	bindGuestVersionToReplica(&replica, " v1 ", "unknown", "k1", "shim-1")
	if replica.GuestImageVersion != "v1" {
		t.Fatalf("guest version=%q, want v1", replica.GuestImageVersion)
	}
	if replica.AgentVersion != "" {
		t.Fatalf("agent version=%q, want empty", replica.AgentVersion)
	}
	if replica.ShimVersion != "shim-1" {
		t.Fatalf("shim version=%q, want shim-1", replica.ShimVersion)
	}
	if replica.CompatStatus != CompatStatusUnknown {
		t.Fatalf("compat status=%s, want UNKNOWN", replica.CompatStatus)
	}
}

func TestIsReplicaSchedulableAllowsStale(t *testing.T) {
	readyStale := ReplicaStatus{Status: ReplicaStatusReady, CompatStatus: CompatStatusStale}
	if !isReplicaSchedulable(readyStale) {
		t.Fatal("READY+STALE replica must remain schedulable")
	}
	if !isReplicaSchedulableNow(context.Background(), readyStale) {
		t.Fatal("READY+STALE replica must remain schedulable now")
	}
	if isReplicaSchedulable(ReplicaStatus{Status: ReplicaStatusFailed, CompatStatus: CompatStatusOK}) {
		t.Fatal("non-READY replica must not be schedulable")
	}
}
