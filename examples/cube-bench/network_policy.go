package main

import (
	"fmt"
	"strings"
)

const (
	networkPolicyNone  = "none"
	networkPolicyRules = "rules"
)

// networkConfigFingerprint summarizes a built-in policy for reports / UI.
type networkConfigFingerprint struct {
	Policy      string `json:"policy"`
	AllowOut    int    `json:"allow_out"`
	Rules       int    `json:"rules"`
	InjectRules int    `json:"inject_rules"`
}

type egressRuleInject struct {
	Header string `json:"header"`
	Secret string `json:"secret"`
	Format string `json:"format,omitempty"`
}

type egressRuleMatch struct {
	SNI    string   `json:"sni,omitempty"`
	Host   string   `json:"host,omitempty"`
	Method []string `json:"method,omitempty"`
	Path   string   `json:"path,omitempty"`
	Scheme string   `json:"scheme,omitempty"`
}

type egressRuleAction struct {
	Allow  bool               `json:"allow"`
	Audit  string             `json:"audit,omitempty"`
	Inject []egressRuleInject `json:"inject,omitempty"`
}

type egressRule struct {
	Name   string           `json:"name"`
	Match  egressRuleMatch  `json:"match"`
	Action egressRuleAction `json:"action"`
}

type sandboxNetworkConfig struct {
	AllowOut []string     `json:"allowOut,omitempty"`
	Rules    []egressRule `json:"rules,omitempty"`
}

func parseNetworkPolicy(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", networkPolicyNone:
		return networkPolicyNone, nil
	case networkPolicyRules:
		return networkPolicyRules, nil
	default:
		return "", fmt.Errorf("--network-policy must be %q or %q, got %q",
			networkPolicyNone, networkPolicyRules, raw)
	}
}

func rulesAllowOut() []string {
	// 12 CIDRs + 12 domains (incl. 2 wildcards) — medium allowlist for create-path
	// CubeVS allow_out_v2 / dns_allow map updates. Hosts are stable fakes; the
	// bench does not require them to resolve or be reachable.
	return []string{
		"1.1.1.1/32",
		"1.0.0.1/32",
		"8.8.8.8/32",
		"8.8.4.4/32",
		"9.9.9.9/32",
		"149.112.112.112/32",
		"208.67.222.222/32",
		"208.67.220.220/32",
		"94.140.14.14/32",
		"94.140.15.15/32",
		"76.76.2.0/24",
		"76.76.10.0/24",
		"dns.bench.cubesandbox.test",
		"registry.bench.cubesandbox.test",
		"cdn.bench.cubesandbox.test",
		"npm.bench.cubesandbox.test",
		"pypi.bench.cubesandbox.test",
		"github.bench.cubesandbox.test",
		"objects.bench.cubesandbox.test",
		"telemetry.bench.cubesandbox.test",
		"docs.bench.cubesandbox.test",
		"status.bench.cubesandbox.test",
		"*.assets.bench.cubesandbox.test",
		"*.cdn.bench.cubesandbox.test",
	}
}

func rulesL7Rules() []egressRule {
	// 6 L7 rules; 2 carry inject so CubeEgress policy PUT is non-trivial.
	return []egressRule{
		{
			Name: "allow_llm_chat",
			Match: egressRuleMatch{
				Scheme: "https",
				SNI:    "api.bench.cubesandbox.test",
				Host:   "api.bench.cubesandbox.test",
				Method: []string{"POST"},
				Path:   "/v1/chat/completions",
			},
			Action: egressRuleAction{
				Allow: true,
				Audit: "metadata",
				Inject: []egressRuleInject{{
					Header: "Authorization",
					Secret: "cube-bench-dummy-llm-key",
					Format: "Bearer ${SECRET}",
				}},
			},
		},
		{
			Name: "allow_llm_models",
			Match: egressRuleMatch{
				Scheme: "https",
				SNI:    "api.bench.cubesandbox.test",
				Host:   "api.bench.cubesandbox.test",
				Method: []string{"GET"},
				Path:   "/v1/models",
			},
			Action: egressRuleAction{
				Allow: true,
				Audit: "metadata",
			},
		},
		{
			Name: "allow_embeddings",
			Match: egressRuleMatch{
				Scheme: "https",
				SNI:    "embed.bench.cubesandbox.test",
				Host:   "embed.bench.cubesandbox.test",
				Method: []string{"POST"},
				Path:   "/v1/embeddings",
			},
			Action: egressRuleAction{
				Allow: true,
				Audit: "metadata",
				Inject: []egressRuleInject{{
					Header: "Authorization",
					Secret: "cube-bench-dummy-embed-key",
					Format: "Bearer ${SECRET}",
				}},
			},
		},
		{
			Name: "allow_vector_upsert",
			Match: egressRuleMatch{
				Scheme: "https",
				SNI:    "vector.bench.cubesandbox.test",
				Host:   "vector.bench.cubesandbox.test",
				Method: []string{"POST", "PUT"},
				Path:   "/v1/indexes/*",
			},
			Action: egressRuleAction{
				Allow: true,
				Audit: "metadata",
			},
		},
		{
			Name: "allow_webhook_callback",
			Match: egressRuleMatch{
				Scheme: "https",
				SNI:    "hooks.bench.cubesandbox.test",
				Host:   "hooks.bench.cubesandbox.test",
				Method: []string{"POST"},
				Path:   "/callbacks/*",
			},
			Action: egressRuleAction{
				Allow: true,
				Audit: "none",
			},
		},
		{
			Name: "deny_metadata_probe",
			Match: egressRuleMatch{
				Scheme: "http",
				Host:   "169.254.169.254",
				Path:   "/*",
			},
			Action: egressRuleAction{
				Allow: false,
				Audit: "full",
			},
		},
	}
}

func rulesNetworkConfig() sandboxNetworkConfig {
	return sandboxNetworkConfig{
		AllowOut: rulesAllowOut(),
		Rules:    rulesL7Rules(),
	}
}

func networkFingerprint(policy string) networkConfigFingerprint {
	fp := networkConfigFingerprint{Policy: policy}
	if policy != networkPolicyRules {
		return fp
	}
	cfg := rulesNetworkConfig()
	fp.AllowOut = len(cfg.AllowOut)
	fp.Rules = len(cfg.Rules)
	for _, r := range cfg.Rules {
		if len(r.Action.Inject) > 0 {
			fp.InjectRules++
		}
	}
	return fp
}

func (fp networkConfigFingerprint) summary() string {
	if fp.Policy == networkPolicyNone || fp.Policy == "" {
		return networkPolicyNone
	}
	return fmt.Sprintf("%s (allowOut=%d rules=%d injectRules=%d)",
		fp.Policy, fp.AllowOut, fp.Rules, fp.InjectRules)
}
