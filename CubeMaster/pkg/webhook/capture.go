// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Fixed field count and a per-string limit bound the entire P1 capture.
const maxStringBytes = 4096

// CaptureInput contains values already available at the success boundary.
// The caller supplies the event identity and occurrence time at that boundary.
type CaptureInput struct {
	ID          uuid.UUID
	Type        string
	Origin      Origin
	SandboxID   string
	TemplateID  string
	ExecutionID string
	BuildID     string
	TeamID      string
	OccurredAt  time.Time
}

// CapturedEvent owns a value snapshot; no mutable input is exposed.
type CapturedEvent struct {
	input CaptureInput
}

func validateCapture(in CaptureInput) error {
	// Check size before scanning strings, including whitespace validation.
	for _, s := range []string{
		in.Type, string(in.Origin), in.SandboxID, in.TemplateID,
		in.ExecutionID, in.BuildID, in.TeamID,
	} {
		if len(s) > maxStringBytes {
			return ErrCaptureTooLarge
		}
	}
	if in.ID == uuid.Nil || in.OccurredAt.IsZero() ||
		!validType(in.Type) || strings.TrimSpace(in.SandboxID) == "" {
		return ErrInvalidEvent
	}
	if !in.Origin.Allows(in.Type) {
		return ErrSuppressed
	}
	return nil
}

// Capture performs bounded validation and value copying, without business
// mapping, serialization, or I/O. Oversized fields are rejected, not truncated.
func Capture(in CaptureInput) (*CapturedEvent, error) {
	if err := validateCapture(in); err != nil {
		return nil, err
	}
	in.OccurredAt = in.OccurredAt.UTC()
	return &CapturedEvent{input: in}, nil
}

func (c *CapturedEvent) validate() error {
	if c == nil {
		return fmt.Errorf("%w: nil capture", ErrInvalidEvent)
	}
	return validateCapture(c.input)
}
