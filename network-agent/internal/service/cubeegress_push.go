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
	logger := CubeLog.WithContext(ctx)
	in := toEgressInput(state.CubeNetworkConfig)
	if in == nil {
		// No L7 rules; nothing for CubeEgress to do. Make sure any
		// stale pending flag from a prior reconcile is cleared.
		state.pendingEgressPush = false
		return
	}
	err := s.egress.PutPolicy(ctx, state.SandboxIP, in)
	switch {
	case err == nil:
		state.pendingEgressPush = false
	case errors.Is(err, cubeegress.ErrNotConfigured):
		// Race: client unconfigured by the time we got here. Treat as
		// no-op; nothing to retry.
		state.pendingEgressPush = false
	case cubeegress.IsPermanent(err):
		logger.Errorf("network-agent push egress policy permanently failed: sandbox_id=%s sandbox_ip=%s err=%v",
			state.SandboxID, state.SandboxIP, err)
		// Don't keep retrying a malformed body — operator must fix the
		// upstream rule. Clear the pending flag so we don't loop.
		state.pendingEgressPush = false
	default:
		logger.Warnf("network-agent push egress policy transiently failed; will retry: sandbox_id=%s sandbox_ip=%s err=%v",
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

// retryPendingEgressPushes is invoked from the maintenance loop. It
// iterates current managed states and re-pushes any that have
// pendingEgressPush=true. Holds s.mu for the duration of each per-state
// push so the state map can't change underneath us; the actual HTTP
// call happens with the lock released to avoid blocking the data plane.
func (s *localService) retryPendingEgressPushes() {
	if s.egress == nil || !s.egress.Configured() {
		return
	}
	// Snapshot states needing retry so we don't hold the lock during HTTP I/O.
	type pending struct {
		state *managedState
		ip    string
		input *cubeegress.PolicyInput
	}
	var todo []pending
	s.mu.Lock()
	for _, st := range s.states {
		if !st.pendingEgressPush {
			continue
		}
		input := toEgressInput(st.CubeNetworkConfig)
		if input == nil {
			// Pending got set but rules are gone (mutation we don't yet
			// support). Clear the flag.
			st.pendingEgressPush = false
			continue
		}
		todo = append(todo, pending{state: st, ip: st.SandboxIP, input: input})
	}
	s.mu.Unlock()

	if len(todo) == 0 {
		return
	}

	logger := CubeLog.WithContext(context.Background())
	for _, item := range todo {
		ctx, cancel := context.WithTimeout(context.Background(), egressRetryCallTimeout)
		err := s.egress.PutPolicy(ctx, item.ip, item.input)
		cancel()
		s.mu.Lock()
		switch {
		case err == nil:
			item.state.pendingEgressPush = false
			logger.Infof("network-agent retry egress policy succeeded: sandbox_ip=%s", item.ip)
		case cubeegress.IsPermanent(err):
			item.state.pendingEgressPush = false
			logger.Errorf("network-agent retry egress policy permanently failed: sandbox_ip=%s err=%v",
				item.ip, err)
		default:
			// Leave pending=true; next maintenance tick tries again.
			logger.Debugf("network-agent retry egress policy still transient: sandbox_ip=%s err=%v",
				item.ip, err)
		}
		s.mu.Unlock()
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
