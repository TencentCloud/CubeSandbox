package runtime

import (
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
)

func TestCubeVSTapRegistrationBuildsL3L4AndL7Options(t *testing.T) {
	allowInternet := false
	sni := "API.Example.COM."
	host := "10.1.2.3:443"
	cfg := &CubeNetworkConfig{
		AllowInternetAccess: &allowInternet,
		AllowOut:            []string{"1.1.1.1/32"},
		DenyOut:             []string{"2.2.2.2/32"},
		Rules: []*EgressRule{{
			Name:  "allow-api",
			Match: &EgressRuleMatch{SNI: &sni},
		}, {
			Name:  "allow-host",
			Match: &EgressRuleMatch{Host: &host},
		}},
	}

	opts, err := cubeVSTapRegistration(cfg)
	if err != nil {
		t.Fatalf("cubeVSTapRegistration error=%v", err)
	}
	if opts.AllowInternetAccess == nil || *opts.AllowInternetAccess != false {
		t.Fatalf("AllowInternetAccess = %#v, want false", opts.AllowInternetAccess)
	}
	if opts.AllowOut == nil || len(*opts.AllowOut) != 1 || (*opts.AllowOut)[0] != "1.1.1.1/32" {
		t.Fatalf("AllowOut = %#v", opts.AllowOut)
	}
	if opts.DenyOut == nil || len(*opts.DenyOut) != 1 || (*opts.DenyOut)[0] != "2.2.2.2/32" {
		t.Fatalf("DenyOut = %#v", opts.DenyOut)
	}
	if opts.L7AllowOut == nil || len(*opts.L7AllowOut) != 2 {
		t.Fatalf("L7AllowOut = %#v", opts.L7AllowOut)
	}
	got := *opts.L7AllowOut
	if got[0].Host != "api.example.com" || got[1].Host != "10.1.2.3" {
		t.Fatalf("L7AllowOut = %#v", got)
	}
	// No explicit port/scheme on the rules: legacy default targets.
	for i, tgt := range got {
		if tgt.Port != 0 || tgt.Scheme != cubevs.L7SchemeNone {
			t.Fatalf("L7AllowOut[%d] = {Port:%d Scheme:%d}, want legacy default {0/L7SchemeNone}",
				i, tgt.Port, tgt.Scheme)
		}
	}
}

func TestCubeVSTapRegistrationBlockAll(t *testing.T) {
	allowInternet := false
	opts, err := cubeVSTapRegistration(&CubeNetworkConfig{
		AllowInternetAccess: &allowInternet,
	})
	if err != nil {
		t.Fatalf("cubeVSTapRegistration error=%v", err)
	}
	if opts.AllowInternetAccess == nil || *opts.AllowInternetAccess != false {
		t.Fatalf("opts.AllowInternetAccess=%v, want false", opts.AllowInternetAccess)
	}
}

func TestCubeVSTapRegistrationExtractsL7AllowOut(t *testing.T) {
	sni := "API.Example.COM."
	sniWildcard := "*.SNI.Example.COM"
	hostIP := "1.2.3.4:443"
	hostCIDR := "10.1.2.3/8"
	hostDomain := "Gateway.Example.COM:8443"
	hostWildcard := "*.Gateway.Example.COM"
	duplicateHost := "gateway.example.com"
	invalidHost := "999.999.999.999"
	opts, err := cubeVSTapRegistration(&CubeNetworkConfig{
		AllowOut: []string{"8.8.8.8"},
		Rules: []*EgressRule{
			{Match: &EgressRuleMatch{SNI: &sni, Host: &hostIP}},
			{Match: &EgressRuleMatch{Host: &hostCIDR}},
			{Match: &EgressRuleMatch{Host: &hostDomain}},
			{Match: &EgressRuleMatch{SNI: &sni}},
			{Match: &EgressRuleMatch{SNI: &sniWildcard}},
			{Match: &EgressRuleMatch{Host: &hostWildcard}},
			{Match: &EgressRuleMatch{Host: &duplicateHost}},
			{Match: &EgressRuleMatch{Host: &invalidHost}},
			{Match: &EgressRuleMatch{Path: stringPtr("/v1/chat")}},
		},
	})
	if err != nil {
		t.Fatalf("cubeVSTapRegistration error=%v", err)
	}
	if opts.AllowOut == nil || len(*opts.AllowOut) != 1 || (*opts.AllowOut)[0] != "8.8.8.8" {
		t.Fatalf("opts.AllowOut=%v, want [8.8.8.8]", opts.AllowOut)
	}
	if opts.L7AllowOut == nil {
		t.Fatal("opts.L7AllowOut=nil, want extracted targets")
	}
	// Rules without explicit Port + Scheme land as legacy default targets
	// (Port=0, Scheme=L7SchemeNone) — downstream buildL7Plan expands them
	// to {80/http, 443/https}. Assert host projection only; port/scheme
	// coverage is in TestExtractL7PortScheme.
	// The first rule carries both SNI and Host: BOTH are projected (SNI
	// first, then Host), so "api.example.com" (from SNI) and "1.2.3.4" (from
	// Host) both appear; the later duplicate SNI is deduplicated.
	wantHosts := []string{
		"api.example.com", "1.2.3.4", "10.0.0.0/8", "gateway.example.com",
		"*.sni.example.com", "*.gateway.example.com",
	}
	gotTargets := *opts.L7AllowOut
	if len(gotTargets) != len(wantHosts) {
		t.Fatalf("opts.L7AllowOut host count=%d, want %d (%+v)",
			len(gotTargets), len(wantHosts), gotTargets)
	}
	for i, w := range wantHosts {
		if gotTargets[i].Host != w {
			t.Fatalf("opts.L7AllowOut[%d].Host=%q, want %q", i, gotTargets[i].Host, w)
		}
		if gotTargets[i].Port != 0 || gotTargets[i].Scheme != cubevs.L7SchemeNone {
			t.Fatalf("opts.L7AllowOut[%d] expected legacy default (0/L7SchemeNone), got Port=%d Scheme=%d",
				i, gotTargets[i].Port, gotTargets[i].Scheme)
		}
	}
}

