// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"strings"
	"testing"
)

func TestParseEndpoints_Valid(t *testing.T) {
	raw := `[
		{"url": "https://ops.example.com/hook", "events": ["sandbox.created"], "secret": "0123456789abcdef"},
		{"url": "http://127.0.0.1:9010/webhooks/cube"}
	]`
	endpoints, err := ParseEndpoints(raw)
	if err != nil {
		t.Fatalf("ParseEndpoints: %v", err)
	}
	if len(endpoints) != 2 {
		t.Fatalf("got %d endpoints, want 2", len(endpoints))
	}
	if endpoints[0].Secret != "0123456789abcdef" {
		t.Fatalf("secret not parsed: %q", endpoints[0].Secret)
	}
	if endpoints[1].Events != nil {
		t.Fatalf("events must default to nil (wildcard): %v", endpoints[1].Events)
	}
}

func TestParseEndpoints_EmptyDisablesFeature(t *testing.T) {
	for _, raw := range []string{"", "   "} {
		endpoints, err := ParseEndpoints(raw)
		if err != nil || endpoints != nil {
			t.Fatalf("raw %q: got (%v, %v), want (nil, nil)", raw, endpoints, err)
		}
	}
}

func TestParseEndpoints_RejectsBadEntries(t *testing.T) {
	cases := map[string]string{
		"not json":       `not-json`,
		"bad scheme":     `[{"url": "ftp://example.com/hook"}]`,
		"missing host":   `[{"url": "https:///path"}]`,
		"userinfo":       `[{"url": "https://user:pw@example.com/hook"}]`,
		"short secret":   `[{"url": "https://example.com/hook", "secret": "short"}]`,
		"trailing entry": `[{"url": "https://ok.example.com"}, {"url": "::bad::"}]`,
		// A misspelled event name would silently never match; reject it.
		"unknown event": `[{"url": "https://example.com/hook", "events": ["sandbox.creaetd"]}]`,
	}
	for name, raw := range cases {
		if _, err := ParseEndpoints(raw); err == nil {
			t.Fatalf("%s: expected error, got nil", name)
		}
	}
	// A failure anywhere must not yield a partial list.
	if endpoints, _ := ParseEndpoints(cases["trailing entry"]); endpoints != nil {
		t.Fatal("partial endpoint list returned on error")
	}
}

func TestParseEndpoints_AcceptsAllKnownEvents(t *testing.T) {
	raw := `[{"url": "https://example.com/hook", "events": [
		"sandbox.created", "sandbox.deleted", "sandbox.paused", "sandbox.resumed"
	]}]`
	if _, err := ParseEndpoints(raw); err != nil {
		t.Fatalf("all known events must parse: %v", err)
	}
}

func TestEndpointSubscribes(t *testing.T) {
	selected := Endpoint{URL: "http://x", Events: []string{EventCreated}}
	if !selected.Subscribes(EventCreated) || selected.Subscribes(EventDeleted) {
		t.Fatal("filtered subscription mismatch")
	}
	wildcard := Endpoint{URL: "http://x"}
	if !wildcard.Subscribes(EventDeleted) {
		t.Fatal("empty events must subscribe to everything")
	}
}

func TestEndpointGroup(t *testing.T) {
	a := Endpoint{URL: "http://x/hook", Events: []string{EventCreated, EventDeleted}}
	// Filter order must not change the group.
	b := Endpoint{URL: "http://x/hook", Events: []string{EventDeleted, EventCreated}}
	if a.Group() != b.Group() {
		t.Fatalf("group must be filter-order independent: %q vs %q", a.Group(), b.Group())
	}
	if !strings.HasPrefix(a.Group(), "webhook:") {
		t.Fatalf("group name must be namespaced: %q", a.Group())
	}
	// A different filter (or URL) must yield a different group, otherwise the
	// stream would be split between subscribers with different filters.
	c := Endpoint{URL: "http://x/hook", Events: []string{EventCreated}}
	if a.Group() == c.Group() {
		t.Fatal("different filters must not share a consumer group")
	}
	d := Endpoint{URL: "http://y/hook", Events: []string{EventCreated, EventDeleted}}
	if a.Group() == d.Group() {
		t.Fatal("different URLs must not share a consumer group")
	}
	// The secret never enters the group name: it must not leak into Redis
	// keyspace metadata, and re-keying must not re-read history.
	e := a
	e.Secret = "0123456789abcdef"
	if a.Group() != e.Group() {
		t.Fatal("secret must not affect the consumer group")
	}
}

func TestEndpointLabel(t *testing.T) {
	ep := Endpoint{URL: "https://user:secret@example.com/hook?token=secret#fragment"}
	if got := ep.Label(); got != "https://example.com/hook" {
		t.Fatalf("label = %q", got)
	}
	bad := Endpoint{URL: "::not-a-url::"}
	if got := bad.Label(); got != "<invalid webhook URL>" {
		t.Fatalf("bad label = %q", got)
	}
}

func TestSign(t *testing.T) {
	// Same vector as the CubeAPI-side implementation: wire compatibility.
	got := sign("secret", []byte("payload"))
	want := "sha256=b82fcb791acec57859b989b430a826488ce2e479fdf92326bd0a2e8375a42ba4"
	if got != want {
		t.Fatalf("sign = %q, want %q", got, want)
	}
}
