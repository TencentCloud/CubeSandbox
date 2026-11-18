// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"context"
	"errors"
	"time"
)

// DeliveryStore owns single-target snapshots. Claim consumes an attempt;
// Complete or Retry resolves that claim. Every operation must respect ctx and
// use short local critical sections. Drain waits only for this instance's work.
// While running, Runtime polls Claim again after errors with a cancellable 50ms
// wait and a fresh operation context. Backends own failed or ambiguous claim
// state; polling does not automatically recover or transfer existing claims.
// Close runs after writer/workers join and returns actual discarded tasks;
// retained durable tasks are not discarded. Close must be safe to repeat.
type DeliveryStore interface {
	Put(context.Context, DeliveryTask) error
	Claim(context.Context, time.Time) (*ClaimedTask, error)
	Complete(context.Context, Receipt, TerminalResult) error
	Retry(context.Context, Receipt, time.Time, FailureSummary) error
	Drain(context.Context) error
	Close(context.Context) (discarded int, err error)
}

type DeliveryTask struct {
	Event          *Event
	SubscriptionID string
	Target         string
	CreatedAt      time.Time
	NextAttemptAt  time.Time
	MaxAttempts    int
}

// Receipt identifies one claim. Backends generate and validate nonempty tokens;
// Runtime passes them through unchanged. Tokens grant no lease or takeover.
type Receipt struct{ Token string }
type ClaimedTask struct {
	Task     DeliveryTask
	Attempts int
	Receipt  Receipt
}
type DeliveryState string

const (
	DeliverySucceeded DeliveryState = "succeeded"
	DeliveryFailed    DeliveryState = "failed"
	DeliveryCancelled DeliveryState = "cancelled"
)

// FailureSummary contains sanitized diagnostics, never a URL or response body.
type FailureSummary struct {
	Code       string
	HTTPStatus int
	Message    string
}
type TerminalResult struct {
	State   DeliveryState
	Summary FailureSummary
}

var (
	ErrInvalidDelivery  = errors.New("webhook: invalid delivery argument")
	ErrDeliveryCapacity = errors.New("webhook: delivery capacity exhausted")
	ErrDeliveryConflict = errors.New("webhook: immutable delivery conflict")
	ErrStaleReceipt     = errors.New("webhook: stale receipt")
	ErrDeliveryClosed   = errors.New("webhook: delivery store closed")
)

const (
	MaxDeliveryBodyBytes   = 256 * 1024
	MaxDeliveryTargetBytes = 4096
	MaxSubscriptionIDBytes = 1024
	MaxFailureMessageBytes = 1024
	maxDeliveryAttempts    = 1000
)
