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
)

// Origin identifies internal operations and is never part of the public body.
type Origin string

const (
	OriginLifecycle            Origin = "lifecycle"
	OriginResumeTimeoutRefresh Origin = "resume_timeout_refresh"
	OriginCreateFailover       Origin = "create_failover"
	OriginPauseTeardown        Origin = "pause_teardown"
)

func validType(eventType string) bool {
	switch eventType {
	case TypeCreated, TypeKilled, TypePaused, TypeResumed, TypeUpdated, TypeCheckpointed:
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
