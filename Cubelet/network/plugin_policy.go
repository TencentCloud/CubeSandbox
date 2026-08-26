// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	networkruntime "github.com/tencentcloud/CubeSandbox/Cubelet/network/runtime"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/netfile"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

// buildNetworkRuntimeCubeNetworkConfig prefers the structured request field and
// falls back to legacy annotations so older callers keep their previous egress
// policy behavior during the runtime migration.
func buildNetworkRuntimeCubeNetworkConfig(request *cubebox.RunCubeSandboxRequest) *networkruntime.CubeNetworkConfig {
	if request == nil {
		return nil
	}
	if request.GetCubeNetworkConfig() != nil {
		return mapRunRequestCubeNetworkConfig(request.GetCubeNetworkConfig())
	}
	return buildLegacyNetworkRuntimeCubeNetworkConfig(request.GetAnnotations())
}

func mapRunRequestCubeNetworkConfig(in *cubebox.CubeNetworkConfig) *networkruntime.CubeNetworkConfig {
	if in == nil {
		return nil
	}
	out := &networkruntime.CubeNetworkConfig{
		AllowOut: append([]string(nil), in.GetAllowOut()...),
		DenyOut:  append([]string(nil), in.GetDenyOut()...),
		Rules:    mapRunRequestEgressRules(in.GetRules()),
	}
	if in.AllowInternetAccess != nil {
		allowInternetAccess := in.GetAllowInternetAccess()
		out.AllowInternetAccess = &allowInternetAccess
	}
	return out
}

func mapRunRequestEgressRules(in []*cubebox.EgressRule) []*networkruntime.EgressRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]*networkruntime.EgressRule, 0, len(in))
	for _, r := range in {
		if r == nil {
			continue
		}
		out = append(out, &networkruntime.EgressRule{
			Name:   r.GetName(),
			Match:  mapRunRequestEgressRuleMatch(r.GetMatch()),
			Action: mapRunRequestEgressRuleAction(r.GetAction()),
		})
	}
	return out
}

func mapRunRequestEgressRuleMatch(in *cubebox.EgressRuleMatch) *networkruntime.EgressRuleMatch {
	if in == nil {
		return nil
	}
	out := &networkruntime.EgressRuleMatch{
		SNI:    in.Sni,
		Host:   in.Host,
		Method: append([]string(nil), in.GetMethod()...),
		Path:   in.Path,
		Scheme: in.Scheme,
	}
	if in.Port != nil {
		p := int(*in.Port)
		out.Port = &p
	}
	return out
}

func mapRunRequestEgressRuleAction(in *cubebox.EgressRuleAction) *networkruntime.EgressRuleAction {
	if in == nil {
		return nil
	}
	out := &networkruntime.EgressRuleAction{
		Allow: in.GetAllow(),
		Audit: in.Audit,
	}
	if len(in.GetInject()) > 0 {
		out.Inject = make([]*networkruntime.EgressRuleInject, 0, len(in.GetInject()))
		for _, inj := range in.GetInject() {
			if inj == nil {
				continue
			}
			out.Inject = append(out.Inject, &networkruntime.EgressRuleInject{
				Header: inj.GetHeader(),
				Secret: inj.GetSecret(),
				Format: inj.Format,
			})
		}
	}
	return out
}

func buildLegacyNetworkRuntimeCubeNetworkConfig(annotations map[string]string) *networkruntime.CubeNetworkConfig {
	if len(annotations) == 0 {
		return nil
	}
	if v, ok := annotations[constants.MasterAnnotationNetworkPolicyBlockAll]; ok && v == "true" {
		allowInternetAccess := false
		return &networkruntime.CubeNetworkConfig{AllowInternetAccess: &allowInternetAccess}
	}
	if v, ok := annotations[constants.MasterAnnotationNetworkPolicyAllowPublicServices]; ok && v == "true" {
		allowInternetAccess := true
		return &networkruntime.CubeNetworkConfig{AllowInternetAccess: &allowInternetAccess}
	}
	if v, ok := annotations[constants.MasterAnnotationNetworkPolicyDefault]; ok && v == "true" {
		allowInternetAccess := true
		return &networkruntime.CubeNetworkConfig{AllowInternetAccess: &allowInternetAccess}
	}
	return nil
}

func formatCubeNetworkAllowInternetAccess(cfg *networkruntime.CubeNetworkConfig) string {
	if cfg == nil || cfg.AllowInternetAccess == nil {
		return "default(true)"
	}
	if *cfg.AllowInternetAccess {
		return "true"
	}
	return "false"
}

func lenCubeNetworkList(cfg *networkruntime.CubeNetworkConfig, allow bool) int {
	if cfg == nil {
		return 0
	}
	if allow {
		return len(cfg.AllowOut)
	}
	return len(cfg.DenyOut)
}

