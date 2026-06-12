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
