// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"context"
	"errors"
	"time"
)

// DeliveryStore owns bounded, process-local single-target snapshots. Claim
// consumes an attempt; Complete or Retry must resolve every successful claim.
type DeliveryStore interface {
	Put(context.Context, DeliveryTask) error
	Claim(context.Context, time.Time) (*ClaimedTask, error)
	Complete(context.Context, Receipt, TerminalResult) error
	Retry(context.Context, Receipt, time.Time, FailureSummary) error
}

type DeliveryTask struct {
	Event          *Event
	SubscriptionID string
	TargetURL      string
	CreatedAt      time.Time
	NextAttemptAt  time.Time
	MaxAttempts    int
}

// Receipt identifies one local claim by object identity. It grants no lease,
// takeover, persistence or cross-process ownership guarantees.
type Receipt struct{ claim *deliveryClaim }
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
)

const (
	MaxDeliveryBodyBytes   = 256 * 1024
	MaxDeliveryURLBytes    = 4096
	MaxSubscriptionIDBytes = 1024
	MaxFailureMessageBytes = 1024
	maxDeliveryAttempts    = 1000
)
