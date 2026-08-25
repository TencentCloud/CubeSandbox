// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/network/runtime/cubeegress"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

// CubeEgressAdapter is the runtime boundary for L7 egress policy delivery.
// Defining it as an interface lets create/delete/maintenance paths inject
// deterministic failures in unit tests without spinning up an httptest server.
type CubeEgressAdapter interface {
	Configured() bool
	PutPolicy(ctx context.Context, sandboxIP string, in *cubeegress.PolicyInput) error
	DeletePolicy(ctx context.Context, sandboxIP string) error
	VerifyPolicyAbsent(ctx context.Context, sandboxIP string) error
}

// newCubeEgressAdapterFromConfig builds the production adapter from Config.
// Returns nil when CubeEgressAdminURL is empty; the call sites tolerate
// nil and skip the push silently.
func newCubeEgressAdapterFromConfig(cfg Config) CubeEgressAdapter {
	if cfg.CubeEgressAdminURL == "" {
		return nil
	}
	timeout := cfg.CubeEgressPushTimeout
	if timeout <= 0 {
		timeout = cubeegress.DefaultPushTimeout
	}
	return cubeegress.New(cfg.CubeEgressAdminURL, timeout)
}

// toEgressInput maps the runtime CubeNetworkConfig into the cubeegress
// package's wire-input shape. Returns nil when there are no L7 rules to push —
// this is the canonical "skip the push" signal that both the per-sandbox push
// and the bulk dump endpoint share, so they can never disagree about whether a
// sandbox has an L7 policy.
//
// CubeEgress wire types intentionally live below this adapter boundary so the
// transport client never imports the runtime controller model. The mapping is
// mechanical and covered by toEgressInputTranslates_test below.
func toEgressInput(cfg *CubeNetworkConfig) *cubeegress.PolicyInput {
	if cfg == nil || len(cfg.Rules) == 0 {
		return nil
	}
	rules := make([]cubeegress.RuleInput, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		if r == nil {
			continue
		}
		rules = append(rules, cubeegress.RuleInput{
			Name:   r.Name,
			Match:  toMatchInput(r.Match),
			Action: toActionInput(r.Action),
		})
	}
	if len(rules) == 0 {
		return nil
	}
	return &cubeegress.PolicyInput{Rules: rules}
}

// toMatchInput deep-copies the match section into the cubeegress boundary type.
func toMatchInput(m *EgressRuleMatch) *cubeegress.MatchInput {
	if m == nil {
		return nil
	}
	out := &cubeegress.MatchInput{
		SNI:    m.SNI,
		Host:   m.Host,
		Method: append([]string(nil), m.Method...),
		Path:   m.Path,
		Scheme: m.Scheme,
	}
	if m.Port != nil {
		p := *m.Port
		out.Port = &p
	}
	return out
}

// toActionInput deep-copies the action section, including header-injection
// entries, into the cubeegress boundary type.
func toActionInput(a *EgressRuleAction) *cubeegress.ActionInput {
	if a == nil {
		return nil
	}
	out := &cubeegress.ActionInput{
		Allow: a.Allow,
		Audit: a.Audit,
	}
	if len(a.Inject) > 0 {
		out.Inject = make([]cubeegress.InjectInput, 0, len(a.Inject))
		for _, inj := range a.Inject {
			if inj == nil {
				continue
			}
			out.Inject = append(out.Inject, cubeegress.InjectInput{
				Header: inj.Header,
				Secret: inj.Secret,
				Format: inj.Format,
			})
		}
	}
	return out
}

// pushEgressForState is the EnsureNetwork side dispatcher. When there are L7
// rules, transient push failures are retried synchronously before creation is
// aborted; permanent 4xx errors are returned immediately.
func (s *NetworkController) pushEgressForState(ctx context.Context, state *managedState) error {
	if s.cubeEgressAdapter == nil || !s.cubeEgressAdapter.Configured() {
		return nil
	}
	in := toEgressInput(state.CubeNetworkConfig)
	if in == nil {
		return nil
	}
	return s.putEgressPolicyForCreate(ctx, state, in)
}

