// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Event is an immutable encoding shared by future delivery targets.
type Event struct {
	id         uuid.UUID
	eventType  string
	occurredAt time.Time
	body       []byte
}

func (e *Event) ID() uuid.UUID         { return e.id }
func (e *Event) Type() string          { return e.eventType }
func (e *Event) OccurredAt() time.Time { return e.occurredAt }
func (e *Event) Body() []byte          { return append([]byte(nil), e.body...) }

type envelope struct {
	ID                 uuid.UUID      `json:"id"`
	Version            string         `json:"version"`
	Type               string         `json:"type"`
	Timestamp          string         `json:"timestamp"`
	EventCategory      string         `json:"event_category"`
	EventLabel         string         `json:"event_label"`
	SandboxID          string         `json:"sandbox_id"`
	SandboxExecutionID string         `json:"sandbox_execution_id"`
	SandboxTemplateID  string         `json:"sandbox_template_id"`
	SandboxBuildID     string         `json:"sandbox_build_id"`
	SandboxTeamID      string         `json:"sandbox_team_id"`
	EventData          map[string]any `json:"event_data,omitempty"`
	EventsTTLDays      int            `json:"events_ttl_days,omitempty"`
}

// Encode validates explicit protocol data and marshals once in the background.
// It does not perform business mapping or generate identities/timestamps.
func Encode(c *CapturedEvent, p *ProtocolEventData) (*Event, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("%w: nil protocol data", ErrInvalidEvent)
	}
	data, err := protocolData(c.input.Type, p)
	if err != nil {
		return nil, err
	}
	in := c.input
	body, err := json.Marshal(envelope{
		ID: in.ID, Version: VersionV2, Type: in.Type,
		Timestamp: in.OccurredAt.Format(time.RFC3339Nano),
		SandboxID: in.SandboxID, SandboxExecutionID: in.ExecutionID,
		SandboxTemplateID: in.TemplateID, SandboxBuildID: in.BuildID,
		SandboxTeamID: in.TeamID, EventData: data,
	})
	if err != nil {
		return nil, err
	}
	return &Event{id: in.ID, eventType: in.Type, occurredAt: in.OccurredAt, body: body}, nil
}

func protocolData(eventType string, p *ProtocolEventData) (map[string]any, error) {
	if (p.Execution != nil && eventType != TypePaused && eventType != TypeKilled) ||
		(p.PauseMode != "" && eventType != TypePaused) ||
		(p.ResumeMode != "" && eventType != TypeResumed) ||
		(p.KillReason != "" && eventType != TypeKilled) ||
		((p.SetTimeout != nil || p.NetworkEgress != nil) && eventType != TypeUpdated) {
		return nil, fmt.Errorf("%w: fields do not belong to event type", ErrInvalidEvent)
	}
	data := make(map[string]any)
	if p.SandboxMetadata != nil {
		data["sandbox_metadata"] = p.SandboxMetadata
	}
	switch eventType {
	case TypeResumed:
		if p.ResumeMode != "restore" && p.ResumeMode != "reboot" {
			return nil, fmt.Errorf("%w: resume_mode must be restore or reboot", ErrInvalidEvent)
		}
		data["resume_mode"] = p.ResumeMode
	case TypePaused:
		if p.PauseMode != "full" && p.PauseMode != "filesystem" {
			return nil, fmt.Errorf("%w: pause_mode must be full or filesystem", ErrInvalidEvent)
		}
		data["pause_mode"] = p.PauseMode
	case TypeUpdated:
		if p.SetTimeout == nil && p.NetworkEgress == nil {
			return nil, fmt.Errorf("%w: updated requires timeout or network", ErrInvalidEvent)
		}
		if p.SetTimeout != nil {
			t, err := time.Parse(time.RFC3339, *p.SetTimeout)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid set_timeout", ErrInvalidEvent)
			}
			data["set_timeout"] = t.UTC().Format(time.RFC3339)
		}
		if p.NetworkEgress != nil {
			data["network_egress"] = p.NetworkEgress
		}
	case TypeKilled:
		reason := p.KillReason
		if reason == "" {
			reason = "unknown"
		}
		data["kill_reason"] = reason
	}
	if eventType == TypePaused || eventType == TypeKilled {
		execution := ExecutionData{}
		if p.Execution != nil {
			execution = *p.Execution
		}
		for _, n := range []*int64{execution.VCPUCount, execution.MemoryMB, execution.ExecutionTimeMS} {
			if n != nil && *n < 0 {
				return nil, fmt.Errorf("%w: negative execution value", ErrInvalidEvent)
			}
		}
		if execution.StartedAt != "" {
			t, err := time.Parse(time.RFC3339, execution.StartedAt)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid started_at", ErrInvalidEvent)
			}
			execution.StartedAt = t.UTC().Format(time.RFC3339)
		}
		data["execution"] = execution
	}
	return data, nil
}
