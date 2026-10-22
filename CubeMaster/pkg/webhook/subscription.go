// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"

type Subscription = config.WebhookSubscription
type SubscriptionProvider interface{ List() []Subscription }
type StaticConfigSubscriptionProvider struct{ subscriptions []Subscription }

func NewStaticConfigSubscriptionProvider(c config.WebhookConf) (*StaticConfigSubscriptionProvider, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &StaticConfigSubscriptionProvider{subscriptions: cloneSubscriptions(c.Subscriptions)}, nil
}
func cloneSubscriptions(in []Subscription) []Subscription {
	out := append([]Subscription(nil), in...)
	for i := range out {
		out[i].Events = append([]string(nil), out[i].Events...)
	}
	return out
}
func (p *StaticConfigSubscriptionProvider) List() []Subscription {
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
