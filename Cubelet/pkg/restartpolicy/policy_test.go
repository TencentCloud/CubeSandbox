// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package restartpolicy

import (
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestReasonForTaskExit(t *testing.T) {
	if got := ReasonForTaskExit(0, 0); got != ReasonSandboxCrashed {
		t.Fatalf("missing shim pid: %s", got)
	}
	if got := ReasonForTaskExit(42, 0); got != ReasonCompleted {
		t.Fatalf("guest exit 0: %s", got)
	}
	if got := ReasonForTaskExit(42, 9); got != ReasonError {
		t.Fatalf("guest exit 9: %s", got)
	}
	if got := ReasonForTaskExit(0, 9); got != ReasonError {
		t.Fatalf("exit 9 without pid: %s", got)
	}
}

func TestShouldRestart(t *testing.T) {
	cases := []struct {
		policy string
		reason Reason
		want   bool
	}{
		{Never, ReasonError, false},
		{Never, ReasonCompleted, false},
		{OnFailure, ReasonCompleted, false},
		{OnFailure, ReasonError, true},
		{OnFailure, ReasonOOMKilled, true},
		{OnFailure, ReasonLivenessFailed, true},
		{Always, ReasonCompleted, true},
		{Always, ReasonError, true},
		{"OnFailure", ReasonError, true},
		{"always", ReasonCompleted, true},
		{"nope", ReasonError, false},
	}
	for _, tc := range cases {
		if got := ShouldRestart(tc.policy, tc.reason); got != tc.want {
			t.Errorf("ShouldRestart(%q, %q)=%v want %v", tc.policy, tc.reason, got, tc.want)
		}
	}
}

func TestBackoffDelayCaps(t *testing.T) {
	b := Backoff{Initial: 10 * time.Second, Max: 30 * time.Second, Multiplier: 2}
	if d := b.Delay(0); d != 10*time.Second {
		t.Fatalf("first delay %s", d)
	}
	if d := b.Delay(5); d != 30*time.Second {
		t.Fatalf("capped delay %s", d)
	}
	huge := DefaultBackoff()
	huge.Jitter = 0
	if d := huge.Delay(64); d != huge.Max {
		t.Fatalf("overflow delay %s", d)
	}
	if !b.Allow(100) {
		t.Fatal("zero max restarts is unlimited")
	}
	b.MaxRestarts = 2
	if !b.Allow(1) || b.Allow(2) {
		t.Fatalf("cap not applied")
	}
}

func TestResetCount(t *testing.T) {
	b := Backoff{StableDuration: time.Minute}
	now := time.Now()
	if b.ResetCount(now.Add(-time.Second).UnixNano(), now) {
		t.Fatal("too soon")
	}
	if !b.ResetCount(now.Add(-2*time.Minute).UnixNano(), now) {
		t.Fatal("stable window elapsed")
	}
}

func TestValidateNeverWithProbe(t *testing.T) {
	req := &cubebox.RunCubeSandboxRequest{
		RestartPolicy: Never,
		Containers: []*cubebox.ContainerConfig{{
			LivenessProbe: &cubebox.LivenessProbe{
				ProbeHandler: &cubebox.ProbeHandler{HttpGet: &cubebox.HTTPGetAction{Port: 80}},
			},
		}},
	}
	if err := Validate(req); err == nil {
		t.Fatal("expected Never+liveness to fail")
	}
	bad := "@127.0.0.1/"
	req.Containers[0].LivenessProbe.ProbeHandler.HttpGet.Path = &bad
	req.RestartPolicy = "OnFailure"
	if err := Validate(req); err == nil {
		t.Fatal("expected host-like path to fail")
	}
	okPath := "/health"
	req.Containers[0].LivenessProbe.ProbeHandler.HttpGet.Path = &okPath
	if err := Apply(req); err != nil {
		t.Fatal(err)
	}
	if req.GetLabels()[Label] != OnFailure {
		t.Fatalf("label not stamped: %+v", req.GetLabels())
	}
}
