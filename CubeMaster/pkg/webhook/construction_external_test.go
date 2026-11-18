// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
)

func TestRuntimeRejectsInvalidOptionsAndSubscriptionsBeforeOwnership(t *testing.T) {
	for name, change := range map[string]func(*webhook.RuntimeOptions){
		"ingress-zero":       func(o *webhook.RuntimeOptions) { o.IngressCapacity = 0 },
		"ingress-large":      func(o *webhook.RuntimeOptions) { o.IngressCapacity = 4097 },
		"workers-zero":       func(o *webhook.RuntimeOptions) { o.Workers = 0 },
		"workers-large":      func(o *webhook.RuntimeOptions) { o.Workers = 33 },
		"timeout-zero":       func(o *webhook.RuntimeOptions) { o.RequestTimeout = 0 },
		"timeout-large":      func(o *webhook.RuntimeOptions) { o.RequestTimeout = time.Minute + 1 },
		"attempts-zero":      func(o *webhook.RuntimeOptions) { o.MaxAttempts = 0 },
		"attempts-large":     func(o *webhook.RuntimeOptions) { o.MaxAttempts = 1001 },
		"store-timeout-zero": func(o *webhook.RuntimeOptions) { o.StoreOperationTimeout = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			store := newSavedStore()
			sender := &busSender{calls: make(chan busSend, 1)}
			provider := &mutableSubscriptions{}
			options := runtimeOptions()
			change(&options)
			r, err := webhook.NewRuntime(context.Background(), options, provider, store, sender)
			require.ErrorIs(t, err, webhook.ErrInvalidDelivery)
			require.Nil(t, r)
			require.Zero(t, store.closes)
			require.Zero(t, sender.closes)
		})
	}
	for _, subs := range [][]webhook.Subscription{
		{{ID: "", Target: "topic", Events: []string{webhook.TypeCreated}}},
		{{ID: "x", Target: " ", Events: []string{webhook.TypeCreated}}},
		{{ID: "x", Target: "topic\nprivate", Events: []string{webhook.TypeCreated}}},
		{{ID: "x", Target: strings.Repeat("x", webhook.MaxDeliveryTargetBytes+1), Events: []string{webhook.TypeCreated}}},
		{{ID: "x", Target: "topic", Events: []string{"unknown"}}},
		{{ID: "x", Target: "topic", Events: []string{webhook.TypeCreated, webhook.TypeCreated}}},
		{{ID: "x", Target: "topic"}},
		{{ID: "x", Target: "a", Events: []string{webhook.TypeCreated}}, {ID: "x", Target: "b", Events: []string{webhook.TypeCreated}}},
		make([]webhook.Subscription, 65),
	} {
		store := newSavedStore()
		sender := &busSender{calls: make(chan busSend, 1)}
		r, err := webhook.NewRuntime(context.Background(), runtimeOptions(), &mutableSubscriptions{items: subs}, store, sender)
		require.ErrorIs(t, err, webhook.ErrInvalidDelivery)
		require.Nil(t, r)
		require.Zero(t, store.closes)
		require.Zero(t, sender.closes)
	}
	store := newSavedStore()
	sender := &busSender{calls: make(chan busSend, 1)}
	provider := &mutableSubscriptions{}
	_, err := webhook.NewRuntime(context.Background(), runtimeOptions(), nil, store, sender)
	require.ErrorIs(t, err, webhook.ErrInvalidDelivery)
	_, err = webhook.NewRuntime(context.Background(), runtimeOptions(), provider, nil, sender)
	require.ErrorIs(t, err, webhook.ErrInvalidDelivery)
	_, err = webhook.NewRuntime(context.Background(), runtimeOptions(), provider, store, nil)
	require.ErrorIs(t, err, webhook.ErrInvalidDelivery)
}
