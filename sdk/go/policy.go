// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"fmt"
	"net"
	"strings"
)

// L7 egress policy types — host/path/SNI matching, audit, credential
// injection. They are pure data holders; matching happens server-side.
// The JSON tags emit the camelCase shape that nests under network.rules,
// so json.Marshal alone produces the same wire format as the Python SDK.

// Match holds rule match conditions. All fields are optional; an empty Match
// matches any request. Fields are AND-ed; Method values are OR-ed;
// sni/host/scheme are compared case-insensitively server-side.
//
// Port + Scheme together drive which TCP port CubeEgress intercepts on the
// sandbox side. Both must be set together or both omitted:
//   - Both omitted → default set {80/http, 443/https} (backward compatible).
//   - Both set → CubeEgress intercepts only that (host, port, scheme) tuple.
//
// Every rule sharing the same (host, port) MUST agree on Scheme — a port can
// only route to one nginx listener. The server rejects the whole policy on
// mismatch.
type Match struct {
	SNI    string   `json:"sni,omitempty"`
	Host   string   `json:"host,omitempty"`
	Method []string `json:"method,omitempty"`
	Path   string   `json:"path,omitempty"`
	Scheme string   `json:"scheme,omitempty"`
	Port   int      `json:"port,omitempty"`
}

// validate checks the port/scheme pair, mirroring the Python and TypeScript
// SDKs and the CubeAPI/CubeMaster server-side contract: a set Port must be in
// [1, 65535] and must be paired with Scheme, and a set Scheme must be http or
// https (case-insensitive). Client-side validation catches typos before the
// network round-trip; the server rejects the same shapes.
func (m Match) validate() error {
	if m.Scheme != "" {
		scheme := strings.ToLower(strings.TrimSpace(m.Scheme))
		if scheme != "http" && scheme != "https" {
			return fmt.Errorf("match.scheme must be 'http' or 'https', got %q", m.Scheme)
		}
	}
	if m.Port != 0 {
		if m.Port < 1 || m.Port > 65535 {
			return fmt.Errorf("match.port must be in [1, 65535], got %d", m.Port)
		}
		if m.Scheme == "" {
			return fmt.Errorf("match.port requires match.scheme to be set")
		}
	}
	return nil
}

// normalized returns a copy with Scheme canonicalized (stripped, lowercased)
// so the wire form is consistent regardless of caller casing.
func (m Match) normalized() Match {
	m.Scheme = strings.ToLower(strings.TrimSpace(m.Scheme))
	return m
}

// Inject injects a credential header on an allowed HTTPS request whose
// SNI/Host matches (server-enforced). Format defaults to "${SECRET}".
type Inject struct {
	Header string `json:"header"`
	Secret string `json:"secret"`
	Format string `json:"format,omitempty"`
}

// Render returns the final injected header value (preview helper).
func (i Inject) Render() string {
	format := i.Format
	if format == "" {
		format = "${SECRET}"
	}
	return strings.ReplaceAll(format, "${SECRET}", i.Secret)
}

// Action is a rule action. Allow passes the request through (optionally
// injecting credentials); !Allow rejects it with HTTP 403. Audit defaults to
// "metadata" server-side when empty.
type Action struct {
	Allow  bool     `json:"allow"`
	Inject []Inject `json:"inject,omitempty"`
	Audit  string   `json:"audit,omitempty"`
}

// Rule is one L7 egress rule. Name is a human-readable audit label.
type Rule struct {
	Name   string `json:"name"`
	Match  Match  `json:"match"`
	Action Action `json:"action"`
}

const (
	denyAllIPv4CIDR               = "0.0.0.0/0"
	allowOutDomainRequiresDenyAll = "When specifying allowed domains in allow_out, you must disable public " +
		"outbound traffic or include '0.0.0.0/0' in deny_out to block all other traffic."
)

// validateAllowOutDomainsRequireDenyAll mirrors the current server contract:
// allowing specific domains is meaningful only when all other egress is denied,
// either by allowInternetAccess=false (defaultDenyAll) or by listing
// 0.0.0.0/0 in denyOut. Returns nil when allowOut carries no domain target.
// buildNetworkPayload translates NetworkOptions into the API's camelCase
// network object, validating it the same way the server will. Shared by sandbox
// creation and Sandbox.UpdateNetwork so both accept identical input.
func buildNetworkPayload(opts NetworkOptions, internetAccessDisabled bool) (map[string]any, error) {
	// Mirror the server-side contract: domain allowOut requires either
	// allowInternetAccess=false or an explicit deny-all CIDR in denyOut.
	if err := validateAllowOutDomainsRequireDenyAll(opts.AllowOut, opts.DenyOut, internetAccessDisabled); err != nil {
		return nil, err
	}

	network := map[string]any{}
	if opts.AllowPublicTraffic != nil {
		network["allowPublicTraffic"] = *opts.AllowPublicTraffic
	}
	if opts.MaskRequestHost != nil {
		network["maskRequestHost"] = *opts.MaskRequestHost
	}
	if len(opts.AllowOut) > 0 {
		network["allowOut"] = opts.AllowOut
	}
	if len(opts.DenyOut) > 0 {
		network["denyOut"] = opts.DenyOut
	}
	if len(opts.Rules) > 0 {
		rules := make([]Rule, len(opts.Rules))
		for i, rule := range opts.Rules {
			if err := rule.Match.validate(); err != nil {
				return nil, fmt.Errorf("network.rules[%d] %q: %w", i, rule.Name, err)
			}
			rule.Match = rule.Match.normalized()
			rules[i] = rule
		}
		network["rules"] = rules
	}
	return network, nil
}

func validateAllowOutDomainsRequireDenyAll(allowOut, denyOut []string, defaultDenyAll bool) error {
	hasDomain := false
	for _, target := range allowOut {
		if isDomainAllowOutTarget(target) {
			hasDomain = true
			break
		}
	}
	if !hasDomain || defaultDenyAll {
		return nil
	}
	for _, target := range denyOut {
		if strings.TrimSpace(target) == denyAllIPv4CIDR {
			return nil
		}
	}
	return &APIError{StatusCode: 400, Message: allowOutDomainRequiresDenyAll, Kind: apiErrorKindAPI}
}

// isDomainAllowOutTarget reports whether target is a DNS name (vs. an IP or
// CIDR). "*." wildcards are accepted; bare IPs and dotted-decimal-like values
// are not domains.
func isDomainAllowOutTarget(target string) bool {
	target = strings.TrimSpace(target)
	if target == "" || strings.Contains(target, "/") {
		return false
	}
	if net.ParseIP(target) != nil {
		return false
	}
	domain := strings.ToLower(strings.TrimRight(target, "."))
	if isDottedDecimalLike(domain) {
		return false
	}
	if strings.HasPrefix(domain, "*.") {
		domain = domain[2:]
	} else if strings.Contains(domain, "*") {
		return false
	}
	return isValidDNSDomainName(domain)
}

func isDottedDecimalLike(target string) bool {
	parts := strings.Split(strings.TrimRight(target, "."), ".")
	if len(parts) != 4 {
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

func isValidDNSDomainName(domain string) bool {
	if domain == "" || len(domain) >= 255 {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}
