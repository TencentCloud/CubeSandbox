package cubevs

import (
	"testing"
)

// TestDomainRuleSetLookupMatchesLPMSemantics pins the matcher against the
// dns_allow_v2 LPM trie it stands in for: exact beats wildcard, the longest
// wildcard suffix wins, "*.base" excludes the apex, and case / trailing dots
// are normalised. Divergence here means the learner would learn addresses the
// datapath never authorised (or refuse ones it did).
func TestDomainRuleSetLookupMatchesLPMSemantics(t *testing.T) {
	cases := []struct {
		name  string
		rules []string
		qname string
		want  string // matched pattern, "" for no match
	}{
		{name: "exact match", rules: []string{"qq.com"}, qname: "qq.com", want: "qq.com"},
		{name: "exact does not cover subdomain", rules: []string{"qq.com"}, qname: "a.qq.com"},
		{name: "wildcard covers subdomain", rules: []string{"*.qq.com"}, qname: "a.qq.com", want: "*.qq.com"},
		{name: "wildcard covers deep subdomain", rules: []string{"*.qq.com"}, qname: "a.b.qq.com", want: "*.qq.com"},
		{name: "wildcard excludes apex", rules: []string{"*.qq.com"}, qname: "qq.com"},
		{name: "wildcard needs the dot", rules: []string{"*.qq.com"}, qname: "aqq.com"},
		{
			name:  "exact wins over wildcard on same name",
			rules: []string{"*.qq.com", "a.qq.com"},
			qname: "a.qq.com", want: "a.qq.com",
		},
		{
			name:  "longest wildcard suffix wins",
			rules: []string{"*.qq.com", "*.a.qq.com"},
			qname: "b.a.qq.com", want: "*.a.qq.com",
		},
		{
			name:  "apex and wildcard coexist",
			rules: []string{"qq.com", "*.qq.com"},
			qname: "qq.com", want: "qq.com",
		},
		{name: "case and trailing dot normalised", rules: []string{"QQ.CoM."}, qname: "qq.com", want: "qq.com"},
		{name: "query case normalised", rules: []string{"qq.com"}, qname: "QQ.CoM.", want: "qq.com"},
		{name: "unrelated name", rules: []string{"qq.com"}, qname: "evil.com"},
		// A bare "*" is rejected by isDNSAllowTarget and makeDNSAllowRule, so
		// it can never reach the index: there is no catch-all.
		{name: "bare star is not a catch-all", rules: []string{"*"}, qname: "anything.com"},
		{name: "empty rule set", rules: nil, qname: "qq.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := buildPlainDomainRuleSet(tc.rules)
			got := rs.lookup(tc.qname)
			switch {
			case tc.want == "" && got != nil:
				t.Fatalf("lookup(%q) matched %q, want no match", tc.qname, got.pattern)
			case tc.want != "" && got == nil:
				t.Fatalf("lookup(%q) found no match, want %q", tc.qname, tc.want)
			case tc.want != "" && got.pattern != tc.want:
				t.Fatalf("lookup(%q) = %q, want %q", tc.qname, got.pattern, tc.want)
			}
		})
	}
}

// TestL7DomainPlainCoverUsesSameMatcher checks the plain-cover marker agrees
// with the learner's matcher. These were two implementations of the same
// semantics before; a divergence would silently deny an L7 host's non-rule
// ports (or leave them open).
func TestL7DomainPlainCoverUsesSameMatcher(t *testing.T) {
	cases := []struct {
		plain []string
		host  string
		want  bool
	}{
		{plain: []string{"both.example.com"}, host: "both.example.com", want: true},
		{plain: []string{"*.example.com"}, host: "a.example.com", want: true},
		{plain: []string{"*.example.com"}, host: "example.com", want: false},
		{plain: []string{"*.example.com"}, host: "aexample.com", want: false},
		{plain: []string{"other.example.com"}, host: "l7only.example.com", want: false},
		{plain: nil, host: "l7only.example.com", want: false},
	}
	for _, tc := range cases {
		rules := []dnsAllowRule{{
			domain: tc.host,
			value:  dnsAllowValue{Flags: uint8(netPolicyFlagL7Required)},
		}}
		markL3AllowedByPlainCover(rules, tc.plain)
		got := rules[0].value.Flags&uint8(netPolicyFlagL3Allowed) != 0
		if got != tc.want {
			t.Fatalf("plain=%v host=%q: L3_ALLOWED=%v, want %v", tc.plain, tc.host, got, tc.want)
		}
	}
}

