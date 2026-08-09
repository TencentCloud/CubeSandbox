package cubevs

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

func TestSplitAllowOutTargets(t *testing.T) {
	cidrs, domains, err := splitAllowOutTargets([]string{
		" 8.8.8.8 ",
		"10.0.0.0/8",
		"api.example.com",
		"*.github.com.",
	})
	if err != nil {
		t.Fatalf("splitAllowOutTargets returned error: %v", err)
	}

	wantCIDRs := []string{"8.8.8.8", "10.0.0.0/8"}
	if !reflect.DeepEqual(cidrs, wantCIDRs) {
		t.Fatalf("cidrs=%v, want %v", cidrs, wantCIDRs)
	}

	wantDomains := []string{"api.example.com", "*.github.com."}
	if !reflect.DeepEqual(domains, wantDomains) {
		t.Fatalf("domains=%v, want %v", domains, wantDomains)
	}
}

func TestSplitAllowOutTargetsRejectsInvalidTargets(t *testing.T) {
	tests := []struct {
		name    string
		targets []string
	}{
		{name: "empty", targets: []string{""}},
		{name: "invalid cidr", targets: []string{"10.0.0.0/foo"}},
		{name: "invalid ipv4", targets: []string{"999.999.999.999"}},
		{name: "ipv6", targets: []string{"2001:db8::1"}},
		{name: "middle wildcard", targets: []string{"api.*.example.com"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := splitAllowOutTargets(tt.targets); err == nil {
				t.Fatalf("splitAllowOutTargets(%v) returned nil error", tt.targets)
			}
		})
	}
}

