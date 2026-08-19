// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package cubeegress translates the embedded network runtime's egress
// configuration into the on-the-wire shape that CubeEgress's admin API expects, and pushes
// it via PUT /admin/v1/policies/<sandbox_ip>.
//
// Why a dedicated package instead of inlining: there are two callers of
// the renderer — the per-sandbox push at EnsureNetwork time and the bulk
// dump endpoint that drives CubeEgress bootstrap on restart — and they
// MUST agree byte-for-byte on the output, otherwise a sandbox's effective
// rules can drift between "freshly created" and "after CubeEgress
// restarts". Co-locating the renderer behind one function is the cheap
// way to enforce that invariant.
//
// Naming reconciliation (see design/cube-egress-rule-delivery.md §D5):
// the embedded network runtime uses Go-idiomatic camelCase JSON keys
// (matches the existing `allowOut`/`denyOut` precedent), while
// CubeEgress's lua/policy.lua and lua/access_phase.lua expect
// snake_case. The renderer is the single point that bridges the two
// dialects.
//
// Why the renderer takes its own input types instead of importing
// runtime.CubeNetworkConfig directly: avoiding an import cycle. The runtime
// package owns the controller model and constructs the cubeegress client;
// cubeegress must therefore not import runtime. The boundary mapping
// (runtime.CubeNetworkConfig → cubeegress.PolicyInput) lives in the
// CubeEgressAdapter and is tested there.

package cubeegress

import (
	"encoding/json"
	"fmt"
)

// PolicyInput is the embedded network runtime's wire-level view of a sandbox's
// egress rules. It mirrors runtime.CubeNetworkConfig at this adapter boundary
// while keeping the transport package independent from the controller model.
type PolicyInput struct {
	Rules []RuleInput
}

// RuleInput mirrors runtime.EgressRule. Pointer fields stay pointers
// so "absent" is distinct from "empty string" — important for match
// fields, where missing means "any" and empty string means "must be
// empty".
type RuleInput struct {
	Name   string
	Match  *MatchInput
	Action *ActionInput
}

// MatchInput mirrors runtime.EgressRuleMatch.
type MatchInput struct {
	SNI    *string
	Host   *string
	Method []string
	Path   *string
	Scheme *string
	Port   *int
}

// ActionInput mirrors runtime.EgressRuleAction.
type ActionInput struct {
	Allow  bool
	Audit  *string
	Inject []InjectInput
}

// InjectInput mirrors runtime.EgressRuleInject.
type InjectInput struct {
	Header string
	Secret string
	Format *string
}

// RenderEgressPolicy converts a PolicyInput into the JSON body
// CubeEgress's PUT /admin/v1/policies/<sandbox_ip> expects.
//
// Returns nil, nil when there is nothing to push (input is nil, or has
// no rules). Callers should use that as the signal to skip the HTTP
// call — CubeEgress treats an empty-rules policy as a validation error
// (lua/policy.lua: "policy.rules must have at least one rule").
//
// policy_id is set to sandboxID so CubeEgress's audit log carries a
// stable handle that traces back to the sandbox without an extra
// lookup. CubeEgress treats policy_id as opaque (lua/policy.lua only
// requires non-empty string).
//
// Field translation:
//
//	rule.Name             → policy.rules[].id
//	(match.* and action.* / inject.* pass through verbatim;
//	the names already match between the two dialects, modulo the
//	camelCase ↔ snake_case rendering this function performs.)
//
// Note: CubeEgress's match grammar today (lua/access_phase.lua) honors
// match fields beyond what Cubelet emits — notably sni_suffix
// and path_prefix — but the upstream chain (SDK → CubeMaster →
// Cubelet embedded network runtime) deliberately does NOT carry those, so the
// renderer doesn't either. CubeEgress operators who want suffix or
// prefix matching today must PUT the policy directly through the
// admin API; they cannot reach it via the sandbox-create path.
func RenderEgressPolicy(sandboxID string, in *PolicyInput) ([]byte, error) {
	body := buildPolicyBody(sandboxID, in)
	if body == nil {
		return nil, nil
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode egress policy: %w", err)
	}
	return out, nil
}

// BuildPolicyBody is the structured form of RenderEgressPolicy. The
// dump endpoint composes a map of these without re-marshaling each one,
// so we expose the intermediate value too. Returns nil when nothing
// would be pushed (matches RenderEgressPolicy semantics).
func BuildPolicyBody(sandboxID string, in *PolicyInput) map[string]any {
	return buildPolicyBody(sandboxID, in)
}

// buildPolicyBody constructs the shared structured policy body for both the PUT
// path and the bootstrap dump path.
func buildPolicyBody(sandboxID string, in *PolicyInput) map[string]any {
	if in == nil || len(in.Rules) == 0 {
		return nil
	}
	rules := make([]map[string]any, 0, len(in.Rules))
	for i := range in.Rules {
		rules = append(rules, renderRule(&in.Rules[i]))
	}
	if len(rules) == 0 {
		return nil
	}
	return map[string]any{
		"policy_id": sandboxID,
		"rules":     rules,
	}
}

// renderRule renders one rule in CubeEgress's snake_case wire shape.
func renderRule(r *RuleInput) map[string]any {
	rule := map[string]any{
		"id":     r.Name,
		"match":  renderMatch(r.Match),
		"action": renderAction(r.Action),
	}
	return rule
}

// renderMatch renders optional match fields, preserving absent-vs-empty-string
// semantics through pointer checks.
func renderMatch(m *MatchInput) map[string]any {
	// CubeEgress requires match to be an object (empty {} allowed); if
	// the upstream side sent nil, emit {} so validation passes.
	if m == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if m.SNI != nil {
		out["sni"] = *m.SNI
	}
	if m.Host != nil {
		out["host"] = *m.Host
	}
	if len(m.Method) > 0 {
		out["method"] = append([]string(nil), m.Method...)
	}
	if m.Path != nil {
		out["path"] = *m.Path
	}
	if m.Scheme != nil {
		out["scheme"] = *m.Scheme
	}
	if m.Port != nil {
		// CubeEgress's access_phase.lua rule_matches compares match.port
		// against ctx.dst_port (from tproxy's $server_port). Emit as a
		// number so cjson decodes it to a numeric type in openresty.
		out["port"] = *m.Port
	}
	return out
}

// renderAction renders the action object accepted by CubeEgress validation.
func renderAction(a *ActionInput) map[string]any {
	// Action.allow is required by CubeEgress validation; nil action would
	// fail at PUT time. Treat a nil action as deny (allow=false) so the
	// caller doesn't accidentally produce a permissive rule via omission.
	if a == nil {
		return map[string]any{"allow": false}
	}
	out := map[string]any{
		"allow": a.Allow,
	}
	if a.Audit != nil {
		out["audit"] = *a.Audit
	}
	if len(a.Inject) > 0 {
		injects := make([]map[string]any, 0, len(a.Inject))
		for i := range a.Inject {
			injects = append(injects, renderInject(&a.Inject[i]))
		}
		if len(injects) > 0 {
			out["inject"] = injects
		}
	}
	return out
}

// renderInject renders one header injection descriptor.
func renderInject(inj *InjectInput) map[string]any {
	out := map[string]any{
		"header": inj.Header,
		"secret": inj.Secret,
	}
	if inj.Format != nil {
		out["format"] = *inj.Format
	}
	return out
}
