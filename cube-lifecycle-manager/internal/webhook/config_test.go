// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import "testing"

func TestFromEnv_DerivesOneEndpointPerURL(t *testing.T) {
	eps := FromEnv(
		[]string{"http://host-a:8080/hook", "http://host-b:8080/hook"},
		[]string{"sandbox.paused", "sandbox.resumed"},
		"shared-secret",
	)
	if len(eps) != 2 {
		t.Fatalf("len = %d, want 2", len(eps))
	}
	for i, ep := range eps {
		if ep.ID != "env-"+string(rune('0'+i)) {
			t.Errorf("id = %q, want env-%d", ep.ID, i)
		}
		if !ep.Enabled {
			t.Errorf("endpoint %d not enabled", i)
		}
		if ep.Secret != "shared-secret" {
			t.Errorf("endpoint %d secret = %q", i, ep.Secret)
		}
		if len(ep.Events) != 0 {
			t.Errorf("endpoint %d Events should be empty (inherits global filter), got %v", i, ep.Events)
		}
	}
	if eps[0].URL != "http://host-a:8080/hook" || eps[1].URL != "http://host-b:8080/hook" {
		t.Errorf("urls not mapped in order: %+v", eps)
	}
}

func TestValidateEndpoint_RejectsBadConfig(t *testing.T) {
	bad := []Endpoint{
		{ID: "a", URL: "ftp://host/x"},                                       // wrong scheme
		{ID: "b", URL: "not a url"},                                          // unparseable
		{ID: "c", URL: "/relative/path"},                                     // no host
		{ID: "d", URL: "http://host/x", Events: []string{"sandbox.unknown"}}, // unknown event
	}
	for _, ep := range bad {
		if err := ValidateEndpoint(ep); err == nil {
			t.Errorf("ValidateEndpoint(%+v) = nil, want error", ep)
		}
	}
}

func TestValidateEndpoint_AcceptsGoodConfig(t *testing.T) {
	good := []Endpoint{
		{ID: "a", URL: "http://host/x", Enabled: true},
		{ID: "b", URL: "https://host:8443/x", Events: []string{"*"}},
		{ID: "c", URL: "http://host/x", Events: []string{"sandbox.created", "sandbox.deleted"}},
	}
	for _, ep := range good {
		if err := ValidateEndpoint(ep); err != nil {
			t.Errorf("ValidateEndpoint(%+v) = %v, want nil", ep, err)
		}
	}
}

func TestEndpointMatches(t *testing.T) {
	all := Endpoint{Events: nil}
	if !all.Matches("sandbox.created") || !all.Matches("sandbox.paused") {
		t.Error("empty subscription should match everything")
	}

	wild := Endpoint{Events: []string{"*"}}
	if !wild.Matches("sandbox.created") {
		t.Error("* should match everything")
	}

	specific := Endpoint{Events: []string{"sandbox.paused"}}
	if !specific.Matches("sandbox.paused") {
		t.Error("paused subscription should match sandbox.paused")
	}
	if specific.Matches("sandbox.created") {
		t.Error("paused subscription should not match sandbox.created")
	}
}
