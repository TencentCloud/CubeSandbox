package cubevs

import (
	"fmt"
	"reflect"
	"testing"
	"unsafe"
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
			name: "allow out v2 counts allow and l7 cidrs",
			err:  validateNetPolicyEntryCounts(allowOutCIDRs, l7AllowOutCIDRs, nil, nil, nil),
			want: "network.allow_out_v2 exceeds maximum entries: got 8193, max 8192",
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

func TestBuildDNSAllowRulesDeduplicatesAndMergesFlags(t *testing.T) {
	rules, err := buildDNSAllowRules(
		[]string{"api.example.com", "*.github.com"},
		[]string{"API.Example.COM.", "*.GitHub.com."},
	)
	if err != nil {
		t.Fatalf("buildDNSAllowRules returned error: %v", err)
	}
	if got, want := len(rules), 2; got != want {
		t.Fatalf("len(rules)=%d, want %d", got, want)
	}

	byKey := make(map[dnsAllowKey]dnsAllowRule, len(rules))
	for _, rule := range rules {
		byKey[rule.key] = rule
	}
	for _, domain := range []string{"api.example.com", "*.github.com"} {
		key, _, err := makeDNSAllowRule(domain, 0)
		if err != nil {
			t.Fatalf("makeDNSAllowRule(%q) returned error: %v", domain, err)
		}
		rule, ok := byKey[key]
		if !ok {
			t.Fatalf("missing DNS allow rule for %q", domain)
		}
		if rule.value.Flags != uint8(netPolicyFlagL7Required) {
			t.Fatalf("rule %q flags=%d, want %d", domain, rule.value.Flags, netPolicyFlagL7Required)
		}
	}
}

func TestBuildAllowOutPolicyEntriesDeduplicatesAndMergesFlags(t *testing.T) {
	entries, err := buildAllowOutPolicyEntries(
		[]string{"198.51.100.1", "203.0.113.0/24"},
		[]string{"198.51.100.1/32", "203.0.113.0/24"},
	)
	if err != nil {
		t.Fatalf("buildAllowOutPolicyEntries returned error: %v", err)
	}
	if got, want := len(entries), 2; got != want {
		t.Fatalf("len(entries)=%d, want %d", got, want)
	}

	for _, entry := range entries {
		if entry.flags != uint8(netPolicyFlagL7Required) {
			t.Fatalf("entry %q flags=%d, want %d", entry.source, entry.flags, netPolicyFlagL7Required)
		}
	}
	if entries[0].source != "198.51.100.1" {
		t.Fatalf("first duplicate source=%q, want first source", entries[0].source)
	}
	if entries[1].source != "203.0.113.0/24" {
		t.Fatalf("second duplicate source=%q, want first source", entries[1].source)
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
	l7AllowOut := []string{"API.Example.COM.", "198.51.100.1/32"}
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
				_, err := buildAllowOutPolicyEntries([]string{"   "}, nil)
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

func repeatedDomains(count int) []string {
	entries := make([]string, count)
	for i := range entries {
		entries[i] = fmt.Sprintf("api-%d.example.com", i)
	}
	return entries
}

func TestNetPolicyValueV2Layout(t *testing.T) {
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

func TestNetPolicyValueV2Expired(t *testing.T) {
	now := uint64(100)
	tests := []struct {
		name  string
		value netPolicyValueV2
		want  bool
	}{
		{name: "static", value: netPolicyValueV2{ExpiresAtNS: 0}, want: false},
		{name: "dynamic valid", value: netPolicyValueV2{ExpiresAtNS: now + 1}, want: false},
		{name: "dynamic expired", value: netPolicyValueV2{ExpiresAtNS: now}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := netPolicyValueV2Expired(tt.value, now); got != tt.want {
				t.Fatalf("netPolicyValueV2Expired()=%t, want %t", got, tt.want)
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
	if got, want := unsafe.Sizeof(value), uintptr(8); got != want {
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
	if got, want := unsafe.Sizeof(value), uintptr(8); got != want {
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
