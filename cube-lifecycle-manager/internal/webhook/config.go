// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"fmt"
	"net/url"
)

// KnownEvents is the set of event types the CLM can emit. Used for validation
// and as the default subscription when no filter is configured.
var KnownEvents = []EventType{EventCreated, EventDeleted, EventPaused, EventResumed, EventUpdated}

// Endpoint is one webhook target. The initial list is derived from
// CUBE_LCM_WEBHOOK_URLS; additional endpoints can be managed at runtime via
// the /admin/webhooks REST API.
type Endpoint struct {
	ID      string   `json:"id"`
	URL     string   `json:"url"`
	Events  []string `json:"events,omitempty"` // empty = inherit the global filter (all)
	Secret  string   `json:"secret,omitempty"` // empty = unsigned (inherits nothing)
	Enabled bool     `json:"enabled"`
}

// FromEnv derives one endpoint per URL. Every endpoint inherits the global
// event filter and the shared secret; per-endpoint overrides come from the
// REST API.
func FromEnv(urls, events []string, secret string) []Endpoint {
	eps := make([]Endpoint, 0, len(urls))
	for i, u := range urls {
		eps = append(eps, Endpoint{
			ID:      fmt.Sprintf("env-%d", i),
			URL:     u,
			Enabled: true,
			// Events deliberately empty so the manager's global filter
			// (events) is the sole filter for the env-derived path.
			Secret: secret,
		})
	}
	return eps
}

// Matches reports whether the endpoint subscribes to the given event type.
// An empty subscription list matches everything (the global filter applies).
func (e Endpoint) Matches(eventType string) bool {
	if len(e.Events) == 0 {
		return true
	}
	for _, ev := range e.Events {
		if ev == "*" || ev == eventType {
			return true
		}
	}
	return false
}

// ValidateEndpoint checks URL shape and event subscription names.
func ValidateEndpoint(ep Endpoint) error {
	u, err := url.Parse(ep.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("webhook endpoint url %q must be an absolute http(s) URL", ep.URL)
	}
	for _, ev := range ep.Events {
		if ev == "*" {
			continue
		}
		if !isKnownEvent(ev) {
			return fmt.Errorf("webhook endpoint %q subscribes to unknown event %q", ep.ID, ev)
		}
	}
	return nil
}

func isKnownEvent(ev string) bool {
	for _, k := range KnownEvents {
		if ev == string(k) {
			return true
		}
	}
	return false
}
