// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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

// EventSnapshot is an encoded event for backend storage. Body is independently
// owned by the caller; restoring it preserves the exact bytes and identity.
type EventSnapshot struct {
	ID         uuid.UUID
	Type       string
	OccurredAt time.Time
	Body       []byte
}

func (e *Event) Snapshot() EventSnapshot {
	return EventSnapshot{ID: e.id, Type: e.eventType, OccurredAt: e.occurredAt, Body: e.Body()}
}

// RestoreEvent validates a stored v2 envelope without re-encoding its body or
// generating an identity. It does not capture a new business event.
func RestoreEvent(s EventSnapshot) (*Event, error) {
	if s.ID == uuid.Nil || !validType(s.Type) || s.OccurredAt.IsZero() || len(s.Body) == 0 || len(s.Body) > MaxDeliveryBodyBytes || !utf8.Valid(s.Body) {
		return nil, ErrInvalidEvent
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(s.Body, &fields); err != nil {
		return nil, ErrInvalidEvent
	}
	// Decode exact envelope keys: encoding/json's struct matching would let
	// an unknown case variant (e.g. ID) override the actual protocol id.
	text := make(map[string]string)
	for _, key := range []string{"id", "version", "type", "timestamp", "event_category", "event_label", "sandbox_id", "sandbox_execution_id", "sandbox_template_id", "sandbox_build_id", "sandbox_team_id"} {
		value, ok := fields[key]
		var decoded string
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &decoded) != nil {
			return nil, ErrInvalidEvent
		}
		text[key] = decoded
	}
	// snapshot_id is omitempty: only snapshot events carry it.
	if raw, ok := fields["snapshot_id"]; ok {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return nil, ErrInvalidEvent
		}
		text["snapshot_id"] = value
	}
	// volume_id is omitempty: only volume events carry it.
	if raw, ok := fields["volume_id"]; ok {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return nil, ErrInvalidEvent
		}
		text["volume_id"] = value
	}
	id, err := uuid.Parse(text["id"])
	if err != nil || id != s.ID || text["version"] != VersionV2 || text["type"] != s.Type {
		return nil, ErrInvalidEvent
	}
	// The identifying id depends on the category: a template event has no
	// sandbox, a snapshot event has no template of its own.
	if IsSandboxEvent(s.Type) && strings.TrimSpace(text["sandbox_id"]) == "" {
		return nil, ErrInvalidEvent
	}
	if IsTemplateEvent(s.Type) && strings.TrimSpace(text["sandbox_template_id"]) == "" {
		return nil, ErrInvalidEvent
	}
	if IsSnapshotEvent(s.Type) && strings.TrimSpace(text["snapshot_id"]) == "" {
		return nil, ErrInvalidEvent
	}
	if IsVolumeEvent(s.Type) && strings.TrimSpace(text["volume_id"]) == "" {
		return nil, ErrInvalidEvent
	}
	timestamp, err := time.Parse(time.RFC3339Nano, text["timestamp"])
	if err != nil || !timestamp.Equal(s.OccurredAt) {
		return nil, ErrInvalidEvent
	}
	for _, key := range []string{"sandbox_id", "sandbox_execution_id", "snapshot_id", "volume_id", "sandbox_template_id", "sandbox_build_id", "sandbox_team_id"} {
		if len(text[key]) > maxStringBytes {
			return nil, ErrInvalidEvent
		}
	}
	if raw, ok := fields["event_data"]; ok {
		// Raw values preserve unknown fields, including numbers outside the
		// float64 range. Validation never converts or re-encodes payload data.
		var data map[string]json.RawMessage
		if json.Unmarshal(raw, &data) != nil {
			return nil, ErrInvalidEvent
		}
	}
	if raw, ok := fields["events_ttl_days"]; ok {
		var days int
		if json.Unmarshal(raw, &days) != nil {
			return nil, ErrInvalidEvent
		}
	}
	return &Event{id: s.ID, eventType: s.Type, occurredAt: s.OccurredAt, body: append([]byte(nil), s.Body...)}, nil
}

type envelope struct {
	ID                 uuid.UUID `json:"id"`
	Version            string    `json:"version"`
	Type               string    `json:"type"`
	Timestamp          string    `json:"timestamp"`
	EventCategory      string    `json:"event_category"`
	EventLabel         string    `json:"event_label"`
	SandboxID          string    `json:"sandbox_id"`
	SandboxExecutionID string    `json:"sandbox_execution_id"`
	SandboxTemplateID  string    `json:"sandbox_template_id"`
	// snapshot_id, not sandbox_snapshot_id: a snapshot is its own resource, not
	// an attribute of the sandbox that produced it, and every other Cube API
	// surface already spells it snapshot_id.
	SnapshotID     string         `json:"snapshot_id,omitempty"`
	VolumeID       string         `json:"volume_id,omitempty"`
	SandboxBuildID string         `json:"sandbox_build_id"`
	SandboxTeamID  string         `json:"sandbox_team_id"`
	EventData      map[string]any `json:"event_data,omitempty"`
	EventsTTLDays  int            `json:"events_ttl_days,omitempty"`
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
	// event_category and event_label stay empty: the v2 envelope keeps the keys
	// for wire compatibility, but Cube has no legacy classification to map them
	// from and inventing one would add a second source of truth beside `type`.
	in := c.input
	body, err := json.Marshal(envelope{
		ID: in.ID, Version: VersionV2, Type: in.Type,
		Timestamp: in.OccurredAt.Format(time.RFC3339Nano),
		SandboxID: in.SandboxID, SandboxExecutionID: in.ExecutionID,
		SandboxTemplateID: in.TemplateID, SnapshotID: in.SnapshotID, VolumeID: in.VolumeID, SandboxBuildID: in.BuildID,
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
		// Template and snapshot events carry their own data via TemplateInfo/ErrorMsg,
		// so the above guard does not apply to them.
		if !IsTemplateEvent(eventType) && !IsSnapshotEvent(eventType) {
			return nil, fmt.Errorf("%w: fields do not belong to event type", ErrInvalidEvent)
		}
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
	case TypeTemplateBuildStarted:
		data["source_image_ref"] = p.SourceImageRef
		data["instance_type"] = p.InstanceType
		data["build_mode"] = p.BuildMode
	case TypeTemplateBuildCompleted:
		data["source_image_ref"] = p.SourceImageRef
		data["artifact_id"] = p.ArtifactID
		data["instance_type"] = p.InstanceType
		data["build_mode"] = p.BuildMode
		if p.ReadyNodeCount >= 0 {
			data["ready_node_count"] = p.ReadyNodeCount
		}
	case TypeTemplateBuildFailed:
		data["source_image_ref"] = p.SourceImageRef
		data["error_message"] = p.ErrorMsg
		data["build_mode"] = p.BuildMode
	case TypeTemplateDeleted:
		data["instance_type"] = p.InstanceType
	case TypeTemplateMigrated:
		data["instance_type"] = p.InstanceType
		if p.ReadyNodeCount >= 0 {
			data["ready_node_count"] = p.ReadyNodeCount
		}
	case TypeSnapshotCreated:
		data["backend"] = p.InstanceType
	case TypeSnapshotDeleted:
		data["error_message"] = p.ErrorMsg
	case TypeSnapshotRollback:
		data["new_gen"] = p.NewGen
	case TypeVolumeCreated, TypeVolumeDeleted:
		if p.VolumeDriver != "" {
			data["volume_driver"] = p.VolumeDriver
		}
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
