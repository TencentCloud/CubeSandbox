// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package restartpolicy is the single place that decides whether a sandbox
// restarts and how long to wait. Plugins and the cubebox service ask it;
// they do not re-implement the policy.
package restartpolicy

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/sandboxrestart"
)

const (
	// Label is copied onto the sandbox so a restarted Cubelet can recover
	// the policy without trusting a free-form annotation.
	Label = "cube.sandbox.restart-policy"

	Never     = sandboxrestart.WireNever
	OnFailure = sandboxrestart.WireOnFailure
	Always    = sandboxrestart.WireAlways

	StateRunning    = "Running"
	StateRestarting = "Restarting"
	StateBackOff    = "BackOff"
	StateGaveUp     = "GaveUp"
	// Succeeded and Failed are terminal: the process exited and the policy
	// does not rebuild the sandbox. They match the design state machine.
	StateSucceeded = "Succeeded"
	StateFailed    = "Failed"
)

// Reason is why the sandbox stopped. Completed is a zero exit; everything
// else is a failure. OnFailure restarts only failures; Always restarts both.
type Reason string

const (
	ReasonCompleted      Reason = "Completed"
	ReasonError          Reason = "Error"
	ReasonOOMKilled      Reason = "OOMKilled"
	ReasonSandboxCrashed Reason = "SandboxCrashed"
	ReasonLivenessFailed Reason = "LivenessProbeFailed"
)

// Normalize accepts k8s names and the on-wire RESTART_POLICY_* values.
func Normalize(raw string) (string, bool) {
	return sandboxrestart.Normalize(raw)
}

// KeepsRequest reports whether the original create request must be stored
// so a later restart can rebuild the sandbox.
func KeepsRequest(policy string) bool {
	p, ok := Normalize(policy)
	return ok && p != Never
}

// ShouldRestart is the k8s rule: Always restarts every exit, OnFailure
// restarts everything except a zero exit, Never never restarts.
func ShouldRestart(policy string, reason Reason) bool {
	p, ok := Normalize(policy)
	if !ok {
		return false
	}
	switch p {
	case Always:
		return true
	case OnFailure:
		return reason != ReasonCompleted
	default:
		return false
	}
}

// ReasonForExit maps a process exit to a restart reason.
func ReasonForExit(exitCode int32) Reason {
	if exitCode == 0 {
		return ReasonCompleted
	}
	return ReasonError
}

// ReasonForTaskExit maps a containerd TaskExit. A shim that disappears
// reports pid 0 and exit status 0. That is a crash, not a guest that
// finished successfully. A real zero exit always carries the shim pid.
func ReasonForTaskExit(pid, exitCode uint32) Reason {
	if pid == 0 && exitCode == 0 {
		return ReasonSandboxCrashed
	}
	return ReasonForExit(int32(exitCode))
}

// Backoff is the kubelet-shaped delay between restarts.
type Backoff struct {
	Initial        time.Duration
	Max            time.Duration
	Multiplier     float64
	MaxRestarts    int32
	Jitter         float64
	StableDuration time.Duration
}

// DefaultBackoff matches kubelet: 10s, x2, cap 300s, no restart cap,
// reset after 10 minutes of stability. Jitter is 10%.
func DefaultBackoff() Backoff {
	return Backoff{
		Initial:        10 * time.Second,
		Max:            5 * time.Minute,
		Multiplier:     2,
		MaxRestarts:    0,
		Jitter:         0.1,
		StableDuration: 10 * time.Minute,
	}
}

// FromProto overlays non-zero request fields on the default backoff.
func FromProto(cfg *cubebox.RestartBackoffConfig) Backoff {
	b := DefaultBackoff()
	if cfg == nil {
		return b
	}
	if cfg.GetInitialIntervalSecond() > 0 {
		b.Initial = time.Duration(cfg.GetInitialIntervalSecond()) * time.Second
	}
	if cfg.GetMaxIntervalSecond() > 0 {
		b.Max = time.Duration(cfg.GetMaxIntervalSecond()) * time.Second
	}
	if cfg.GetMultiplier() > 0 {
		b.Multiplier = cfg.GetMultiplier()
	}
	b.MaxRestarts = cfg.GetMaxRestarts()
	if cfg.GetJitter() > 0 {
		b.Jitter = cfg.GetJitter()
		if b.Jitter > 1 {
			b.Jitter = 1
		}
	}
	if cfg.GetStableDurationSecond() > 0 {
		b.StableDuration = time.Duration(cfg.GetStableDurationSecond()) * time.Second
	}
	return b
}

