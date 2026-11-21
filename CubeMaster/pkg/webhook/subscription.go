// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"strings"
	"unicode"
)

// Subscription is a static transport-independent target and event filter.
type Subscription struct {
	ID      string
	Enabled bool
	Target  string
	Events  []string
}
type SubscriptionProvider interface{ List() []Subscription }
type StaticSubscriptionProvider struct{ subscriptions []Subscription }

func NewStaticSubscriptionProvider(subscriptions []Subscription) (*StaticSubscriptionProvider, error) {
	if len(subscriptions) > 64 {
		return nil, ErrInvalidDelivery
	}
	seen := make(map[string]bool)
	for _, s := range subscriptions {
		if !validSubscriptionText(s.ID, MaxSubscriptionIDBytes) || !validSubscriptionText(s.Target, MaxDeliveryTargetBytes) || seen[s.ID] || len(s.Events) == 0 || len(s.Events) > MaxEventsPerSubscription {
			return nil, ErrInvalidDelivery
		}
		seen[s.ID] = true
		events := make(map[string]bool)
		for _, e := range s.Events {
			if !validType(e) || events[e] {
				return nil, ErrInvalidDelivery
			}
			events[e] = true
		}
	}
	return &StaticSubscriptionProvider{subscriptions: cloneSubscriptions(subscriptions)}, nil
}
func validSubscriptionText(s string, max int) bool {
	return strings.TrimSpace(s) != "" && validText(s, max) && strings.IndexFunc(s, unicode.IsControl) < 0
}
func cloneSubscriptions(in []Subscription) []Subscription {
	out := append([]Subscription(nil), in...)
	for i := range out {
		out[i].Events = append([]string(nil), out[i].Events...)
	}
	return out
}
func (p *StaticSubscriptionProvider) List() []Subscription {
	return cloneSubscriptions(p.subscriptions)
}
func matches(s Subscription, kind string) bool {
	if s.Enabled {
		for _, e := range s.Events {
			if e == kind {
				return true
			}
		}
	}
	return false
}
