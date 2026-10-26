// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"errors"
	"fmt"
	"net"
	"slices"
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
			// The pointer fields must be deep-copied too: a shallow struct copy
			// would alias the caller's request, so a later mutation of the
			// request would leak into the stored (supposedly immutable) copy.
			match.SNI = cloneStringPtr(r.Match.SNI)
			match.Host = cloneStringPtr(r.Match.Host)
			match.Path = cloneStringPtr(r.Match.Path)
			match.Scheme = cloneStringPtr(r.Match.Scheme)
			match.Port = cloneIntPtr(r.Match.Port)
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

// cloneStringPtr returns a copy of a *string, or nil. Mirrors the CubeMaster
// helper of the same name (pkg/service/sandbox/types/types.go).
func cloneStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// cloneIntPtr returns a copy of a *int, or nil. Mirrors the CubeMaster helper
// of the same name (pkg/service/sandbox/types/types.go).
func cloneIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
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
// withDNSResolverAllowOut folds the sandbox's resolver CIDRs back into cfg.
//
// An update request carries only user-authored targets, but the create path
// appended the resolver /32s so domain rules could be resolved at all. Without
// this the first update of a domain-based policy would revoke DNS itself and
// black-hole every domain rule it just installed.
//
// Same condition as the create path: only a policy that still names a domain
// keeps the resolver exception, so an update that drops every domain also drops
// the implicit DNS access.
func withDNSResolverAllowOut(cfg *CubeNetworkConfig, resolverCIDRs []string) *CubeNetworkConfig {
	if cfg == nil || len(resolverCIDRs) == 0 || !needsDNSResolution(cfg) {
		return cfg
	}
	for _, cidr := range resolverCIDRs {
		if !slices.Contains(cfg.AllowOut, cidr) {
			cfg.AllowOut = append(cfg.AllowOut, cidr)
		}
	}
	return cfg
}

// needsDNSResolution reports whether any allow_out target or L7 rule host is a
// domain. It asks the predicate that mirrors where cubevs actually installs a
// target, so a bare IPv4 literal does not read as a domain -- a name-shape check
// accepts "10.0.0.1" because digits are valid DNS label characters, and folding
// the resolver in for an IP-only policy would grant access nobody asked for.
func needsDNSResolution(cfg *CubeNetworkConfig) bool {
	if slices.ContainsFunc(cfg.AllowOut, cubevs.IsAllowOutDomainTarget) {
		return true
	}
	targets, err := extractL7AllowOutTargetsFromRules(cfg.Rules)
	if err != nil {
		// Malformed rules are rejected later with a precise error. Assume DNS is
		// needed so a bad request cannot silently strip resolver access.
		return true
	}
	return slices.ContainsFunc(targets, func(t cubevs.L7Target) bool {
		return cubevs.IsAllowOutDomainTarget(t.Host)
	})
}

