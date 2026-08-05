// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"fmt"
	"net"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
)

// cloneCubeNetworkConfig deep-copies the request policy before it is stored in
// managedState. Callers may reuse or mutate their request object after
// EnsureNetwork returns, so the runtime must own an immutable copy.
func cloneCubeNetworkConfig(in *CubeNetworkConfig) *CubeNetworkConfig {
	if in == nil {
		return nil
	}
	out := &CubeNetworkConfig{
		AllowOut: append([]string(nil), in.AllowOut...),
		DenyOut:  append([]string(nil), in.DenyOut...),
		Rules:    cloneEgressRules(in.Rules),
	}
	if in.AllowInternetAccess != nil {
		v := *in.AllowInternetAccess
		out.AllowInternetAccess = &v
	}
	return out
}

// cloneEgressRules deep-copies the nested L7 rule tree and drops nil rule or
// injection entries so downstream renderers can iterate safely.
func cloneEgressRules(in []*EgressRule) []*EgressRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]*EgressRule, 0, len(in))
	for _, r := range in {
		if r == nil {
			continue
		}
		cp := &EgressRule{Name: r.Name}
		if r.Match != nil {
			match := *r.Match
			match.Method = append([]string(nil), r.Match.Method...)
			cp.Match = &match
		}
		if r.Action != nil {
			action := &EgressRuleAction{Allow: r.Action.Allow}
			if r.Action.Audit != nil {
				audit := *r.Action.Audit
				action.Audit = &audit
			}
			if len(r.Action.Inject) > 0 {
				action.Inject = make([]*EgressRuleInject, 0, len(r.Action.Inject))
				for _, inj := range r.Action.Inject {
					if inj == nil {
						continue
					}
					injCopy := *inj
					if inj.Format != nil {
						format := *inj.Format
						injCopy.Format = &format
					}
					action.Inject = append(action.Inject, &injCopy)
				}
			}
			cp.Action = action
		}
		out = append(out, cp)
	}
	return out
}

// formatCubeNetworkConfig renders a compact log-only view of the policy. It
// deliberately avoids dumping full L7 rule bodies, which may contain secrets in
// header-injection rules.
func formatCubeNetworkConfig(in *CubeNetworkConfig) string {
	if in == nil {
		return "allow_internet_access=default(true) allow_out=[] deny_out=[] rules=0"
	}
	allowInternetAccess := "default(true)"
	if in.AllowInternetAccess != nil {
		allowInternetAccess = fmt.Sprintf("%t", *in.AllowInternetAccess)
	}
	return fmt.Sprintf("allow_internet_access=%s allow_out=%v deny_out=%v rules=%d", allowInternetAccess, in.AllowOut, in.DenyOut, len(in.Rules))
}

// cubeVSTapRegistration translates a CubeNetworkConfig into the cubevs MVM
// options consumed by the eBPF datapath. cubevs enforces L3/L4
// allow_internet_access / allow_out / deny_out, and it also receives network
// targets extracted from L7 rules as L7 allow targets. The complete L7 rules
// are still pushed to CubeEgress separately.
func cubeVSTapRegistration(cfg *CubeNetworkConfig) cubevs.MVMOptions {
	if cfg == nil {
		allowInternetAccess := true
		return cubevs.MVMOptions{AllowInternetAccess: &allowInternetAccess}
	}
	opts := cubevs.MVMOptions{}
	if cfg.AllowInternetAccess != nil {
		v := *cfg.AllowInternetAccess
		opts.AllowInternetAccess = &v
	} else {
		allowInternetAccess := true
		opts.AllowInternetAccess = &allowInternetAccess
	}
	if len(cfg.AllowOut) > 0 {
		allowOut := append([]string(nil), cfg.AllowOut...)
		opts.AllowOut = &allowOut
	}
	if l7AllowOut := extractL7AllowOutTargetsFromRules(cfg.Rules); len(l7AllowOut) > 0 {
		opts.L7AllowOut = &l7AllowOut
	}
	if len(cfg.DenyOut) > 0 {
		denyOut := append([]string(nil), cfg.DenyOut...)
		opts.DenyOut = &denyOut
	}
	return opts
}