// Allow reports whether another restart is permitted. MaxRestarts 0 is unlimited.
func (b Backoff) Allow(restartCount int32) bool {
	if b.MaxRestarts <= 0 {
		return true
	}
	return restartCount < b.MaxRestarts
}

// Delay is the wait before the next attempt. restartCount is how many
// restarts already happened.
func (b Backoff) Delay(restartCount int32) time.Duration {
	raw := float64(b.Initial)
	if restartCount > 0 && b.Multiplier > 0 {
		raw = float64(b.Initial) * math.Pow(b.Multiplier, float64(restartCount))
	}
	maxRaw := float64(b.Max)
	if math.IsNaN(raw) || math.IsInf(raw, 0) || raw < 0 || (b.Max > 0 && raw > maxRaw) {
		if b.Max > 0 {
			raw = maxRaw
		} else if raw < 0 || math.IsNaN(raw) || math.IsInf(raw, 0) {
			raw = 0
		}
	}
	base := time.Duration(raw)
	if b.Jitter <= 0 || base <= 0 {
		return base
	}
	return base + time.Duration(float64(base)*b.Jitter*rand.Float64())
}

// ResetCount reports whether the sandbox has been stable long enough to
// forget the restart streak.
func (b Backoff) ResetCount(lastRestartAt int64, now time.Time) bool {
	if b.StableDuration <= 0 || lastRestartAt <= 0 {
		return false
	}
	return now.Sub(time.Unix(0, lastRestartAt)) >= b.StableDuration
}

// Validate checks policy, backoff and liveness fields. Never combined with
// a liveness probe is rejected: a probe that cannot restart would only kill.
func Validate(req *cubebox.RunCubeSandboxRequest) error {
	if req == nil {
		return nil
	}
	policy, ok := Normalize(req.GetRestartPolicy())
	if !ok {
		return fmt.Errorf("invalid restart policy %q", req.GetRestartPolicy())
	}
	if cfg := req.GetRestartBackoff(); cfg != nil {
		if cfg.GetMultiplier() < 0 {
			return fmt.Errorf("restart_backoff.multiplier must be >= 0")
		}
		if cfg.GetJitter() < 0 || cfg.GetJitter() > 1 {
			return fmt.Errorf("restart_backoff.jitter must be between 0 and 1")
		}
	}
	var sawProbe bool
	for _, c := range req.GetContainers() {
		if err := ValidateProbe(c.GetLivenessProbe()); err != nil {
			return err
		}
		if c.GetLivenessProbe() != nil && c.GetLivenessProbe().GetProbeHandler() != nil {
			sawProbe = true
		}
	}
	if policy == Never && sawProbe {
		return fmt.Errorf("liveness_probe requires restart policy OnFailure or Always")
	}
	return nil
}

// Apply writes the canonical policy back onto the request and its label.
func Apply(req *cubebox.RunCubeSandboxRequest) error {
	if err := Validate(req); err != nil {
		return err
	}
	if req == nil {
		return nil
	}
	if strings.TrimSpace(req.GetRestartPolicy()) == "" {
		return nil
	}
	policy, _ := Normalize(req.GetRestartPolicy())
	req.RestartPolicy = policy
	if !KeepsRequest(policy) {
		return nil
	}
	if req.Labels == nil {
		req.Labels = map[string]string{}
	}
	req.Labels[Label] = policy
	return nil
}

// ValidateProbe accepts a nil probe. A set probe must name exactly one handler.
func ValidateProbe(probe *cubebox.LivenessProbe) error {
	if probe == nil || probe.GetProbeHandler() == nil {
		return nil
	}
	h := probe.GetProbeHandler()
	n := 0
	if tcp := h.GetTcpSocket(); tcp != nil {
		n++
		if tcp.GetPort() <= 0 || tcp.GetPort() > 65535 {
			return fmt.Errorf("liveness tcp port must be 1-65535")
		}
	}
	if httpGet := h.GetHttpGet(); httpGet != nil {
		n++
		if httpGet.GetPort() <= 0 || httpGet.GetPort() > 65535 {
			return fmt.Errorf("liveness http port must be 1-65535")
		}
		path := httpGet.GetPath()
		if path != "" && !strings.HasPrefix(path, "/") {
			return fmt.Errorf("liveness http path must be empty or start with /")
		}
		if strings.Contains(path, "@") || strings.Contains(path, "://") {
			return fmt.Errorf("liveness http path must not contain a host")
		}
	}
	if h.GetPing() != nil {
		n++
	}
	if n != 1 {
		return fmt.Errorf("liveness_probe must set exactly one of tcp_socket, http_get, ping")
	}
	return nil
}
