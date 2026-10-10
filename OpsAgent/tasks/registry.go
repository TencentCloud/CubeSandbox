// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package tasks hosts the task domains: self-contained capability units the
// agent skeleton mounts as HTTP routes and reconcile hooks. Adding a domain
// must never require touching internal/.
package tasks

import (
	"context"
	"net/http"
)

// TaskDomain is one remotely driven node capability.
type TaskDomain interface {
	// Name identifies the domain in logs.
	Name() string
	// Routes returns the domain's routes, keyed by Go 1.22 ServeMux pattern.
	Routes() map[string]http.HandlerFunc
	// Reconcile runs once per tick; no-op for domains without a pull model.
	Reconcile(ctx context.Context)
}