// TestExtractL7PortScheme covers the four accepted rule shapes plus the
// error paths. Rules missing one of {Port, Scheme}, using an unknown scheme
// string, or an out-of-range port must be dropped rather than silently
// promoted to the legacy default set — that would let a typo like "htps" fall
// back to port 443 instead of surfacing.
func TestExtractL7PortScheme(t *testing.T) {
	intPtr := func(i int) *int { return &i }

	tests := []struct {
		name       string
		match      EgressRuleMatch
		wantPort   uint16
		wantScheme uint8
		wantErr    bool
	}{
		{"both nil (legacy)", EgressRuleMatch{}, 0, cubevs.L7SchemeNone, false},
		{"port only rejected",
			EgressRuleMatch{Port: intPtr(8080)}, 0, 0, true},
		{"http alone fills default port 80",
			EgressRuleMatch{Scheme: stringPtr("http")}, 80, cubevs.L7SchemeHTTP, false},
		{"https alone fills default port 443",
			EgressRuleMatch{Scheme: stringPtr("https")}, 443, cubevs.L7SchemeHTTPS, false},
		{"https mixed case alone fills 443",
			EgressRuleMatch{Scheme: stringPtr("HTTPS")}, 443, cubevs.L7SchemeHTTPS, false},
		{"http lowercase",
			EgressRuleMatch{Port: intPtr(8080), Scheme: stringPtr("http")},
			8080, cubevs.L7SchemeHTTP, false},
		{"https mixed case",
			EgressRuleMatch{Port: intPtr(8443), Scheme: stringPtr("HTTPS")},
			8443, cubevs.L7SchemeHTTPS, false},
		{"scheme with whitespace",
			EgressRuleMatch{Port: intPtr(80), Scheme: stringPtr("  http  ")},
			80, cubevs.L7SchemeHTTP, false},
		{"unknown scheme rejected",
			EgressRuleMatch{Port: intPtr(80), Scheme: stringPtr("gopher")},
			0, 0, true},
		{"unknown scheme alone rejected",
			EgressRuleMatch{Scheme: stringPtr("gopher")}, 0, 0, true},
		{"port too low rejected",
			EgressRuleMatch{Port: intPtr(0), Scheme: stringPtr("http")},
			0, 0, true},
		{"port too high rejected",
			EgressRuleMatch{Port: intPtr(65536), Scheme: stringPtr("http")},
			0, 0, true},
		{"port negative rejected",
			EgressRuleMatch{Port: intPtr(-1), Scheme: stringPtr("http")},
			0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port, scheme, err := extractL7PortScheme(&tt.match)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if port != tt.wantPort {
				t.Fatalf("port=%d, want %d", port, tt.wantPort)
			}
			if scheme != tt.wantScheme {
				t.Fatalf("scheme=%d, want %d", scheme, tt.wantScheme)
			}
		})
	}
}

