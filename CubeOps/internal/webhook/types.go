// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const internalSchemaVersion = 1

// Event preserves the flattened JSON representation emitted by CubeAPI.
type Event map[string]json.RawMessage

// InternalBatch is the CubeAPI-to-CubeOps transport envelope.
type InternalBatch struct {
	SchemaVersion int     `json:"schema_version"`
	Events        []Event `json:"events"`
}

// ExternalBatch is the envelope delivered to subscribed webhook endpoints.
type ExternalBatch struct {
	BatchID string  `json:"batch_id"`
	Events  []Event `json:"events"`
}

// Name returns the event routing key, or an empty string when it is invalid.
func (e Event) Name() string {
	var name string
	if err := json.Unmarshal(e["event"], &name); err != nil {
		return ""
	}
	return name
}

func validateEvent(event Event) error {
	timestamp, err := requiredString(event, "timestamp")
	if err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339, timestamp); err != nil {
		return fmt.Errorf("timestamp must use RFC3339: %w", err)
	}

	if _, err := requiredString(event, "level"); err != nil {
		return err
	}
	if _, err := requiredString(event, "event"); err != nil {
		return err
	}
	return nil
}

func requiredString(event Event, field string) (string, error) {
	var value string
	if err := json.Unmarshal(event[field], &value); err != nil {
		return "", fmt.Errorf("%s must be a string: %w", field, err)
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s must not be empty", field)
	}
	return value, nil
}
