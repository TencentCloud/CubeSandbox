// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"context"
	"errors"
	"time"

	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
	"github.com/tencentcloud/CubeSandbox/network-agent/internal/cubeegress"
)

// egressClient is the subset of cubeegress.Client we use. Defining it
// as an interface in the service package lets the maintenance loop's
// retry path swap in a fake during unit tests without spinning up an
// httptest server, and lets NewLocalService accept either a real client
// (in production) or nil (when the admin URL isn't set).
type egressClient interface {
	Configured() bool
	PutPolicy(ctx context.Context, sandboxIP string, in *cubeegress.PolicyInput) error
	DeletePolicy(ctx context.Context, sandboxIP string) error
}

// newEgressClientFromConfig builds the production client from Config.
// Returns nil when CubeEgressAdminURL is empty; the call sites tolerate
// nil and skip the push silently.
func newEgressClientFromConfig(cfg Config) egressClient {
	if cfg.CubeEgressAdminURL == "" {
		return nil
	}
	timeout := cfg.CubeEgressPushTimeout
	if timeout <= 0 {
		timeout = cubeegress.DefaultPushTimeout
	}
	return cubeegress.New(cfg.CubeEgressAdminURL, timeout)
}

// toEgressInput maps the upstream service.CubeNetworkConfig into the
// cubeegress package's wire-input shape. Returns nil when there are no
// L7 rules to push — this is the canonical "skip the push" signal that
// both the per-sandbox push and the bulk dump endpoint share, so they
// can never disagree about whether a sandbox has an L7 policy.
//
// Why a separate boundary type instead of importing service.CubeNetworkConfig
// from cubeegress: that would make cubeegress depend on service, which
// would close the import cycle (service → cubeegress → service). Doing
// the mapping here is the cheap fix; the mapping is mechanical and
// covered by toEgressInputTranslates_test below.
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

func toMatchInput(m *EgressRuleMatch) *cubeegress.MatchInput {
	if m == nil {
		return nil
	}
	return &cubeegress.MatchInput{
		SNI:    m.SNI,
		Host:   m.Host,
		Method: append([]string(nil), m.Method...),
		Path:   m.Path,
		Scheme: m.Scheme,
	}
}

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

// pushEgressForState is the EnsureNetwork side dispatcher. It runs
// best-effort: a permanent error (4xx) is logged and dropped — replays
// won't fix it. A transient error sets state.pendingEgressPush so the
// maintenance loop retries later (see retryPendingEgressPushes).
//
// Returns whether the push attempt was actually made (so the caller
// can record audit-style metrics if it ever wants to).
func (s *localService) pushEgressForState(ctx context.Context, state *managedState) {
	if s.egress == nil || !s.egress.Configured() {
		return
	}
	in := toEgressInput(state.CubeNetworkConfig)
	if in == nil {
		// No L7 rules; nothing for CubeEgress to do. Make sure any
		// stale pending flag from a prior reconcile is cleared.
		state.pendingEgressPush = false
		return
	}
	applyEgressPushResult(ctx, state, s.egress.PutPolicy(ctx, state.SandboxIP, in))
}

// applyEgressPushResult records the outcome of a CubeEgress PutPolicy call on
// state.pendingEgressPush: cleared on success or a permanent (4xx) failure —
// where a replay can't help and the operator must fix the rule — and set on a
// transient failure so retryPendingEgressPushes tries again later. The caller
// must have exclusive access to state (hold s.mu, or own a state not yet
// published to s.states).
func applyEgressPushResult(ctx context.Context, state *managedState, err error) {
	switch {
	case err == nil, errors.Is(err, cubeegress.ErrNotConfigured):
		state.pendingEgressPush = false
	case cubeegress.IsPermanent(err):
		CubeLog.WithContext(ctx).Errorf("network-agent push egress policy permanently failed: sandbox_id=%s sandbox_ip=%s err=%v",
			state.SandboxID, state.SandboxIP, err)
		state.pendingEgressPush = false
	default:
		CubeLog.WithContext(ctx).Warnf("network-agent push egress policy transiently failed; will retry: sandbox_id=%s sandbox_ip=%s err=%v",
			state.SandboxID, state.SandboxIP, err)
		state.pendingEgressPush = true
	}
}

