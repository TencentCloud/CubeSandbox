package runtime

import "testing"

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

	opts := cubeVSTapRegistration(cfg)
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
	if (*opts.L7AllowOut)[0] != "api.example.com" || (*opts.L7AllowOut)[1] != "10.1.2.3" {
		t.Fatalf("L7AllowOut = %#v", *opts.L7AllowOut)
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
