// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"errors"
	"time"
)

// DeliveryStore persists single-target snapshots. Claim consumes an attempt even
// when no HTTP request follows. A receipt protects storage ownership, not external
// exactly-once delivery. Callers must stop sending after an uncertain operation.
type DeliveryStore interface {
	Put(context.Context, DeliveryTask) error
	Claim(context.Context, time.Time, int, time.Duration) ([]ClaimedTask, error)
	Complete(context.Context, Receipt, TerminalResult) error
	Retry(context.Context, Receipt, time.Time, FailureSummary) error
	Prune(context.Context, time.Time, int) (int, error)
}

// DeliveryTask contains only immutable inputs. Reuse exactly this snapshot on an
// uncertain Put, including both initial timestamps. Event is produced by Encode.
type DeliveryTask struct {
	Event          *Event
	SubscriptionID string
	TargetURL      string
	CreatedAt      time.Time
	NextAttemptAt  time.Time
	MaxAttempts    int
}

// Receipt is opaque and valid until a successful transition or takeover. Merely
// passing LeaseUntil does not revoke ownership. Random tokens prevent ABA after
// pruning and recreation; receipts are not HTTP delivery IDs.
type Receipt struct {
	key, token string
	generation int
}
type ClaimedTask struct {
	Task       DeliveryTask
	Attempts   int
	LeaseUntil time.Time
	Receipt    Receipt
}

type DeliveryState string

const (
	DeliverySucceeded DeliveryState = "succeeded"
	DeliveryFailed    DeliveryState = "failed"
	DeliveryCancelled DeliveryState = "cancelled"
)

// FailureSummary must be sanitized by the caller: no credentials, response body,
// or sensitive URL. The store bounds size, but cannot infer sensitive contents.
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
	ErrStoreUnavailable = errors.New("webhook: store unavailable")
	ErrStoreUncertain   = errors.New("webhook: operation outcome uncertain")
	ErrStoreCorrupt     = errors.New("webhook: invalid store data; diagnosis required")
)

const (
	MaxDeliveryBodyBytes         = 256 * 1024
	MaxDeliveryURLBytes          = 4096
	MaxSubscriptionIDBytes       = 1024
	MaxFailureMessageBytes       = 1024
	MaxDeliveryBatch             = 32
	MaxDeliveryRecordBytes       = 512 * 1024
	maxImmutableBytes            = 384 * 1024 // leaves room for ownership and two bounded summaries
	maxDeliveryAttempts          = 1000
	maxStoreMillis         int64 = 9999999999999 // at most 13 digits: exact through Redis cjson encoding
)