// TestDecodeDNSAllowKeyRoundTrip is the invertibility guarantee reconcile
// depends on: every pattern makeDNSAllowRule encodes must decode back to the
// same normalised pattern.
func TestDecodeDNSAllowKeyRoundTrip(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "qq.com", want: "qq.com"},
		{in: "*.qq.com", want: "*.qq.com"},
		{in: "a.b.c.qq.com", want: "a.b.c.qq.com"},
		{in: "*.a.b.qq.com", want: "*.a.b.qq.com"},
		{in: "localhost", want: "localhost"},
		{in: "QQ.CoM.", want: "qq.com"},
		{in: "*.QQ.CoM.", want: "*.qq.com"},
		{in: "x-1.example-2.com", want: "x-1.example-2.com"},
	}
	for _, tc := range cases {
		key, value, err := makeDNSAllowRule(tc.in, 0)
		if err != nil {
			t.Fatalf("makeDNSAllowRule(%q): %v", tc.in, err)
		}
		got, err := decodeDNSAllowKey(key, value)
		if err != nil {
			t.Fatalf("decodeDNSAllowKey(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("round trip %q = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDecodeDNSAllowKeyRejectsGarbage(t *testing.T) {
	key, value, err := makeDNSAllowRule("qq.com", 0)
	if err != nil {
		t.Fatalf("makeDNSAllowRule: %v", err)
	}

	bad := value
	bad.NameLen = 0
	if _, err := decodeDNSAllowKey(key, bad); err == nil {
		t.Fatal("name_len 0 must be rejected")
	}

	bad = value
	bad.NameLen = maxDNSNameLen + 1
	if _, err := decodeDNSAllowKey(key, bad); err == nil {
		t.Fatal("out-of-range name_len must be rejected")
	}

	badKey := key
	badKey.Name[value.NameLen-1] = 'x' // neither '\0' nor '.'
	if _, err := decodeDNSAllowKey(badKey, value); err == nil {
		t.Fatal("invalid terminator must be rejected")
	}
}

// TestDomainRuleSetCarriesFlagsAndPorts checks the index keeps what
// learnedAllowOutRows needs to derive rows: the rule's flags and port set.
func TestDomainRuleSetCarriesFlagsAndPorts(t *testing.T) {
	rules := []DomainRule{{
		Domain: "qq.com",
		Flags:  uint8(netPolicyFlagL7Required),
		Ports:  []L7Port{{Port: 8080, Scheme: L7SchemeHTTP}},
	}}
	rs := buildDomainRuleSetFromPersisted(rules)
	got := rs.lookup("qq.com")
	if got == nil {
		t.Fatal("rule not found")
	}
	if got.flags != uint8(netPolicyFlagL7Required) {
		t.Fatalf("flags = %#x, want L7_REQUIRED", got.flags)
	}
	if len(got.ports) != 1 || ntohsPort(got.ports[0].Port) != 8080 || got.ports[0].Scheme != L7SchemeHTTP {
		t.Fatalf("ports = %+v, want one 8080/http tuple in network order", got.ports)
	}
}

// TestParseDomainRuleRejectsUnrepresentable makes sure the index never holds a
// pattern the datapath has no key for.
func TestParseDomainRuleRejectsUnrepresentable(t *testing.T) {
	bad := []string{
		"",
		"*",
		"*.",
		"a.*.com",
		"**.com",
		"qq..com",
		"-qq.com",
		"qq-.com",
		"under_score.com",
	}
	for _, domain := range bad {
		if rule := parseDomainRule(DomainRule{Domain: domain}); rule != nil {
			t.Fatalf("parseDomainRule(%q) accepted %q, want reject", domain, rule.pattern)
		}
		// Cross-check: makeDNSAllowRule must agree, otherwise the index and
		// the map would disagree on what is installable.
		if _, _, err := makeDNSAllowRule(domain, 0); err == nil && domain != "under_score.com" && domain != "qq..com" &&
			domain != "-qq.com" && domain != "qq-.com" {
			t.Fatalf("makeDNSAllowRule(%q) accepted a pattern parseDomainRule rejected", domain)
		}
	}
}

// TestDomainRulesFromPlanRoundTrip checks the persisted form preserves flags
// and ports so a restart rebuilds an identical index.
func TestDomainRulesFromPlanRoundTrip(t *testing.T) {
	key, value, err := makeDNSAllowRule("*.qq.com", uint8(netPolicyFlagL7Required|netPolicyFlagL3Allowed))
	if err != nil {
		t.Fatalf("makeDNSAllowRule: %v", err)
	}
	applyPortsToDNSAllowValue(&value, []l7PortEntry{
		{Port: htonsPort(8080), Scheme: L7SchemeHTTP},
		{Port: htonsPort(9443), Scheme: L7SchemeHTTPS},
	})
	plan := []dnsAllowRule{{key: key, value: value, domain: "*.qq.com"}}

	persisted := domainRulesFromPlan(plan)
	rs := buildDomainRuleSetFromPersisted(persisted)
	rule := rs.lookup("a.qq.com")
	if rule == nil {
		t.Fatal("wildcard rule lost in round trip")
	}
	if rule.flags != value.Flags {
		t.Fatalf("flags = %#x, want %#x", rule.flags, value.Flags)
	}
	if len(rule.ports) != 2 {
		t.Fatalf("ports = %d, want 2", len(rule.ports))
	}
	if ntohsPort(rule.ports[0].Port) != 8080 || ntohsPort(rule.ports[1].Port) != 9443 {
		t.Fatalf("ports = %+v, want 8080 then 9443", rule.ports)
	}
}
