// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package sandbox

import "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"

var lifecyclePublisher webhook.PublisherHolder

func SetWebhookPublisher(p webhook.Publisher) { lifecyclePublisher.Set(p) }
