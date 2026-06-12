package cubevs

import (
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestBusinessMapNamesReturnsCopy(t *testing.T) {
	names := BusinessMapNames()
	if len(names) == 0 {
		t.Fatal("BusinessMapNames returned empty list")
	}

	names[0] = "mutated"
	if got := BusinessMapNames()[0]; got == "mutated" {
		t.Fatal("BusinessMapNames returned shared backing array")
	}
}

func TestNormalizeBusinessMapNames(t *testing.T) {
	got, err := normalizeBusinessMapNames([]string{
		MapNameDNSAllow,
		MapNameDNSAllow,
		MapNameAllowOutV2,
	})
	if err != nil {
		t.Fatalf("normalizeBusinessMapNames returned error: %v", err)
	}

	want := []string{MapNameDNSAllow, MapNameAllowOutV2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeBusinessMapNames()=%v, want %v", got, want)
	}
}

func TestNormalizeBusinessMapNamesAll(t *testing.T) {
	got, err := normalizeBusinessMapNames([]string{"all"})
	if err != nil {
		t.Fatalf("normalizeBusinessMapNames returned error: %v", err)
	}
	if !reflect.DeepEqual(got, BusinessMapNames()) {
		t.Fatalf("normalizeBusinessMapNames(all)=%v, want all business maps", got)
	}
}

func TestDumpDNSAllowRuleExact(t *testing.T) {
	key, value, err := makeDNSAllowRule("api.example.com", uint8(netPolicyFlagL7Required))
	if err != nil {
		t.Fatalf("makeDNSAllowRule returned error: %v", err)
	}

	rule, err := dumpDNSAllowRule(key, value)
	if err != nil {
		t.Fatalf("dumpDNSAllowRule returned error: %v", err)
	}
	if rule.Domain != "api.example.com" {
		t.Fatalf("Domain=%q, want api.example.com", rule.Domain)
	}
	if rule.Wildcard {
		t.Fatal("Wildcard=true, want false")
	}
	if !rule.L7Required {
		t.Fatal("L7Required=false, want true")
	}
}

func TestDumpDNSAllowRuleWildcard(t *testing.T) {
	key, value, err := makeDNSAllowRule("*.example.com", 0)
	if err != nil {
		t.Fatalf("makeDNSAllowRule returned error: %v", err)
	}

	rule, err := dumpDNSAllowRule(key, value)
	if err != nil {
		t.Fatalf("dumpDNSAllowRule returned error: %v", err)
	}
	if rule.Domain != "*.example.com" {
		t.Fatalf("Domain=%q, want *.example.com", rule.Domain)
	}
	if !rule.Wildcard {
		t.Fatal("Wildcard=false, want true")
	}
}

func TestApplyDNSPolicyModeDump(t *testing.T) {
	tests := []struct {
		name         string
		flags        uint8
		wantEnabled  bool
		wantLearning bool
	}{
		{
			name: "disabled",
		},
		{
			name:         "track only",
			flags:        dnsPolicyFlagLearningEnabled,
			wantEnabled:  true,
			wantLearning: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result DNSAllowMapDump
			applyDNSPolicyModeDump(&result, tt.flags)

			if result.Enabled != tt.wantEnabled {
				t.Fatalf("Enabled=%v, want %v", result.Enabled, tt.wantEnabled)
			}
			if result.LearningEnabled != tt.wantLearning {
				t.Fatalf("LearningEnabled=%v, want %v", result.LearningEnabled, tt.wantLearning)
			}
			if result.Flags != tt.flags {
				t.Fatalf("Flags=%d, want %d", result.Flags, tt.flags)
			}
		})
	}
}

func TestDumpSessionKey(t *testing.T) {
	key := sessionKey{
		SourceIP:   ipToUint32([]byte{10, 0, 0, 2}),
		TargetIP:   ipToUint32([]byte{8, 8, 8, 8}),
		SourcePort: htons(12345),
		TargetPort: htons(443),
		Version:    7,
		Protocol:   unix.IPPROTO_TCP,
	}

	got := dumpSessionKey(key)
	if got.SourceIP != "10.0.0.2" || got.TargetIP != "8.8.8.8" {
		t.Fatalf("dumpSessionKey IPs=%s->%s, want 10.0.0.2->8.8.8.8", got.SourceIP, got.TargetIP)
	}
	if got.SourcePort != 12345 || got.TargetPort != 443 {
		t.Fatalf("dumpSessionKey ports=%d->%d, want 12345->443", got.SourcePort, got.TargetPort)
	}
	if got.ProtocolName != "tcp" {
		t.Fatalf("ProtocolName=%q, want tcp", got.ProtocolName)
	}
}
