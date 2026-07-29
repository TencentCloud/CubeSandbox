// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package webhook delivers sandbox lifecycle events (created / deleted /
// paused / resumed) to external, operator-configured HTTP endpoints.
//
// Each endpoint owns an independent Redis consumer group on the lifecycle
// events stream, so a slow or unreachable receiver never delays other
// subscriptions or the CLM's own auto-pause/resume loop. Delivery is
// at-least-once: an event is XACKed only after it reaches a terminal state
// (delivered, permanently rejected, or retries exhausted), so a crashed
// replica redelivers its pending events after restart.
package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Endpoint is one outbound Webhook subscription. It mirrors the endpoint
// shape used by the CubeAPI-side Webhook configuration so operators can move
// between the two without reformatting.
type Endpoint struct {
	// URL is the HTTP(S) receiver address. Userinfo credentials are
	// rejected: use Secret for authentication instead.
	URL string `json:"url"`
	// Events lists the subscribed event names (e.g. "sandbox.created").
	// Empty subscribes to every lifecycle event.
	Events []string `json:"events,omitempty"`
	// Secret, when non-empty, enables HMAC-SHA256 signing of the exact
	// request body in the X-Cube-Signature-256 header. Must be at least
	// 16 bytes; empty means unsigned.
	Secret string `json:"secret,omitempty"`
}

// ParseEndpoints decodes the JSON array carried by CUBE_LCM_WEBHOOK_ENDPOINTS
// and validates every entry. An empty input yields no endpoints (feature
// disabled); a malformed entry fails the whole parse so a typo never
// silently drops a subscription.
func ParseEndpoints(raw string) ([]Endpoint, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var endpoints []Endpoint
	if err := json.Unmarshal([]byte(raw), &endpoints); err != nil {
		return nil, fmt.Errorf("webhook endpoints: invalid JSON: %w", err)
	}
	for i, ep := range endpoints {
		if err := ep.validate(); err != nil {
			return nil, fmt.Errorf("webhook endpoints[%d]: %w", i, err)
		}
	}
	return endpoints, nil
}

func (e Endpoint) validate() error {
	u, err := url.Parse(e.URL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid URL %q", e.URL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL must use http or https: %q", e.URL)
	}
	if u.User != nil {
		return fmt.Errorf("URL must not contain userinfo credentials: %q", e.URL)
	}
	if e.Secret != "" && len(e.Secret) < 16 {
		return fmt.Errorf("secret must be at least 16 bytes")
	}
	return nil
}

// Subscribes reports whether the endpoint wants the named event. An empty
// Events list is the wildcard subscription.
func (e Endpoint) Subscribes(event string) bool {
	if len(e.Events) == 0 {
		return true
	}
	for _, item := range e.Events {
		if item == event {
			return true
		}
	}
	return false
}

// Group returns the endpoint's Redis consumer group name on the lifecycle
// events stream. The name is derived from the URL and the (sorted) event
// filter so that:
//   - every CLM replica derives the same group for the same subscription
//     (replicas share the group and split the work via consumer names);
//   - two subscriptions with different filters never share a group, which
//     would split the stream between them and break the filters;
//   - the group is disjoint from the CLM's own "cube-proxy-sidecar" group,
//     so webhook delivery cannot disturb auto-pause/resume.
func (e Endpoint) Group() string {
	h := sha256.New()
	h.Write([]byte(e.URL))
	events := append([]string(nil), e.Events...)
	sort.Strings(events)
	for _, ev := range events {
		h.Write([]byte{0})
		h.Write([]byte(ev))
	}
	return "webhook:" + hex.EncodeToString(h.Sum(nil))[:12]
}

// Label returns the endpoint URL sanitized for logs: credentials, query, and
// fragment are stripped so tokens inside the URL never reach log output.
func (e Endpoint) Label() string {
	u, err := url.Parse(e.URL)
	if err != nil {
		return "<invalid webhook URL>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
