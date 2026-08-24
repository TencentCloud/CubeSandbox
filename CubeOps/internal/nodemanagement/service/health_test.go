// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/service"
)

func TestReadyConditionTrue(t *testing.T) {
	cases := []struct {
		name string
		cond []model.NodeCondition
		want bool
	}{
		{"ready", []model.NodeCondition{{Type: "Ready", Status: "True"}}, true},
		{"not ready", []model.NodeCondition{{Type: "Ready", Status: "False"}}, false},
		{"empty", []model.NodeCondition{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := service.ReadyConditionTrue(tc.cond); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEvaluateHealth(t *testing.T) {
	now := time.Now()
	timeout := 15 * time.Second // MetadataTimeout() = 15s cubelet heartbeat window
	cases := []struct {
		name       string
		ready      bool
		hb         time.Time
		want       bool
		wantReason string
	}{
		{"healthy", true, now, true, ""},
		{"expired", true, now.Add(-timeout - time.Second), false, model.ReasonHeartbeatExpired},
		{"not ready", false, now, false, model.ReasonReportedNotReady},
		{"zero heartbeat", true, time.Time{}, false, model.ReasonHeartbeatExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := service.EvaluateHealth(tc.ready, tc.hb, now, timeout)
			if s.Healthy != tc.want || s.UnhealthyReason != tc.wantReason {
				t.Errorf("got %+v, want healthy=%v reason=%s", s, tc.want, tc.wantReason)
			}
		})
	}
}

func TestMetadataTimeout(t *testing.T) {
	// 15s cubelet heartbeat expiry window; independent of CubeMaster sync interval.
	if got := service.MetadataTimeout(); got != 15*time.Second {
		t.Errorf("MetadataTimeout() = %v, want 15s", got)
	}
}
