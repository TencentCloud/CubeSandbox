// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package webhook provides bounded lifecycle event publication and background
// protocol encoding. Acceptance into memory does not imply durable delivery.
package webhook

import "errors"

const (
	VersionV2        = "v2"
	TypeCreated      = "sandbox.lifecycle.created"
	TypeKilled       = "sandbox.lifecycle.killed"
	TypePaused       = "sandbox.lifecycle.paused"
	TypeResumed      = "sandbox.lifecycle.resumed"
	TypeUpdated      = "sandbox.lifecycle.updated"
	TypeCheckpointed = "sandbox.lifecycle.checkpointed"

	// Template lifecycle events
	TypeTemplateBuildStarted   = "template.lifecycle.build.started"
	TypeTemplateBuildCompleted = "template.lifecycle.build.completed"
	TypeTemplateBuildFailed    = "template.lifecycle.build.failed"
	TypeTemplateDeleted        = "template.lifecycle.deleted"
	// TypeTemplateMigrated is emitted when a template is redistributed
	// to a new set of nodes via the redo/migrate flow.
	TypeTemplateMigrated = "template.lifecycle.migrated"

	// Snapshot lifecycle events
	TypeSnapshotCreated  = "template.lifecycle.snapshot.created"
	TypeSnapshotDeleted  = "template.lifecycle.snapshot.deleted"
	TypeSnapshotRollback = "template.lifecycle.snapshot.rollback.completed"

	// Volume lifecycle events. A volume is its own resource with its own id, so
	// it sits beside the sandbox and template families rather than under them.
	TypeVolumeCreated = "volume.lifecycle.created"
	TypeVolumeDeleted = "volume.lifecycle.deleted"
)

// Origin identifies internal operations and is never part of the public body.
type Origin string

const (
	OriginLifecycle            Origin = "lifecycle"
	OriginResumeTimeoutRefresh Origin = "resume_timeout_refresh"
	OriginCreateFailover       Origin = "create_failover"
	OriginPauseTeardown        Origin = "pause_teardown"
)

// webhookEventNames is the single source of truth for the accepted types. Both
// the membership check and the per-subscription bound derive from it, so a
// subscription that lists every supported event stays valid.
var webhookEventNames = []string{
	TypeCreated, TypeKilled, TypePaused, TypeResumed, TypeUpdated, TypeCheckpointed,

	TypeTemplateBuildStarted, TypeTemplateBuildCompleted, TypeTemplateBuildFailed,
	TypeTemplateDeleted, TypeTemplateMigrated,

	TypeSnapshotCreated, TypeSnapshotDeleted, TypeSnapshotRollback,

	TypeVolumeCreated, TypeVolumeDeleted,
}

// MaxEventsPerSubscription bounds one subscription's filter; subscribing to
// every supported type must be accepted.
var MaxEventsPerSubscription = len(webhookEventNames)

func validType(eventType string) bool {
	for _, name := range webhookEventNames {
		if name == eventType {
			return true
		}
	}
	return false
}

// IsSandboxEvent reports whether the event type belongs to the sandbox category.
func IsSandboxEvent(eventType string) bool {
	switch eventType {
	case TypeCreated, TypeKilled, TypePaused, TypeResumed, TypeUpdated, TypeCheckpointed:
		return true
	default:
		return false
	}
}

// IsTemplateEvent reports whether the event type belongs to the template category.
func IsTemplateEvent(eventType string) bool {
	switch eventType {
	case TypeTemplateBuildStarted, TypeTemplateBuildCompleted, TypeTemplateBuildFailed,
		TypeTemplateDeleted, TypeTemplateMigrated:
		return true
	default:
		return false
	}
}

// IsSnapshotEvent reports whether the event type belongs to the snapshot category.
func IsSnapshotEvent(eventType string) bool {
	switch eventType {
	case TypeSnapshotCreated, TypeSnapshotDeleted, TypeSnapshotRollback:
		return true
	default:
		return false
	}
}

// IsVolumeEvent reports whether the event type belongs to the volume category.
func IsVolumeEvent(eventType string) bool {
	switch eventType {
	case TypeVolumeCreated, TypeVolumeDeleted:
		return true
	default:
		return false
	}
}

func (o Origin) Allows(eventType string) bool {
	return o == OriginLifecycle && validType(eventType)
}

var (
	ErrInvalidEvent    = errors.New("webhook: invalid event")
	ErrCaptureTooLarge = errors.New("webhook: capture exceeds string limit")
	ErrSuppressed      = errors.New("webhook: origin suppresses event")
)

type PublishStatus int

const (
	PublishAccepted PublishStatus = iota
	PublishDisabled
	PublishFull
	PublishStopped
	PublishInvalid
)

// PublishResult is for observation; it must not change the sandbox result.
type PublishResult struct {
	Status PublishStatus
	Err    error
}

type Publisher interface {
	TryPublish(ev *CapturedEvent) PublishResult
}