// syncEgressPolicy converges CubeEgress on cfg's rule set for a live sandbox.
//
// Whether CubeEgress is involved at all is decided by the rule sets alone:
//
//	want != nil                 -> install the new rules
//	want == nil, installed != nil -> clear the rules the sandbox still has
//	want == nil, installed == nil -> L7 is not in play; do not touch CubeEgress
//
// The third row is why this exists. pushEgressForState can stop at "no rules, no
// call" because on create nothing is installed yet, but an update must also be
// able to clear. Deriving that from the rule sets keeps an L3-only sandbox — one
// that never had L7 rules and still doesn't — independent of CubeEgress instead
// of failing its policy updates whenever the proxy is unreachable.
//
// `installed` comes from the state's own config, which can under-report if an
// earlier update pushed rules and then failed before persisting. Those leftovers
// are inert (the datapath was never told to steer at them) and CubeEgress
// re-seeds from DumpEgressPolicies on its next reload.
func (s *NetworkController) syncEgressPolicy(ctx context.Context, state *managedState, cfg *CubeNetworkConfig) error {
	want := toEgressInput(cfg)
	installed := toEgressInput(state.CubeNetworkConfig)
	if want == nil && installed == nil {
		return nil
	}
	// L7 is in play, so CubeEgress has to be part of this deployment. An unset
	// admin URL means it is not (dev mode), which the create and release paths
	// also treat as a silent no-op.
	if s.cubeEgressAdapter == nil || !s.cubeEgressAdapter.Configured() {
		return nil
	}
	if want == nil {
		return s.deleteEgressForState(ctx, state.SandboxID, state.SandboxIP)
	}
	return s.putEgressPolicyForCreate(ctx, state, want)
}

// putEgressPolicyForCreate keeps sandbox creation from failing on a single
// transient CubeEgress admin blip. Three total attempts are deliberately small:
// with the default 2s per-call timeout the worst case is bounded around six
// seconds, while the two short gaps cover common OpenResty reload/startup races.
func (s *NetworkController) putEgressPolicyForCreate(ctx context.Context, state *managedState, in *cubeegress.PolicyInput) error {
	attempts := len(egressCreatePushRetryDelays) + 1
	for attempt := 1; attempt <= attempts; attempt++ {
		err := s.cubeEgressAdapter.PutPolicy(ctx, state.SandboxIP, in)
		if err == nil || cubeegress.IsPermanent(err) || attempt == attempts {
			return err
		}
		delay := egressCreatePushRetryDelays[attempt-1]
		CubeLog.WithContext(ctx).Warnf(
			"network runtime push egress policy failed, retrying: sandbox_id=%s sandbox_ip=%s attempt=%d/%d delay=%s err=%v",
			state.SandboxID, state.SandboxIP, attempt, attempts, delay, err,
		)
		if err := waitBeforeEgressCreateRetry(ctx, delay); err != nil {
			return err
		}
	}
	return nil
}

func waitBeforeEgressCreateRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var egressCreatePushRetryDelays = []time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
}

// deleteEgressForState fires DELETE /admin/v1/policies/<ip> at release/cleanup
// time. A failed delete leaves the TAP/state in Cleaning so the maintenance loop
// retries before the sandbox IP can be reused by a different sandbox.
func (s *NetworkController) deleteEgressForState(ctx context.Context, sandboxID, sandboxIP string) error {
	if s.cubeEgressAdapter == nil || !s.cubeEgressAdapter.Configured() {
		return nil
	}
	if err := s.cubeEgressAdapter.DeletePolicy(ctx, sandboxIP); err != nil && !errors.Is(err, cubeegress.ErrNotConfigured) {
		return fmt.Errorf("delete CubeEgress policy for sandbox %s ip %s: %w", sandboxID, sandboxIP, err)
	}
	if err := s.cubeEgressAdapter.VerifyPolicyAbsent(ctx, sandboxIP); err != nil && !errors.Is(err, cubeegress.ErrNotConfigured) {
		return fmt.Errorf("verify CubeEgress policy absent for sandbox %s ip %s: %w", sandboxID, sandboxIP, err)
	}
	return nil
}

// DumpEgressPolicies serves the GET /v1/policies/dump endpoint that
// CubeEgress's bootstrap.lua reads on worker init. The wire shape
// matches what bootstrap.lua's `_apply` already accepts:
//
//	{ "policies": { "<sandbox_ip>": { "policy_id": ..., "rules": [...] } } }
//
// (the outer `policies` wrapper is added by the HTTP layer; this
// method returns the inner map only).
//
// The body for each sandbox is built through the same renderer used
// by per-sandbox push — see cubeegress.BuildPolicyBody — so a fresh
// CubeEgress that bootstraps off this endpoint sees byte-identical
// rules to whatever a live network runtime would have just pushed via
// PUT /admin/v1/policies/<ip>. That equivalence is what makes the
// race "EnsureNetwork during CubeEgress bootstrap" benign (see
// design/cube-egress-rule-delivery.md "Failure handling" table).
//
// Sandboxes whose CubeNetworkConfig has no L7 rules are omitted; the
// caller (CubeEgress) only cares about sandboxes that actually have
// an L7 policy to install.
func (s *NetworkController) DumpEgressPolicies(_ context.Context) (map[string]map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]map[string]any, len(s.states))
	for _, st := range s.states {
		input := toEgressInput(st.CubeNetworkConfig)
		body := cubeegress.BuildPolicyBody(st.SandboxIP, input)
		if body == nil {
			continue
		}
		out[st.SandboxIP] = body
	}
	return out, nil
}
