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

func TestIsExitReason(t *testing.T) {
	for _, r := range []Reason{ReasonCompleted, ReasonError, ReasonOOMKilled, ReasonSandboxCrashed} {
		if !IsExitReason(r) {
			t.Errorf("%q must count as an observed exit", r)
		}
	}
	for _, r := range []Reason{ReasonLivenessFailed, "PauseFailed", ""} {
		if IsExitReason(r) {
			t.Errorf("%q must not count as an observed exit", r)
		}
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
		{OnFailure, ReasonSandboxCrashed, true},
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

func TestStable(t *testing.T) {
	now := time.Now()
	b := Backoff{StableDuration: time.Minute}

	cases := []struct {
		name        string
		stableSince int64
		want        bool
	}{
		{"too soon", now.Add(-time.Second).UnixNano(), false},
		{"window elapsed", now.Add(-2 * time.Minute).UnixNano(), true},
		// The boundary must be inclusive: a run that has been up exactly
		// StableDuration is stable. The e2e test sleeps stableDuration+2s and
		// expects a reset, so an off-by-one here would be user visible.
		{"exactly at window", now.Add(-time.Minute).UnixNano(), true},
		{"one nanosecond short", now.Add(-time.Minute + time.Nanosecond).UnixNano(), false},
		// Unknown anchor: never guess, or a row written before the anchor
		// existed would clear its streak immediately on upgrade.
		{"zero anchor", 0, false},
		{"negative anchor", -1, false},
	}
	for _, c := range cases {
		if got := b.Stable(c.stableSince, now); got != c.want {
			t.Errorf("%s: Stable = %v, want %v", c.name, got, c.want)
		}
	}

	// StableDuration 0 disables the reset entirely.
	if (Backoff{}).Stable(now.Add(-time.Hour).UnixNano(), now) {
		t.Error("zero StableDuration must disable the reset")
	}
}

func TestFromProtoZeroUsesDefaults(t *testing.T) {
	got := FromProto(&cubebox.RestartBackoffConfig{MaxRestarts: 4})
	def := DefaultBackoff()
	if got.Multiplier != def.Multiplier || got.Jitter != def.Jitter || got.StableDuration != def.StableDuration {
		t.Fatalf("zero fields changed defaults: %+v", got)
	}
	if got.MaxRestarts != 4 {
		t.Fatalf("max restarts %d", got.MaxRestarts)
	}
}

func TestValidateEmptyProbeHandler(t *testing.T) {
	req := &cubebox.RunCubeSandboxRequest{
		RestartPolicy: OnFailure,
		Containers:    []*cubebox.ContainerConfig{{LivenessProbe: &cubebox.LivenessProbe{}}},
	}
	if err := Validate(req); err == nil {
		t.Fatal("empty probe handler")
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
