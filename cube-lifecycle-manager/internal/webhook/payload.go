// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"time"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/redisstream"
)

// Event names delivered to Webhook endpoints.
const (
	EventCreated = "sandbox.created"
	EventDeleted = "sandbox.deleted"
	EventPaused  = "sandbox.paused"
	EventResumed = "sandbox.resumed"
)

// PayloadVersion identifies the payload schema so receivers can evolve
// parsing logic when a future version adds or renames fields.
const PayloadVersion = "1"

// Payload is the JSON body POSTed to a Webhook endpoint. The exact serialized
// bytes are what the HMAC signature covers, so fields are ordered and
// omitted deterministically via struct tags.
type Payload struct {
	Version   string `json:"version"`
	Event     string `json:"event"`
	Timestamp string `json:"timestamp"` // RFC 3339, mirrors X-Cube-Timestamp
	SandboxID string `json:"sandbox_id"`
	// TemplateID is set for sandbox.created when the stream event carries
	// lifecycle metadata.
	TemplateID string `json:"template_id,omitempty"`
	// State is the terminal runtime state for sandbox.paused ("paused") and
	// sandbox.resumed ("running").
	State string `json:"state,omitempty"`
}

// MapEvent converts a lifecycle stream event into its Webhook payload.
// It returns nil for events that are not external lifecycle transitions
// (metadata updates, transition markers, unknown ops, or state events with
// no usable payload), which the caller simply ACKs and skips.
//
// The payload timestamp prefers the stream entry's own timestamp (written by
// CubeMaster when the transition happened) over the consumption time, so
// redelivered events keep their original event time.
func MapEvent(ev redisstream.Event, now func() time.Time) *Payload {
	if now == nil {
		now = time.Now
	}
	ts := now()
	if ev.Timestamp > 0 {
		ts = time.UnixMilli(ev.Timestamp)
	}
	p := &Payload{
		Version:   PayloadVersion,
		Timestamp: ts.UTC().Format(time.RFC3339),
		SandboxID: ev.SandboxID,
	}
	switch ev.Op {
	case lifecycle.OpCreate:
		p.Event = EventCreated
		if ev.Meta != nil {
			p.TemplateID = ev.Meta.TemplateID
		}
	case lifecycle.OpDelete:
		p.Event = EventDeleted
	case lifecycle.OpState:
		if ev.State == nil {
			return nil
		}
		switch ev.State.State {
		case lifecycle.StatePaused:
			p.Event = EventPaused
			p.State = lifecycle.StatePaused
		case lifecycle.StateRunning:
			p.Event = EventResumed
			p.State = lifecycle.StateRunning
		default:
			// Transition markers ("pausing" / "resuming") are private to
			// CLM coordination and must never leave the cluster.
			return nil
		}
	default:
		return nil
	}
	return p
}