func formatNetworkRuntimeCubeNetworkConfig(cfg *networkruntime.CubeNetworkConfig) string {
	if cfg == nil {
		return "allow_internet_access=default(true) allow_out=[] deny_out=[] rules=0"
	}
	return fmt.Sprintf(
		"allow_internet_access=%s allow_out=%v deny_out=%v rules=%d",
		formatCubeNetworkAllowInternetAccess(cfg),
		cfg.AllowOut,
		cfg.DenyOut,
		len(cfg.Rules),
	)
}

// mergeDNSAllowOutCIDRs appends resolver /32 CIDRs when a policy contains domain
// targets. Without this compatibility step, a sandbox with AllowInternetAccess=false
// could be allowed to reach a domain by policy but still fail DNS resolution.
func mergeDNSAllowOutCIDRs(ctx context.Context, cfg *networkruntime.CubeNetworkConfig, dnsServers []string) (*networkruntime.CubeNetworkConfig, []string) {
	if !shouldAppendDNSAllowOut(cfg) || len(dnsServers) == 0 {
		return cfg, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	out := cloneNetworkRuntimeCubeNetworkConfig(cfg)
	if out == nil {
		out = &networkruntime.CubeNetworkConfig{}
	}
	dnsAllowOutCIDRs := dnsServersToAllowOutCIDRs(ctx, dnsServers)
	// CubeVS AllowOut entries are CIDR-only today and cannot express UDP/TCP port 53.
	// These resolver CIDRs intentionally keep domain-based allow rules functional
	// even when AllowInternetAccess=false; restricting them to DNS ports requires a
	// network runtime/CubeVS policy-model extension.
	out.AllowOut = appendUniqueString(out.AllowOut, dnsAllowOutCIDRs)
	return out, dnsAllowOutCIDRs
}

// dnsServersToAllowOutCIDRs converts resolved DNS server addresses into
// allow_out CIDRs, dropping the ones CubeVS cannot express.
//
// Kept separate from mergeDNSAllowOutCIDRs because the two questions are
// different: whether to *merge* these into the policy depends on the policy
// naming a domain, but the list itself is a property of the sandbox and is
// recorded unconditionally so a later policy update can fold it back in.
func dnsServersToAllowOutCIDRs(ctx context.Context, dnsServers []string) []string {
	if ctx == nil {
		ctx = context.Background()
	}
	cidrs := make([]string, 0, len(dnsServers))
	for _, dnsServer := range dnsServers {
		cidr, ok := dnsServerToCIDR(dnsServer)
		if !ok {
			if isIPv6DNSServer(dnsServer) {
				log.G(ctx).Warnf("skip IPv6 DNS server when appending DNS allow-out CIDR: dns_server=%s", strings.TrimSpace(dnsServer))
			}
			continue
		}
		cidrs = append(cidrs, cidr)
	}
	return cidrs
}

// shouldAppendDNSAllowOut keeps the resolver exception narrow: pure IP/CIDR
// policies do not need DNS, but domain and L7 host/SNI rules do.
func shouldAppendDNSAllowOut(cfg *networkruntime.CubeNetworkConfig) bool {
	if cfg == nil {
		return false
	}

	for _, target := range cfg.AllowOut {
		if isAllowOutDomainTarget(target) {
			return true
		}
	}
	return hasL7DomainRuleTarget(cfg.Rules)
}

func isAllowOutDomainTarget(raw string) bool {
	target := strings.TrimSpace(raw)
	if target == "" {
		return false
	}
	if isIPv4NetworkTarget(target) {
		return false
	}
	if strings.Contains(target, "/") {
		return false
	}
	if net.ParseIP(target) != nil || isDottedDecimalLikeTarget(target) {
		return false
	}
	return isDNSAllowDomainTarget(target)
}

func hasL7DomainRuleTarget(rules []*networkruntime.EgressRule) bool {
	for _, rule := range rules {
		if rule == nil || rule.Match == nil {
			continue
		}
		if rule.Match.SNI != nil && isL7DomainTarget(*rule.Match.SNI) {
			return true
		}
		if rule.Match.Host != nil && isL7HostDomainTarget(*rule.Match.Host) {
			return true
		}
	}
	return false
}

func isL7HostDomainTarget(raw string) bool {
	target := strings.TrimSpace(raw)
	if target == "" {
		return false
	}
	if host, _, err := net.SplitHostPort(target); err == nil {
		target = host
	}
	if net.ParseIP(target) != nil {
		return false
	}
	if strings.Contains(target, "/") {
		return false
	}
	if isDottedDecimalLikeTarget(target) {
		return false
	}
	return isL7DomainTarget(target)
}

func isL7DomainTarget(raw string) bool {
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if isDottedDecimalLikeTarget(domain) {
		return false
	}
	return isValidDNSDomainTarget(domain)
}

func isIPv4NetworkTarget(target string) bool {
	if strings.Contains(target, "/") {
		ip, _, err := net.ParseCIDR(target)
		return err == nil && ip.To4() != nil
	}
	ip := net.ParseIP(target)
	return ip != nil && ip.To4() != nil
}

func isDottedDecimalLikeTarget(target string) bool {
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

func isDNSAllowDomainTarget(target string) bool {
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(target), "."))
	return isValidDNSDomainTarget(domain)
}