func TestValidateNetPolicyEntryCountsUsesFinalMapTargets(t *testing.T) {
	allowOutCIDRs := repeatedCIDRs(maxNetPolicyEntries)
	l7AllowOutCIDRs := []string{"198.51.100.1"}
	dnsAllowDomains := repeatedDomains(maxDNSAllowDomains)
	l7DNSAllowDomains := []string{"api-extra.example.com"}
	denyOut := append(repeatedCIDRs(maxNetPolicyEntries-len(alwaysDeniedSandboxCIDRs)+1), alwaysDeniedSandboxCIDRs...)

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "allow out v3 counts allow and l7 cidrs",
			err:  validateNetPolicyEntryCounts(allowOutCIDRs, l7AllowOutCIDRs, nil, nil, nil),
			want: "network.allow_out_v3 exceeds maximum entries: got 8193, max 8192",
		},
		{
			name: "dns allow counts allow and l7 domains",
			err:  validateNetPolicyEntryCounts(nil, nil, dnsAllowDomains, l7DNSAllowDomains, nil),
			want: "network.dns_allow exceeds maximum entries: got 1025, max 1024",
		},
		{
			name: "deny out counts effective deny cidrs",
			err:  validateNetPolicyEntryCounts(nil, nil, nil, nil, denyOut),
			want: "network.deny_out exceeds maximum entries: got 8193, max 8192",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err == nil {
				t.Fatalf("validateNetPolicyEntryCounts returned nil error")
			}
			if got := tt.err.Error(); got != tt.want {
				t.Fatalf("error=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateNetPolicyEntryCountsDeduplicatesByMapKey(t *testing.T) {
	err := validateNetPolicyEntryCounts(
		[]string{"198.51.100.1", "198.51.100.1/32"},
		[]string{"198.51.100.1"},
		[]string{"API.Example.COM."},
		[]string{"api.example.com"},
		[]string{"203.0.113.1", "203.0.113.1/32"},
	)
	if err != nil {
		t.Fatalf("validateNetPolicyEntryCounts returned error: %v", err)
	}
}

func TestBuildDNSAllowRulesDeduplicatesFlagsAndPorts(t *testing.T) {
	// Plain L3 domain rules go through buildDNSAllowRules; L7 domain rules
	// with per-host port sets go through buildL7Plan and are merged in with
	// mergeDNSAllowRules. Verify each half separately here.
	base, err := buildDNSAllowRules([]string{"api.example.com", "*.github.com", "API.Example.COM."})
	if err != nil {
		t.Fatalf("buildDNSAllowRules returned error: %v", err)
	}
	if got, want := len(base), 2; got != want {
		t.Fatalf("len(base)=%d, want %d", got, want)
	}
	for _, rule := range base {
		if rule.value.Flags != 0 {
			t.Fatalf("base rule %q should have no flags, got %d", rule.domain, rule.value.Flags)
		}
		if rule.value.PortCount != 0 {
			t.Fatalf("base rule %q should have no port set, got %d", rule.domain, rule.value.PortCount)
		}
	}
}

func TestBuildAllowOutPolicyEntriesDeduplicatesByKey(t *testing.T) {
	entries, err := buildAllowOutPolicyEntries(
		[]string{"198.51.100.1", "203.0.113.0/24", "198.51.100.1/32"},
	)
	if err != nil {
		t.Fatalf("buildAllowOutPolicyEntries returned error: %v", err)
	}
	if got, want := len(entries), 2; got != want {
		t.Fatalf("len(entries)=%d, want %d", got, want)
	}
	// First occurrence wins for the source label.
	if entries[0].source != "198.51.100.1" {
		t.Fatalf("first source=%q, want first occurrence", entries[0].source)
	}
	if entries[1].source != "203.0.113.0/24" {
		t.Fatalf("second source=%q", entries[1].source)
	}
	// Non-L7 allow_out entries carry no flags and no port set.
	for _, e := range entries {
		if e.flags != 0 {
			t.Fatalf("entry %q flags=%d, want 0", e.source, e.flags)
		}
		if len(e.ports) != 0 {
			t.Fatalf("entry %q ports=%v, want empty", e.source, e.ports)
		}
	}
}

func TestBuildDenyOutPolicyEntriesDeduplicates(t *testing.T) {
	entries, err := buildDenyOutPolicyEntries([]string{
		"192.0.2.1",
		"192.0.2.1/32",
		"198.51.100.0/24",
	})
	if err != nil {
		t.Fatalf("buildDenyOutPolicyEntries returned error: %v", err)
	}
	if got, want := len(entries), 2; got != want {
		t.Fatalf("len(entries)=%d, want %d", got, want)
	}
	if entries[0].source != "192.0.2.1" {
		t.Fatalf("duplicate source=%q, want first source", entries[0].source)
	}
}

func TestAppendDenyOutPolicyEntriesDeduplicatesExisting(t *testing.T) {
	dst, err := buildDenyOutPolicyEntries([]string{"192.168.0.0/16"})
	if err != nil {
		t.Fatalf("buildDenyOutPolicyEntries(dst) returned error: %v", err)
	}
	src, err := buildDenyOutPolicyEntries([]string{"192.168.0.0/16", "10.0.0.0/8"})
	if err != nil {
		t.Fatalf("buildDenyOutPolicyEntries(src) returned error: %v", err)
	}

	got := appendDenyOutPolicyEntries(dst, src)
	if len(got) != 2 {
		t.Fatalf("len(got)=%d, want 2", len(got))
	}
	if got[0].source != "192.168.0.0/16" {
		t.Fatalf("duplicate source=%q, want existing dst source", got[0].source)
	}
	if got[1].source != "10.0.0.0/8" {
		t.Fatalf("appended source=%q, want src-only entry", got[1].source)
	}
}

func TestBuildNetPolicyPlanDeduplicatesAndMergesFlags(t *testing.T) {
	allowOut := []string{"api.example.com", "198.51.100.1"}
	l7AllowOut := []L7Target{
		{Host: "API.Example.COM."},
		{Host: "198.51.100.1/32"},
	}
	denyOut := []string{"192.168.0.0/16", "203.0.113.0/24"}

	plan, err := buildNetPolicyPlan(MVMOptions{
		AllowOut:   &allowOut,
		L7AllowOut: &l7AllowOut,
		DenyOut:    &denyOut,
	})
	if err != nil {
		t.Fatalf("buildNetPolicyPlan returned error: %v", err)
	}

	if got, want := len(plan.allowOutEntries), 1; got != want {
		t.Fatalf("len(plan.allowOutEntries)=%d, want %d", got, want)
	}
	if got, want := plan.allowOutEntries[0].flags, uint8(netPolicyFlagL7Required); got != want {
		t.Fatalf("allow out flags=%d, want %d", got, want)
	}
	if got, want := len(plan.dnsAllowRules), 1; got != want {
		t.Fatalf("len(plan.dnsAllowRules)=%d, want %d", got, want)
	}
	if got, want := plan.dnsAllowRules[0].value.Flags, uint8(netPolicyFlagL7Required); got != want {
		t.Fatalf("dns allow flags=%d, want %d", got, want)
	}
	if got, want := plan.dnsPolicyFlags, uint8(dnsPolicyFlagLearningEnabled); got != want {
		t.Fatalf("dnsPolicyFlags=%d, want %d", got, want)
	}
	if got, want := len(plan.denyOutEntries), 2; got != want {
		t.Fatalf("len(plan.denyOutEntries)=%d, want %d", got, want)
	}
	if got, want := len(effectiveDenyOutEntriesForReplace(plan)), len(alwaysDeniedSandboxEntries)+1; got != want {
		t.Fatalf("len(effectiveDenyOutEntriesForReplace(plan))=%d, want %d", got, want)
	}
}

func TestBuildNetPolicyPlanBlockAllKeepsDefaultDenyOutOnReplace(t *testing.T) {
	allowInternetAccess := false
	plan, err := buildNetPolicyPlan(MVMOptions{AllowInternetAccess: &allowInternetAccess})
	if err != nil {
		t.Fatalf("buildNetPolicyPlan returned error: %v", err)
	}
	if got, want := len(plan.denyOutEntries), 1; got != want {
		t.Fatalf("len(plan.denyOutEntries)=%d, want %d", got, want)
	}
	if got, want := len(effectiveDenyOutEntriesForReplace(plan)), len(alwaysDeniedSandboxEntries)+1; got != want {
		t.Fatalf("len(effectiveDenyOutEntriesForReplace(plan))=%d, want %d", got, want)
	}
}

func TestPolicyEntryBuildersRejectBlankCIDR(t *testing.T) {
	tests := []struct {
		name string
		fn   func() error
	}{
		{
			name: "allow out builder",
			fn: func() error {
				_, err := buildAllowOutPolicyEntries([]string{"   "})
				return err
			},
		},
		{
			name: "deny out builder",
			fn: func() error {
				_, err := buildDenyOutPolicyEntries([]string{"   "})
				return err
			},
		},
		{
			name: "net policy plan allow out",
			fn: func() error {
				allowOut := []string{"   "}
				_, err := buildNetPolicyPlan(MVMOptions{AllowOut: &allowOut})
				return err
			},
		},
		{
			name: "net policy plan deny out",
			fn: func() error {
				denyOut := []string{"   "}
				_, err := buildNetPolicyPlan(MVMOptions{DenyOut: &denyOut})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.fn(); err == nil {
				t.Fatalf("returned nil error")
			}
		})
	}
}

func TestAlwaysDeniedSandboxEntriesInitializes(t *testing.T) {
	entries, err := buildDenyOutPolicyEntries(alwaysDeniedSandboxCIDRs)
	if err != nil {
		t.Fatalf("buildDenyOutPolicyEntries(alwaysDeniedSandboxCIDRs) returned error: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("alwaysDeniedSandboxCIDRs produced no entries")
	}
	if got, want := len(alwaysDeniedSandboxEntries), len(entries); got != want {
		t.Fatalf("len(alwaysDeniedSandboxEntries)=%d, want %d", got, want)
	}
}

func repeatedCIDRs(count int) []string {
	entries := make([]string, count)
	for i := range entries {
		entries[i] = fmt.Sprintf("198.%d.%d.%d", 18+i/65536, (i/256)%256, i%256)
	}
	return entries
}

// -------- L7 port + scheme merge tests -------------------------------------

func TestBuildL7Plan_DefaultRuleImpliesEmptyPortSet(t *testing.T) {
	// A single rule without port/scheme should be encoded as PortCount=0 in
	// the dns_allow value so the datapath falls back to {80, 443} at match
	// time. This is the backward-compat path.
	_, dns, err := buildL7Plan([]L7Target{{Host: "api.example.com"}})
	if err != nil {
		t.Fatalf("buildL7Plan returned error: %v", err)
	}
	if len(dns) != 1 {
		t.Fatalf("len(dns)=%d, want 1", len(dns))
	}
	if got := dns[0].value.PortCount; got != 0 {
		t.Fatalf("PortCount=%d, want 0 (unspecified rule keeps default set)", got)
	}
	if got := dns[0].value.Flags; got != uint8(netPolicyFlagL7Required) {
		t.Fatalf("Flags=%d, want %d", got, netPolicyFlagL7Required)
	}
}

func TestBuildL7Plan_ExplicitPortsSetPortCount(t *testing.T) {
	// Two rules: default 80/443 + explicit 8080/http. The default gets
	// expanded for conflict checking, so the resulting port set is
	// {80, 443, 8080}, all http/https appropriately.
	_, dns, err := buildL7Plan([]L7Target{
		{Host: "api.example.com"},
		{Host: "api.example.com", Port: 8080, Scheme: L7SchemeHTTP},
	})
	if err != nil {
		t.Fatalf("buildL7Plan returned error: %v", err)
	}
	if len(dns) != 1 {
		t.Fatalf("len(dns)=%d, want 1 (rules for same host merge)", len(dns))
	}
	if got, want := int(dns[0].value.PortCount), 3; got != want {
		t.Fatalf("PortCount=%d, want %d", got, want)
	}

	// Verify each expected port is present with the correct scheme.
	found := map[uint16]uint8{}
	for i := uint8(0); i < dns[0].value.PortCount; i++ {
		p := dns[0].value.Ports[i]
		found[ntohsPort(p.Port)] = p.Scheme
	}
	for _, expect := range []struct {
		port   uint16
		scheme uint8
	}{
		{80, L7SchemeHTTP},
		{443, L7SchemeHTTPS},
		{8080, L7SchemeHTTP},
	} {
		if got, ok := found[expect.port]; !ok || got != expect.scheme {
			t.Fatalf("port %d: got scheme %d (present=%v), want %d",
				expect.port, got, ok, expect.scheme)
		}
	}
}

func TestBuildL7Plan_PreservesFirstAppearancePortOrder(t *testing.T) {
	_, dns, err := buildL7Plan([]L7Target{
		{Host: "api.example.com", Port: 9002, Scheme: L7SchemeHTTP},
		{Host: "api.example.com", Port: 9001, Scheme: L7SchemeHTTPS},
		{Host: "api.example.com", Port: 9003, Scheme: L7SchemeHTTP},
		{Host: "api.example.com", Port: 9001, Scheme: L7SchemeHTTPS},
	})
	if err != nil {
		t.Fatalf("buildL7Plan returned error: %v", err)
	}
	if len(dns) != 1 {
		t.Fatalf("len(dns)=%d, want 1", len(dns))
	}
	want := []uint16{9002, 9001, 9003}
	if got := int(dns[0].value.PortCount); got != len(want) {
		t.Fatalf("PortCount=%d, want %d", got, len(want))
	}
	for i, port := range want {
		if got := ntohsPort(dns[0].value.Ports[i].Port); got != port {
			t.Fatalf("Ports[%d]=%d, want %d", i, got, port)
		}
	}
}

func TestBuildL7Plan_SchemeConflictOnSamePortRejected(t *testing.T) {
	// (host, port=443) with scheme=http conflicts with the default rule's
	// implicit (host, 443, https). iptables can only steer 443 to one
	// listener, so this must be rejected at rule-build time.
	_, _, err := buildL7Plan([]L7Target{
		{Host: "api.example.com"},
		{Host: "api.example.com", Port: 443, Scheme: L7SchemeHTTP},
	})
	if err == nil {
		t.Fatal("expected scheme conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("error should mention conflict, got %v", err)
	}
}

func TestBuildL7Plan_SchemeConflictOnExplicitPortRejected(t *testing.T) {
	// Two explicit rules for the same (host, port) with different schemes.
	_, _, err := buildL7Plan([]L7Target{
		{Host: "api.example.com", Port: 8080, Scheme: L7SchemeHTTP},
		{Host: "api.example.com", Port: 8080, Scheme: L7SchemeHTTPS},
	})
	if err == nil {
		t.Fatal("expected scheme conflict error, got nil")
	}
}

func TestBuildL7Plan_MergesDNSHostCaseVariants(t *testing.T) {
	// "API.example.com", "api.example.com." and "api.example.com" all
	// normalise to the same dns_allow key (case / trailing dot). They must
	// aggregate into one host so every port survives — previously each raw
	// string formed its own group and the later same-key merge silently
	// dropped the earlier port set.
	_, dns, err := buildL7Plan([]L7Target{
		{Host: "API.example.com", Port: 8080, Scheme: L7SchemeHTTP},
		{Host: "api.example.com.", Port: 9090, Scheme: L7SchemeHTTP},
	})
	if err != nil {
		t.Fatalf("buildL7Plan returned error: %v", err)
	}
	if len(dns) != 1 {
		t.Fatalf("len(dns)=%d, want 1 (case/dot variants merge into one host)", len(dns))
	}
	found := map[uint16]uint8{}
	for i := uint8(0); i < dns[0].value.PortCount; i++ {
		p := dns[0].value.Ports[i]
		found[ntohsPort(p.Port)] = p.Scheme
	}
	for _, port := range []uint16{8080, 9090} {
		if _, ok := found[port]; !ok {
			t.Fatalf("port %d dropped after merging host variants (got %v)", port, found)
		}
	}
}

func TestBuildL7Plan_DetectsDNSSchemeConflictAcrossCaseVariants(t *testing.T) {
	// Same normalised host, same port, different scheme => conflict, even
	// though the raw host strings differ only by case. Previously the two
	// groups passed conflict detection independently and the conflict was
	// silently resolved last-write-wins.
	_, _, err := buildL7Plan([]L7Target{
		{Host: "X.com", Port: 443, Scheme: L7SchemeHTTP},
		{Host: "x.com", Port: 443, Scheme: L7SchemeHTTPS},
	})
	if err == nil {
		t.Fatal("buildL7Plan did not detect scheme conflict across case variants")
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("error should mention conflict, got %v", err)
	}
}

func TestBuildL7Plan_MergesCIDRNotationVariants(t *testing.T) {
	// "1.2.3.4" and "1.2.3.4/32" parse to the same lpm key. They must
	// aggregate into one CIDR entry so both ports survive.
	cidrs, _, err := buildL7Plan([]L7Target{
		{Host: "1.2.3.4", Port: 8080, Scheme: L7SchemeHTTP},
		{Host: "1.2.3.4/32", Port: 9090, Scheme: L7SchemeHTTP},
	})
	if err != nil {
		t.Fatalf("buildL7Plan returned error: %v", err)
	}
	if len(cidrs) != 1 {
		t.Fatalf("len(cidrs)=%d, want 1 (notation variants merge into one host)", len(cidrs))
	}
	found := map[uint16]uint8{}
	for _, p := range cidrs[0].ports {
		found[ntohsPort(p.Port)] = p.Scheme
	}
	for _, port := range []uint16{8080, 9090} {
		if _, ok := found[port]; !ok {
			t.Fatalf("port %d dropped after merging CIDR notation variants (got %v)", port, found)
		}
	}
}

func TestBuildL7Plan_DetectsCIDRSchemeConflictAcrossNotationVariants(t *testing.T) {
	// Same lpm key, same port, different scheme => conflict, even though the
	// raw CIDR strings differ only by notation.
	_, _, err := buildL7Plan([]L7Target{
		{Host: "1.2.3.4", Port: 443, Scheme: L7SchemeHTTP},
		{Host: "1.2.3.4/32", Port: 443, Scheme: L7SchemeHTTPS},
	})
	if err == nil {
		t.Fatal("buildL7Plan did not detect scheme conflict across CIDR notation variants")
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("error should mention conflict, got %v", err)
	}
}

func TestBuildL7Plan_SubnetCIDRRejected(t *testing.T) {
	// A subnet CIDR (prefixlen<32) is not a valid L7 host: the datapath
	// matches exact (ip, port)/48 pairs and cannot express a subnet+port
	// rule, so it must be rejected rather than silently narrowed to the
	// network address. A /32 host and a domain remain valid.
	if _, _, err := buildL7Plan([]L7Target{{Host: "10.0.0.0/24", Port: 443, Scheme: L7SchemeHTTPS}}); err == nil {
		t.Fatal("buildL7Plan accepted a subnet CIDR L7 host, want rejection")
	}
	if _, _, err := buildL7Plan([]L7Target{{Host: "1.2.3.4", Port: 443, Scheme: L7SchemeHTTPS}}); err != nil {
		t.Fatalf("single host IP (/32) must remain valid, got %v", err)
	}
	if _, _, err := buildL7Plan([]L7Target{{Host: "api.example.com", Port: 443, Scheme: L7SchemeHTTPS}}); err != nil {
		t.Fatalf("domain host must remain valid, got %v", err)
	}
}

func TestExpandedAllowOutEntryCount(t *testing.T) {
	// Plain allow -> 1 entry; default-port L7 -> 2 (80/443); explicit-port L7
	// -> len(ports). Budget validation must use this expanded count, not one
	// per host.
	defaultL7 := allowOutPolicyEntry{flags: uint8(netPolicyFlagL7Required)}
	plain := allowOutPolicyEntry{flags: 0}
	explicitL7 := allowOutPolicyEntry{
		flags: uint8(netPolicyFlagL7Required),
		ports: []l7PortEntry{
			{Port: htonsPort(8080), Scheme: L7SchemeHTTP},
			{Port: htonsPort(8443), Scheme: L7SchemeHTTPS},
		},
	}
	entries := []allowOutPolicyEntry{defaultL7, plain, explicitL7}
	// 2 (default) + 1 (plain) + 2 (explicit) = 5
	if got := expandedAllowOutEntryCount(entries); got != 5 {
		t.Fatalf("expandedAllowOutEntryCount=%d, want 5", got)
	}
}

func TestValidateNetPolicyPlanCountsExpandedL7Ports(t *testing.T) {
	// Each L7 host with 8 explicit ports occupies 8 inner-map entries, not 1.
	// 1025 such hosts need 8200 entries > maxNetPolicyEntries (8192); the old
	// len(allowOutEntries) check counted 1025 and passed, undercounting the
	// /48 expansion and letting population overflow mid-write (E2BIG).
	mkPorts := func() []l7PortEntry {
		ports := make([]l7PortEntry, 0, maxL7PortsPerHost)
		for i := 0; i < maxL7PortsPerHost; i++ {
			ports = append(ports, l7PortEntry{Port: htonsPort(uint16(8000 + i)), Scheme: L7SchemeHTTP})
		}
		return ports
	}
	l7Entry := allowOutPolicyEntry{
		key:   lpmKey{Prefixlen: 32, IP: 0x0a000001},
		flags: uint8(netPolicyFlagL7Required),
		ports: mkPorts(),
	}

	// 1024 hosts -> 8192 entries == budget: allowed.
	plan := &netPolicyPlan{}
	for i := 0; i < 1024; i++ {
		plan.allowOutEntries = append(plan.allowOutEntries, l7Entry)
	}
	if err := validateNetPolicyPlan(plan); err != nil {
		t.Fatalf("1024 8-port L7 hosts (8192 entries) should fit the budget, got %v", err)
	}

	// 1025 hosts -> 8200 entries > budget: must reject.
	plan.allowOutEntries = append(plan.allowOutEntries, l7Entry)
	if err := validateNetPolicyPlan(plan); err == nil {
		t.Fatal("1025 8-port L7 hosts (8200 entries) should exceed the budget, got nil")
	}
}

func TestBuildL7Plan_SameHostPortSchemeIsIdempotent(t *testing.T) {
	// The same (host, port, scheme) tuple appearing twice is fine — different
	// rules may share the tuple to attach different lua policy actions in
	// CubeEgress. Only the shared datapath entry needs to be deduplicated.
	_, dns, err := buildL7Plan([]L7Target{
		{Host: "api.example.com", Port: 8080, Scheme: L7SchemeHTTP},
		{Host: "api.example.com", Port: 8080, Scheme: L7SchemeHTTP},
	})
	if err != nil {
		t.Fatalf("duplicate tuple must be idempotent, got err %v", err)
	}
	if len(dns) != 1 || dns[0].value.PortCount != 1 {
		t.Fatalf("dns=%+v, want single rule with 1 port", dns)
	}
	if got := ntohsPort(dns[0].value.Ports[0].Port); got != 8080 {
		t.Fatalf("port=%d, want 8080", got)
	}
	if dns[0].value.Ports[0].Scheme != L7SchemeHTTP {
		t.Fatalf("scheme=%d, want L7SchemeHTTP", dns[0].value.Ports[0].Scheme)
	}
}

func TestBuildL7Plan_MultiHostIndependent(t *testing.T) {
	// Two hosts sharing a port at different schemes must NOT conflict — the
	// scheme-consistency rule is per (host, port), not per port.
	_, dns, err := buildL7Plan([]L7Target{
		{Host: "api.example.com", Port: 8080, Scheme: L7SchemeHTTP},
		{Host: "other.example.com", Port: 8080, Scheme: L7SchemeHTTPS},
	})
	if err != nil {
		t.Fatalf("cross-host same-port with different schemes must be allowed: %v", err)
	}
	if len(dns) != 2 {
		t.Fatalf("expected two dns rules, got %d", len(dns))
	}
}

func TestBuildL7Plan_IPClassifiedAsCIDR(t *testing.T) {
	// An IP-literal L7 target lands in allow_out_v3 as a static entry with
	// L7_REQUIRED and the explicit (port, scheme) set copied into ports[].
	cidrs, dns, err := buildL7Plan([]L7Target{
		{Host: "1.2.3.4", Port: 8443, Scheme: L7SchemeHTTPS},
	})
	if err != nil {
		t.Fatalf("buildL7Plan returned error: %v", err)
	}
	if len(dns) != 0 {
		t.Fatalf("dns entries=%d, want 0 for IP-literal host", len(dns))
	}
	if len(cidrs) != 1 {
		t.Fatalf("cidrs=%d, want 1", len(cidrs))
	}
	if got := cidrs[0].flags; got != uint8(netPolicyFlagL7Required) {
		t.Fatalf("flags=%d, want %d", got, netPolicyFlagL7Required)
	}
	if len(cidrs[0].ports) != 1 || ntohsPort(cidrs[0].ports[0].Port) != 8443 {
		t.Fatalf("ports=%+v, want [{Port:8443, Scheme:HTTPS}]", cidrs[0].ports)
	}
	if cidrs[0].ports[0].Scheme != L7SchemeHTTPS {
		t.Fatalf("scheme=%d, want L7SchemeHTTPS", cidrs[0].ports[0].Scheme)
	}
}

func TestBuildL7Plan_PortBudgetExceededRejected(t *testing.T) {
	// More distinct (port, scheme) tuples than maxL7PortsPerHost — must fail
	// rather than silently truncate, so users see a real error at policy
	// submission time instead of dropped rules at runtime.
	targets := make([]L7Target, 0, maxL7PortsPerHost+1)
	for i := 0; i < maxL7PortsPerHost+1; i++ {
		targets = append(targets, L7Target{
			Host: "api.example.com",
			Port: uint16(9000 + i),
			// Half http, half https — no scheme conflict on distinct ports.
			Scheme: L7SchemeHTTP,
		})
	}
	if _, _, err := buildL7Plan(targets); err == nil {
		t.Fatal("expected budget-exceeded error, got nil")
	}
}

func TestBuildL7Plan_ExplicitRuleWithoutSchemeRejected(t *testing.T) {
	// A rule with Port set but Scheme == None (or vice versa) is a schema
	// error — the caller (extractL7PortScheme) is supposed to reject partial
	// specifications before we get here, but defense-in-depth is cheap.
	_, _, err := buildL7Plan([]L7Target{
		{Host: "api.example.com", Port: 8080, Scheme: L7SchemeNone},
	})
	if err == nil {
		t.Fatal("expected error for port-without-scheme, got nil")
	}
}

func TestApplyPortsToDNSAllowValue_Empty(t *testing.T) {
	// Empty ports slice must leave PortCount=0 so the datapath falls back to
	// the default set. Regression guard: don't accidentally set PortCount to
	// len(ports) when ports is nil.
	var v dnsAllowValue
	applyPortsToDNSAllowValue(&v, nil)
	if v.PortCount != 0 {
		t.Fatalf("PortCount=%d, want 0", v.PortCount)
	}
}

func repeatedDomains(count int) []string {
	entries := make([]string, count)
	for i := range entries {
		entries[i] = fmt.Sprintf("api-%d.example.com", i)
	}
	return entries
}

func TestNetPolicyValueV2Layout(t *testing.T) {
	// netPolicyValueV2 is the legacy 16-byte allow_out_v2 ABI, read only when
	// migrating a pre-v3 allow_out_v2 map to allow_out_v3.
	var value netPolicyValueV2
	if got, want := unsafe.Sizeof(value), uintptr(16); got != want {
		t.Fatalf("unsafe.Sizeof(netPolicyValueV2{})=%d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(value.ExpiresAtNS), uintptr(0); got != want {
		t.Fatalf("ExpiresAtNS offset=%d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(value.Flags), uintptr(8); got != want {
		t.Fatalf("Flags offset=%d, want %d", got, want)
	}
	if netPolicyFlagL7Required != 1 {
		t.Fatalf("netPolicyFlagL7Required=%d, want 1", netPolicyFlagL7Required)
	}
}

func TestNetPolicyValueV3Layout(t *testing.T) {
	// netPolicyValueV3 is the current on-disk ABI for allow_out_v3.
	// The port lives in the LPM key, so the value only carries the
	// scheme plus the DNS-learned expiry. Any layout drift silently
	// corrupts the persisted policy, so assert the exact offsets.
	var value netPolicyValueV3
	if got, want := unsafe.Sizeof(value), uintptr(16); got != want {
		t.Fatalf("unsafe.Sizeof(netPolicyValueV3{})=%d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(value.ExpiresAtNS), uintptr(0); got != want {
		t.Fatalf("ExpiresAtNS offset=%d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(value.Flags), uintptr(8); got != want {
		t.Fatalf("Flags offset=%d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(value.Scheme), uintptr(9); got != want {
		t.Fatalf("Scheme offset=%d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(value.KeyPrefixlen), uintptr(10); got != want {
		t.Fatalf("KeyPrefixlen offset=%d, want %d", got, want)
	}
}

func TestNetPolicyValueV3Expired(t *testing.T) {
	now := uint64(100)
	tests := []struct {
		name  string
		value netPolicyValueV3
		want  bool
	}{
		{name: "static", value: netPolicyValueV3{ExpiresAtNS: 0}, want: false},
		{name: "dynamic valid", value: netPolicyValueV3{ExpiresAtNS: now + 1}, want: false},
		{name: "dynamic expired", value: netPolicyValueV3{ExpiresAtNS: now}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := netPolicyValueV3Expired(tt.value, now); got != tt.want {
				t.Fatalf("netPolicyValueV3Expired()=%t, want %t", got, tt.want)
			}
		})
	}
}

func TestMakeDNSAllowRuleSetsL7Flag(t *testing.T) {
	key, value, err := makeDNSAllowRule("API.Example.COM.", uint8(netPolicyFlagL7Required))
	if err != nil {
		t.Fatalf("makeDNSAllowRule returned error: %v", err)
	}
	if value.Flags != uint8(netPolicyFlagL7Required) {
		t.Fatalf("value.Flags=%d, want %d", value.Flags, netPolicyFlagL7Required)
	}
	if got, want := unsafe.Sizeof(value), uintptr(40); got != want {
		t.Fatalf("unsafe.Sizeof(dnsAllowValue{})=%d, want %d", got, want)
	}
	if key.Name[int(value.NameLen)-1] != 0 {
		t.Fatalf("exact rule terminator=%d, want 0", key.Name[int(value.NameLen)-1])
	}
}

func TestMakeDNSAllowWildcardRulePreservesL7Flag(t *testing.T) {
	key, value, err := makeDNSAllowRule("*.Example.COM.", uint8(netPolicyFlagL7Required))
	if err != nil {
		t.Fatalf("makeDNSAllowRule returned error: %v", err)
	}
	if value.Flags != uint8(netPolicyFlagL7Required) {
		t.Fatalf("value.Flags=%d, want %d", value.Flags, netPolicyFlagL7Required)
	}
	if key.Name[int(value.NameLen)-1] != '.' {
		t.Fatalf("wildcard rule terminator=%d, want '.'", key.Name[int(value.NameLen)-1])
	}
}

func TestDNSPolicyFlagsForDomainsLearningOnly(t *testing.T) {
	tests := []struct {
		name         string
		allowDomains []string
		l7Domains    []string
		want         uint8
	}{
		{name: "disabled", want: 0},
		{name: "allow_out domain", allowDomains: []string{"api.example.com"}, want: dnsPolicyFlagLearningEnabled},
		{name: "l7 domain", l7Domains: []string{"api.example.com"}, want: dnsPolicyFlagLearningEnabled},
		{name: "allow_out and l7 domains", allowDomains: []string{"api.example.com"}, l7Domains: []string{"api.example.org"}, want: dnsPolicyFlagLearningEnabled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dnsPolicyFlagsForDomains(tt.allowDomains, tt.l7Domains)
			if got != tt.want {
				t.Fatalf("dnsPolicyFlagsForDomains()=%d, want %d", got, tt.want)
			}
		})
	}
}

func TestMVMMetadataLayoutAndDNSPolicyFlags(t *testing.T) {
	var meta mvmMetadata
	if got, want := unsafe.Sizeof(meta), uintptr(128); got != want {
		t.Fatalf("unsafe.Sizeof(mvmMetadata{})=%d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(meta.DNSPolicyFlags), uintptr(72); got != want {
		t.Fatalf("DNSPolicyFlags offset=%d, want %d", got, want)
	}
	if dnsPolicyFlagLearningEnabled != 1 {
		t.Fatalf("dnsPolicyFlagLearningEnabled=%d, want 1", dnsPolicyFlagLearningEnabled)
	}
}

func TestDNSAllowValueLayoutAndFlags(t *testing.T) {
	var value dnsAllowValue
	if got, want := unsafe.Sizeof(value), uintptr(40); got != want {
		t.Fatalf("unsafe.Sizeof(dnsAllowValue{})=%d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(value.NameLen), uintptr(0); got != want {
		t.Fatalf("NameLen offset=%d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(value.Flags), uintptr(4); got != want {
		t.Fatalf("Flags offset=%d, want %d", got, want)
	}
}

func TestDNSAllowDuplicateRulesMergeFlags(t *testing.T) {
	_, allowValue, err := makeDNSAllowRule("api.example.com", 0)
	if err != nil {
		t.Fatalf("makeDNSAllowRule returned error: %v", err)
	}
	_, l7Value, err := makeDNSAllowRule("API.Example.COM.", uint8(netPolicyFlagL7Required))
	if err != nil {
		t.Fatalf("makeDNSAllowRule returned error: %v", err)
	}

	allowValue.Flags |= l7Value.Flags
	if allowValue.Flags != uint8(netPolicyFlagL7Required) {
		t.Fatalf("merged Flags=%d, want %d", allowValue.Flags, netPolicyFlagL7Required)
	}
}

// attachAllowOutV3Inner creates an allow_out_v3 inner LPM trie and attaches
// it to the outer hash-of-maps under ifindex, returning the inner map.
func attachAllowOutV3Inner(t *testing.T, outer *ebpf.Map, ifindex uint32) *ebpf.Map {
	t.Helper()
	inner, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.LPMTrie,
		KeySize:    uint32(unsafe.Sizeof(lpmKeyV3{})),
		ValueSize:  uint32(unsafe.Sizeof(netPolicyValueV3{})),
		MaxEntries: maxNetPolicyEntries,
		Flags:      unix.BPF_F_NO_PREALLOC,
	})
	if err != nil {
		if bpfTestUnavailable(err) {
			t.Skipf("kernel BPF LPM trie unavailable: %v", err)
		}
		t.Fatalf("create allow_out_v3 inner: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	if err := outer.Put(&ifindex, inner); err != nil {
		t.Fatalf("attach allow_out_v3 inner: %v", err)
	}
	return inner
}

func mustLookupV3(t *testing.T, inner *ebpf.Map, key lpmKeyV3) netPolicyValueV3 {
	t.Helper()
	var value netPolicyValueV3
	if err := inner.Lookup(&key, &value); err != nil {
		t.Fatalf("lookup %+v: %v", key, err)
	}
	return value
}

// TestPopulateAllowOutStaticV3OverCoveringLearnedStaysStatic is the
// exact-key-match regression test for the userspace writer: a static /48
// written while a DNS-learned /32 (non-zero TTL) covers the same IP must
// stay static. The inner LPM lookup is longest-prefix, so without the
// KeyPrefixlen check the learned /32's TTL was wrongly inherited and the
// "static" /48 aged out at the learned TTL.
func TestPopulateAllowOutStaticV3OverCoveringLearnedStaysStatic(t *testing.T) {
	outer := newAllowOutV3OuterMap(t)
	ifindex := uint32(305)
	inner := attachAllowOutV3Inner(t, outer, ifindex)
	ip := mustParseCIDRForTest(t, "192.0.2.80").IP

	// Pre-existing DNS-learned /32 (temporary) covering the IP.
	learnedTTL := uint64(123456789)
	seed := lpmKeyV3{Prefixlen: 32, IP: ip, Port: 0}
	if err := inner.Update(&seed, &netPolicyValueV3{
		ExpiresAtNS: learnedTTL, Flags: testFlagMarker, KeyPrefixlen: 32,
	}, ebpf.UpdateAny); err != nil {
		t.Fatalf("seed learned /32: %v", err)
	}

	entries := []allowOutPolicyEntry{{
		key:   lpmKey{Prefixlen: 32, IP: ip},
		flags: netPolicyFlagL7Required,
		ports: []l7PortEntry{{Port: htonsPort(443), Scheme: L7SchemeHTTPS}},
	}}
	if err := populateAllowOutInnerMap(outer, ifindex, entries); err != nil {
		t.Fatalf("populateAllowOutInnerMap: %v", err)
	}

	got := mustLookupV3(t, inner, lpmKeyV3{Prefixlen: 48, IP: ip, Port: htonsPort(443)})
	if got.KeyPrefixlen != 48 {
		t.Fatalf("static /48 KeyPrefixlen=%d, want 48", got.KeyPrefixlen)
	}
	if got.ExpiresAtNS != 0 {
		t.Fatalf("static /48 inherited learned TTL %d from covering /32, want static (0)", got.ExpiresAtNS)
	}
	if got.Flags&testFlagMarker != 0 {
		t.Fatalf("static /48 inherited flags from covering /32: %#x", got.Flags)
	}
}

// TestPopulateAllowOutStaticV3OverExactLearnedStaticWins is the counterpart:
// a static /48 written over a learned entry for the EXACT same key merges
// the learned flags but the static (zero) expiry WINS — the entry becomes
// permanent. Keeping the learned TTL instead would let the reaper delete
// the entry at the old TTL and silently drop the static verdict.
func TestPopulateAllowOutStaticV3OverExactLearnedStaticWins(t *testing.T) {
	outer := newAllowOutV3OuterMap(t)
	ifindex := uint32(306)
	inner := attachAllowOutV3Inner(t, outer, ifindex)
	ip := mustParseCIDRForTest(t, "192.0.2.90").IP

	learnedTTL := uint64(123456789)
	seed := lpmKeyV3{Prefixlen: 48, IP: ip, Port: htonsPort(443)}
	if err := inner.Update(&seed, &netPolicyValueV3{
		ExpiresAtNS: learnedTTL, Flags: uint8(netPolicyFlagL7Required) | testFlagMarker,
		Scheme: L7SchemeHTTP, KeyPrefixlen: 48, // stale scheme: static write must overwrite
	}, ebpf.UpdateAny); err != nil {
		t.Fatalf("seed learned /48: %v", err)
	}

	entries := []allowOutPolicyEntry{{
		key:   lpmKey{Prefixlen: 32, IP: ip},
		flags: netPolicyFlagL7Required,
		ports: []l7PortEntry{{Port: htonsPort(443), Scheme: L7SchemeHTTPS}},
	}}
	if err := populateAllowOutInnerMap(outer, ifindex, entries); err != nil {
		t.Fatalf("populateAllowOutInnerMap: %v", err)
	}

	got := mustLookupV3(t, inner, lpmKeyV3{Prefixlen: 48, IP: ip, Port: htonsPort(443)})
	if got.ExpiresAtNS != 0 {
		t.Fatalf("static write over learned /48 must win with zero expiry, got %d (learned TTL was %d)", got.ExpiresAtNS, learnedTTL)
	}
	if got.Flags&testFlagMarker == 0 {
		t.Fatalf("exact learned /48 flags not merged: %#x", got.Flags)
	}
	if got.Scheme != L7SchemeHTTPS {
		t.Fatalf("scheme=%d after static write, want HTTPS (last write wins)", got.Scheme)
	}
}

// TestPopulateAllowOutStaticV3OverExactStaticMergesFlags covers the
// static-over-static same-key cell: the write must union the old entry's
// flags and keep the zero (static) expiry.
func TestPopulateAllowOutStaticV3OverExactStaticMergesFlags(t *testing.T) {
	outer := newAllowOutV3OuterMap(t)
	ifindex := uint32(308)
	inner := attachAllowOutV3Inner(t, outer, ifindex)
	ip := mustParseCIDRForTest(t, "192.0.2.100").IP

	seed := lpmKeyV3{Prefixlen: 48, IP: ip, Port: htonsPort(443)}
	if err := inner.Update(&seed, &netPolicyValueV3{
		Flags:  uint8(netPolicyFlagL7Required) | testFlagMarker,
		Scheme: L7SchemeHTTPS, KeyPrefixlen: 48,
	}, ebpf.UpdateAny); err != nil {
		t.Fatalf("seed static /48: %v", err)
	}

	entries := []allowOutPolicyEntry{{
		key:   lpmKey{Prefixlen: 32, IP: ip},
		flags: netPolicyFlagL7Required,
		ports: []l7PortEntry{{Port: htonsPort(443), Scheme: L7SchemeHTTPS}},
	}}
	if err := populateAllowOutInnerMap(outer, ifindex, entries); err != nil {
		t.Fatalf("populateAllowOutInnerMap: %v", err)
	}

	got := mustLookupV3(t, inner, lpmKeyV3{Prefixlen: 48, IP: ip, Port: htonsPort(443)})
	if got.ExpiresAtNS != 0 {
		t.Fatalf("static-over-static must stay static, got expiry %d", got.ExpiresAtNS)
	}
	if got.Flags&testFlagMarker == 0 {
		t.Fatalf("static-over-static lost old flags: %#x", got.Flags)
	}
	if got.Flags&uint8(netPolicyFlagL7Required) == 0 {
		t.Fatalf("static-over-static lost rule flags: %#x", got.Flags)
	}
}

// TestPopulateAllowOutPlainStaticOverwritesLearnedUnconditionally pins the
// plain (non-L7) path's NO-merge semantics: a static /32 overwrites a learned
// /32 outright — last write wins for flags, scheme AND expiry, with no
// same-key preservation (the plain path does not read the old entry at all).
func TestPopulateAllowOutPlainStaticOverwritesLearnedUnconditionally(t *testing.T) {
	outer := newAllowOutV3OuterMap(t)
	ifindex := uint32(309)
	inner := attachAllowOutV3Inner(t, outer, ifindex)
	ip := mustParseCIDRForTest(t, "192.0.2.110").IP

	seed := lpmKeyV3{Prefixlen: 32, IP: ip, Port: 0}
	if err := inner.Update(&seed, &netPolicyValueV3{
		ExpiresAtNS: 123456789, Flags: testFlagMarker, KeyPrefixlen: 32,
	}, ebpf.UpdateAny); err != nil {
		t.Fatalf("seed learned /32: %v", err)
	}

	entries := []allowOutPolicyEntry{{
		key:   lpmKey{Prefixlen: 32, IP: ip},
		flags: 0, // plain allow, no L7
	}}
	if err := populateAllowOutInnerMap(outer, ifindex, entries); err != nil {
		t.Fatalf("populateAllowOutInnerMap: %v", err)
	}

	got := mustLookupV3(t, inner, lpmKeyV3{Prefixlen: 32, IP: ip, Port: 0})
	if got.KeyPrefixlen != 32 {
		t.Fatalf("KeyPrefixlen=%d, want 32", got.KeyPrefixlen)
	}
	if got.ExpiresAtNS != 0 {
		t.Fatalf("plain static overwrite kept learned TTL %d", got.ExpiresAtNS)
	}
	if got.Flags != 0 {
		t.Fatalf("plain static overwrite kept old flags: %#x", got.Flags)
	}
	if got.Scheme != L7SchemeNone {
		t.Fatalf("scheme=%d, want NONE", got.Scheme)
	}
}