// deleteEgressForState fires DELETE /admin/v1/policies/<ip> at
// ReleaseNetwork time. Strictly best-effort: if it fails, the sandbox
// IP is gone and CubeEgress's stale entry is harmless (no traffic will
// arrive on it; if the IP gets re-allocated, the new sandbox's PUT
// replaces it). Errors log at WARN, never propagate.
func (s *localService) deleteEgressForState(ctx context.Context, sandboxID, sandboxIP string) {
	if s.egress == nil || !s.egress.Configured() {
		return
	}
	if err := s.egress.DeletePolicy(ctx, sandboxIP); err != nil && !errors.Is(err, cubeegress.ErrNotConfigured) {
		CubeLog.WithContext(ctx).Warnf("network-agent delete egress policy failed (best-effort): sandbox_id=%s sandbox_ip=%s err=%v",
			sandboxID, sandboxIP, err)
	}
}

// pushEgressSerialized renders the sandbox's CURRENT egress policy and pushes it
// to CubeEgress, serialized per sandbox via state.egressPushMu. This prevents a
// live UpdateEgressRule and a maintenance retry (or two concurrent updates) from
// interleaving and leaving a stale policy live: whichever push acquires
// egressPushMu last re-renders under s.mu and therefore reflects the latest
// rules. It returns whether the push is still pending (a transient failure the
// maintenance loop will retry) and the push error (nil on success or when no
// push was needed) so callers can log the outcome.
//
// Must be called WITHOUT s.mu held; egressPushMu is always taken before s.mu.
func (s *localService) pushEgressSerialized(ctx context.Context, state *managedState) (pending bool, err error) {
	state.egressPushMu.Lock()
	defer state.egressPushMu.Unlock()

	s.mu.Lock()
	pushable := s.egress != nil && s.egress.Configured()
	input := toEgressInput(state.CubeNetworkConfig)
	s.mu.Unlock()

	if !pushable {
		return false, nil
	}
	if input == nil {
		// No L7 rules to push; clear any stale pending flag.
		s.mu.Lock()
		state.pendingEgressPush = false
		s.mu.Unlock()
		return false, nil
	}

	err = s.egress.PutPolicy(ctx, state.SandboxIP, input)

	s.mu.Lock()
	applyEgressPushResult(ctx, state, err)
	pending = state.pendingEgressPush
	s.mu.Unlock()
	return pending, err
}

// retryPendingEgressPushes is invoked from the maintenance loop. It re-pushes
// every managed state with pendingEgressPush=true via pushEgressSerialized, which
// serializes per sandbox against a live UpdateEgressRule so a retry can never
// overwrite a freshly rotated credential with a stale policy.
func (s *localService) retryPendingEgressPushes() {
	if s.egress == nil || !s.egress.Configured() {
		return
	}
	s.mu.Lock()
	var todo []*managedState
	for _, st := range s.states {
		if st.pendingEgressPush {
			todo = append(todo, st)
		}
	}
	s.mu.Unlock()

	for _, st := range todo {
		// Skip if a concurrent UpdateEgressRule already resolved this sandbox;
		// pushEgressSerialized would otherwise redundantly re-push the same policy.
		s.mu.Lock()
		stillPending := st.pendingEgressPush
		s.mu.Unlock()
		if !stillPending {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), egressRetryCallTimeout)
		pending, err := s.pushEgressSerialized(ctx, st)
		cancel()
		if err == nil && !pending {
			CubeLog.WithContext(context.Background()).Infof(
				"network-agent retry egress policy succeeded: sandbox_ip=%s", st.SandboxIP)
		}
	}
}

// egressRetryCallTimeout bounds each retry attempt independently of the
// per-call timeout configured for steady-state pushes. Kept short so a
// stuck CubeEgress doesn't slow down the maintenance loop's other work
// (tap recovery, etc.).
const egressRetryCallTimeout = 2 * time.Second

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
// rules to whatever a live network-agent would have just pushed via
// PUT /admin/v1/policies/<ip>. That equivalence is what makes the
// race "EnsureNetwork during CubeEgress bootstrap" benign (see
// design/cube-egress-rule-delivery.md "Failure handling" table).
//
// Sandboxes whose CubeNetworkConfig has no L7 rules are omitted; the
// caller (CubeEgress) only cares about sandboxes that actually have
// an L7 policy to install.
func (s *localService) DumpEgressPolicies(_ context.Context) (map[string]map[string]any, error) {
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