func isValidDNSDomainTarget(domain string) bool {
	if domain == "" || len(domain) >= 254 {
		return false
	}
	if strings.HasPrefix(domain, "*.") {
		domain = domain[2:]
	} else if strings.Contains(domain, "*") {
		return false
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

func cloneNetworkRuntimeCubeNetworkConfig(cfg *networkruntime.CubeNetworkConfig) *networkruntime.CubeNetworkConfig {
	if cfg == nil {
		return nil
	}
	out := &networkruntime.CubeNetworkConfig{
		AllowOut: append([]string(nil), cfg.AllowOut...),
		DenyOut:  append([]string(nil), cfg.DenyOut...),
		Rules:    cfg.Rules,
	}
	if cfg.AllowInternetAccess != nil {
		v := *cfg.AllowInternetAccess
		out.AllowInternetAccess = &v
	}
	return out
}

func dnsServerToCIDR(ip string) (string, bool) {
	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	if parsedIP == nil {
		return "", false
	}
	if ipv4 := parsedIP.To4(); ipv4 != nil {
		return ipv4.String() + "/32", true
	}
	return "", false
}

func isIPv6DNSServer(ip string) bool {
	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	return parsedIP != nil && parsedIP.To4() == nil
}

func appendUniqueString(base []string, extra []string) []string {
	if len(extra) == 0 {
		return append([]string(nil), base...)
	}
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := append([]string(nil), base...)
	for _, item := range base {
		seen[item] = struct{}{}
	}
	for _, item := range extra {
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

// ErrSandboxNetworkNotActive reports that the sandbox has no active network, so
// its policy cannot be updated. Callers map it to a client-visible conflict
// rather than an internal failure.
var ErrSandboxNetworkNotActive = networkruntime.ErrNetworkNotActive

// UpdateSandboxNetworkPolicy replaces a running sandbox's egress policy.
//
// cfg is the complete desired state as authored by the user; the runtime folds
// the sandbox's DNS resolver CIDRs back in, so callers pass exactly what the
// API received. Unlike Create there is no legacy-annotation fallback: the
// update API is new, so a caller that omits the config is a programming error
// rather than an old client.
func UpdateSandboxNetworkPolicy(ctx context.Context, sandboxID string, cfg *cubebox.CubeNetworkConfig) error {
	if dnm == nil || dnm.tapPlugin == nil || dnm.tapPlugin.networkRuntime == nil {
		return fmt.Errorf("network runtime is not initialized")
	}
	if sandboxID == "" {
		return fmt.Errorf("sandbox id is empty")
	}
	return dnm.tapPlugin.networkRuntime.UpdateNetworkPolicy(ctx, &networkruntime.UpdateNetworkPolicyRequest{
		SandboxID:         sandboxID,
		CubeNetworkConfig: mapRunRequestCubeNetworkConfig(cfg),
		DNSAllowOutCIDRs:  hostDNSAllowOutCIDRs(ctx),
	})
}

// hostDNSAllowOutCIDRs resolves the node's default DNS servers as allow-out
// CIDRs. The runtime only needs this for sandboxes created before it started
// recording its own resolver list; those it recorded win. Per-container DNS
// overrides are not recoverable here, so such a legacy sandbox falls back to
// the node defaults — still better than losing DNS outright.
func hostDNSAllowOutCIDRs(ctx context.Context) []string {
	servers, err := netfile.ResolveEffectiveDNSServers(nil)
	if err != nil {
		log.G(ctx).Warnf("update network policy: resolve host dns servers failed: %v", err)
		return nil
	}
	cidrs := make([]string, 0, len(servers))
	for _, server := range servers {
		if cidr, ok := dnsServerToCIDR(server); ok {
			cidrs = append(cidrs, cidr)
		}
	}
	return cidrs
}

// IsSandboxNetworkNotActive reports whether err means the sandbox has no active
// network, which is a caller mistake (wrong or stopped sandbox) rather than a
// node-side failure.
func IsSandboxNetworkNotActive(err error) bool {
	return errors.Is(err, ErrSandboxNetworkNotActive)
}
