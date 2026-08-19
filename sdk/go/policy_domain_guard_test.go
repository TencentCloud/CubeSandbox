// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import "testing"

func TestAllowOutDomainGuardFiresForUnicodeDomains(t *testing.T) {
	domains := []string{
		"example.com",
		"exаmple.com",
		"bücher.example",
		"例子.com",
		"café.example.com",
		"xn--bcher-kva.example",
		"*.café.example.com",
	}

	for _, d := range domains {
		t.Run(d, func(t *testing.T) {
			if !isDomainAllowOutTarget(d) {
				t.Fatalf("isDomainAllowOutTarget(%q) = false, want true", d)
			}
			if err := validateAllowOutDomainsRequireDenyAll([]string{d}, nil, false); err == nil {
				t.Fatalf("guard did not fire for %q with public egress enabled", d)
			}
			if err := validateAllowOutDomainsRequireDenyAll([]string{d}, []string{"0.0.0.0/0"}, false); err != nil {
				t.Fatalf("guard fired for %q despite deny-all: %v", d, err)
			}
			if err := validateAllowOutDomainsRequireDenyAll([]string{d}, nil, true); err != nil {
				t.Fatalf("guard fired for %q despite defaultDenyAll: %v", d, err)
			}
		})
	}
}

func TestNonDomainTargetsStillNotTreatedAsDomains(t *testing.T) {
	nonDomains := []string{
		"10.0.0.1",
		"10.0.0.0/8",
		"::1",
		"999.1.2.3",
		"1.2.3.4",
		"",
		"has space",
		"under_score.example.com",
		"double**star.example.com",
	}

	for _, target := range nonDomains {
		if isDomainAllowOutTarget(target) {
			t.Errorf("isDomainAllowOutTarget(%q) = true, want false", target)
		}
	}
}