func cubeVSTapRegistration(cfg *CubeNetworkConfig) (cubevs.MVMOptions, error) {
	if cfg == nil {
		allowInternetAccess := true
		return cubevs.MVMOptions{AllowInternetAccess: &allowInternetAccess}, nil
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
	l7AllowOut, err := extractL7AllowOutTargetsFromRules(cfg.Rules)
	if err != nil {
		return cubevs.MVMOptions{}, err
	}
	if len(l7AllowOut) > 0 {
		opts.L7AllowOut = &l7AllowOut
	}
	if len(cfg.DenyOut) > 0 {
		denyOut := append([]string(nil), cfg.DenyOut...)
		opts.DenyOut = &denyOut
	}
	return opts, nil
}

// extractL7AllowOutTargetsFromRules walks the L7 rule list and produces the
// (host, port, scheme) tuples cubevs needs for its per-host dns_allow_v2 /
// allow_out_v3 port set. SNI-only rules and rules without a Match.Host are
// treated as legacy port-agnostic entries — they expand to {80/http, 443/https}
// downstream in buildL7Plan. Rules that specify a Port MUST also specify a
// scheme ("http" or "https"). Any invalid port/scheme pair rejects the whole
// projection so CubeVS and CubeEgress cannot observe different policies.
//
// Duplicate (host, port, scheme) tuples are deduplicated to keep the map
// value's port_count within maxL7PortsPerHost when a user attaches the same
// port to several rules.
func extractL7AllowOutTargetsFromRules(rules []*EgressRule) ([]cubevs.L7Target, error) {
	type key struct {
		host   string
		port   uint16
		scheme uint8
	}
	seen := make(map[key]struct{})
	targets := make([]cubevs.L7Target, 0, len(rules))
	add := func(host string, ok bool, port uint16, scheme uint8) {
		if !ok {
			return
		}
		k := key{host, port, scheme}
		if _, exists := seen[k]; exists {
			return
		}
		seen[k] = struct{}{}
		targets = append(targets, cubevs.L7Target{Host: host, Port: port, Scheme: scheme})
	}

	for i, rule := range rules {
		if rule == nil || rule.Match == nil {
			continue
		}
		port, scheme, err := extractL7PortScheme(rule.Match)
		if err != nil {
			return nil, fmt.Errorf("network.rules[%d] %q: %w", i, rule.Name, err)
		}
		// A rule carrying both SNI and Host projects BOTH as L7 targets (SNI
		// first, then Host), not Host alone.
		if rule.Match.SNI != nil {
			host, ok := normalizeL7DomainTarget(*rule.Match.SNI)
			add(host, ok, port, scheme)
		}
		if rule.Match.Host != nil {
			host, ok := normalizeL7HostTarget(*rule.Match.Host)
			add(host, ok, port, scheme)
		}
	}
	return targets, nil
}

// extractL7PortScheme reads Match.Port + Match.Scheme and normalises them into
// cubevs.L7Target's numeric representation.
//
//   - Port set, Scheme nil → invalid (port without scheme cannot decide the
//     nginx listener); reject.
//   - Port nil, Scheme set  → fill in the scheme's default port (http → 80,
//     https → 443). Callers that only want to say "https on this host" can
//     omit port and get the conventional default.
//   - Port set, Scheme set  → new port-scoped feature: exact tuple.
//   - Both nil              → legacy: buildL7Plan expands to
//     {80/http, 443/https}.
//
// Any recognised, non-empty Scheme string is normalised to lowercase and
// stripped of surrounding whitespace before comparison — the wire form is
// case-insensitive.
func extractL7PortScheme(match *EgressRuleMatch) (uint16, uint8, error) {
	if match.Port == nil && match.Scheme == nil {
		// Legacy: buildL7Plan will expand to {80/http, 443/https}.
		return 0, cubevs.L7SchemeNone, nil
	}

	// Scheme presence guides port validation. Decode it first (nil is fine).
	var schemeValue uint8 = cubevs.L7SchemeNone
	if match.Scheme != nil {
		switch strings.ToLower(strings.TrimSpace(*match.Scheme)) {
		case "http":
			schemeValue = cubevs.L7SchemeHTTP
		case "https":
			schemeValue = cubevs.L7SchemeHTTPS
		default:
			return 0, 0, fmt.Errorf("scheme must be http or https, got %q", *match.Scheme)
		}
	}

	if match.Port == nil {
		// Scheme only → fill in scheme's canonical default port.
		return defaultPortForScheme(schemeValue), schemeValue, nil
	}

	if match.Scheme == nil {
		return 0, 0, errors.New("port requires scheme")
	}

	p := *match.Port
	if p <= 0 || p > 65535 {
		return 0, 0, fmt.Errorf("port must be in [1, 65535], got %d", p)
	}
	return uint16(p), schemeValue, nil
}

// defaultPortForScheme returns the conventional port for a bare scheme.
// http → 80, https → 443. Any other scheme value returns 0 (should never be
// reached: extractL7PortScheme rejects unknown schemes before calling this).
func defaultPortForScheme(scheme uint8) uint16 {
	switch scheme {
	case cubevs.L7SchemeHTTP:
		return 80
	case cubevs.L7SchemeHTTPS:
		return 443
	default:
		return 0
	}
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