// extractL7AllowOutTargetsFromRules converts SNI/Host matches into the coarse
// network targets that cubevs needs to allow before CubeEgress can inspect L7
// traffic. Invalid or non-IPv4-looking targets are ignored rather than failing
// sandbox creation; the full rule is still validated by CubeEgress on push.
func extractL7AllowOutTargetsFromRules(rules []*EgressRule) []string {
	seen := make(map[string]struct{})
	targets := make([]string, 0, len(rules))
	add := func(target string, ok bool) {
		if !ok {
			return
		}
		if _, exists := seen[target]; exists {
			return
		}
		seen[target] = struct{}{}
		targets = append(targets, target)
	}

	for _, rule := range rules {
		if rule == nil || rule.Match == nil {
			continue
		}
		if rule.Match.SNI != nil {
			add(normalizeL7DomainTarget(*rule.Match.SNI))
		}
		if rule.Match.Host != nil {
			add(normalizeL7HostTarget(*rule.Match.Host))
		}
	}
	return targets
}

// normalizeL7DomainTarget canonicalizes a DNS name or wildcard suffix for the
// eBPF-side allow list. Dotted-decimal strings are rejected here so malformed
// IPv4 values do not get treated as domains.
func normalizeL7DomainTarget(raw string) (string, bool) {
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if isDottedDecimalLikeL7Target(domain) || !isValidL7DomainName(domain) {
		return "", false
	}
	return domain, true
}

// normalizeL7HostTarget accepts the Host match forms that can be represented in
// cubevs: IPv4, IPv4 CIDR, domain name, or wildcard domain. IPv6 is ignored
// because the current sandbox datapath is IPv4-only.
func normalizeL7HostTarget(raw string) (string, bool) {
	target := strings.TrimSpace(raw)
	if target == "" {
		return "", false
	}
	if host, _, err := net.SplitHostPort(target); err == nil {
		target = host
	}

	if ip := net.ParseIP(target); ip != nil {
		if ip.To4() == nil {
			return "", false
		}
		return ip.To4().String(), true
	}
	if strings.Contains(target, "/") {
		ip, ipNet, err := net.ParseCIDR(target)
		if err != nil || ip.To4() == nil {
			return "", false
		}
		return ipNet.String(), true
	}
	if isDottedDecimalLikeL7Target(target) {
		return "", false
	}
	return normalizeL7DomainTarget(target)
}

// isDottedDecimalLikeL7Target catches strings such as "999.1.2.3" so they are
// not accidentally accepted as domain names after net.ParseIP rejects them.
func isDottedDecimalLikeL7Target(target string) bool {
	parts := strings.Split(strings.TrimSuffix(target, "."), ".")
	if len(parts) != net.IPv4len {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	return true
}

// isValidL7DomainName validates the conservative subset supported by the
// datapath allow list: ordinary labels plus a single leading wildcard label.
func isValidL7DomainName(domain string) bool {
	if domain == "" || len(domain) >= 254 {
		return false
	}
	if strings.Contains(domain, "*") {
		if strings.Count(domain, "*") != 1 || !strings.HasPrefix(domain, "*.") || len(domain) <= len("*.") {
			return false
		}
		domain = domain[2:]
	}
	labels := strings.Split(domain, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, ch := range label {
			isAlphaNum := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')
			if !isAlphaNum && ch != '-' {
				return false
			}
			if ch == '-' && (i == 0 || i == len(label)-1) {
				return false
			}
		}
	}
	return true
}

// nonEmpty returns value unless it is empty, in which case fallback is used.
func nonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// cloneStringMap makes response metadata independent from the stored map.
func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