func TestCubeVSTapRegistrationRejectsEntireInvalidL7Policy(t *testing.T) {
	host := "api.example.com"
	https := "https"
	validPort := 8443
	invalidPort := 8080

	opts, err := cubeVSTapRegistration(&CubeNetworkConfig{
		AllowOut: []string{"8.8.8.8"},
		Rules: []*EgressRule{
			{
				Name: "valid-rule",
				Match: &EgressRuleMatch{
					Host: &host, Port: &validPort, Scheme: &https,
				},
			},
			{
				Name:  "invalid-rule",
				Match: &EgressRuleMatch{Host: &host, Port: &invalidPort},
			},
		},
	})
	if err == nil {
		t.Fatal("cubeVSTapRegistration error=nil, want invalid policy error")
	}
	if !strings.Contains(err.Error(), `network.rules[1] "invalid-rule"`) ||
		!strings.Contains(err.Error(), "port requires scheme") {
		t.Fatalf("error=%q, want rule location and reason", err)
	}
	if opts.AllowOut != nil || opts.L7AllowOut != nil || opts.DenyOut != nil {
		t.Fatalf("opts=%+v, want empty options on whole-policy rejection", opts)
	}
}

func TestToEgressInputTranslatesL7Rules(t *testing.T) {
	host := "api.example.com"
	method := []string{"GET", "POST"}
	audit := "security"
	format := "bearer"
	cfg := &CubeNetworkConfig{Rules: []*EgressRule{{
		Name: "inject-token",
		Match: &EgressRuleMatch{
			Host:   &host,
			Method: method,
			Path:   stringPtr("/v1"),
		},
		Action: &EgressRuleAction{
			Allow: true,
			Audit: &audit,
			Inject: []*EgressRuleInject{{
				Header: "Authorization",
				Secret: "secret-key",
				Format: &format,
			}},
		},
	}}}

	input := toEgressInput(cfg)
	if input == nil || len(input.Rules) != 1 {
		t.Fatalf("input = %#v", input)
	}
	rule := input.Rules[0]
	if rule.Name != "inject-token" || rule.Match == nil || rule.Match.Host == nil || *rule.Match.Host != host {
		t.Fatalf("rule match = %#v", rule)
	}
	if len(rule.Match.Method) != 2 || rule.Match.Method[1] != "POST" {
		t.Fatalf("methods = %#v", rule.Match.Method)
	}
	if rule.Action == nil || !rule.Action.Allow || rule.Action.Audit == nil || *rule.Action.Audit != audit {
		t.Fatalf("action = %#v", rule.Action)
	}
	if len(rule.Action.Inject) != 1 || rule.Action.Inject[0].Header != "Authorization" {
		t.Fatalf("inject = %#v", rule.Action.Inject)
	}
}

func stringPtr(value string) *string {
	return &value
}

// TestCloneEgressRulesDeepCopiesMatchPointers guards against the shallow-copy
// regression: cloneEgressRules must not alias the caller's match pointers, so
// mutating the request after the clone must not leak into the stored copy.
func TestCloneEgressRulesDeepCopiesMatchPointers(t *testing.T) {
	port := 8443
	original := &EgressRule{
		Name: "rule",
		Match: &EgressRuleMatch{
			SNI:    stringPtr("api.example.com"),
			Host:   stringPtr("api.example.com"),
			Path:   stringPtr("/v1/chat"),
			Scheme: stringPtr("https"),
			Port:   &port,
			Method: []string{"GET"},
		},
		Action: &EgressRuleAction{Allow: true},
	}

	cloned := cloneEgressRules([]*EgressRule{original})
	if len(cloned) != 1 || cloned[0].Match == nil {
		t.Fatalf("cloneEgressRules returned %#v", cloned)
	}

	// Mutate every pointer field on the original; the clone must be unaffected.
	*original.Match.SNI = "mutated.example.com"
	*original.Match.Host = "mutated.example.com"
	*original.Match.Path = "/mutated"
	*original.Match.Scheme = "http"
	*original.Match.Port = 9999

	cm := cloned[0].Match
	if *cm.SNI != "api.example.com" || *cm.Host != "api.example.com" ||
		*cm.Path != "/v1/chat" || *cm.Scheme != "https" || *cm.Port != 8443 {
		t.Fatalf("clone aliased caller pointers: SNI=%q Host=%q Path=%q Scheme=%q Port=%d",
			*cm.SNI, *cm.Host, *cm.Path, *cm.Scheme, *cm.Port)
	}
}
