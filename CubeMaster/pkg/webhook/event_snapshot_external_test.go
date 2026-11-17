// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
)

func encodedEvent(t *testing.T) *webhook.Event {
	t.Helper()
	c, err := webhook.Capture(webhook.CaptureInput{ID: uuid.New(), Type: webhook.TypeCreated, Origin: webhook.OriginLifecycle, SandboxID: "external", OccurredAt: time.Now().UTC()})
	require.NoError(t, err)
	p, err := webhook.MapToProtocol(c)
	require.NoError(t, err)
	e, err := webhook.Encode(c, p)
	require.NoError(t, err)
	return e
}

func TestEventSnapshotRestore(t *testing.T) {
	e := encodedEvent(t)
	accepted := operationCount(t, "capture", "accepted")
	snapshot := e.Snapshot()
	// Preserve whitespace and fields unknown to this version of the reader.
	snapshot.Body = append([]byte(" \n"), bytes.Replace(snapshot.Body, []byte(`"version":"v2"`), []byte(`"version":"v2", "future":{"x":1}`), 1)...)
	restored, err := webhook.RestoreEvent(snapshot)
	require.NoError(t, err)
	require.Equal(t, e.ID(), restored.ID())
	require.Equal(t, e.Type(), restored.Type())
	require.True(t, e.OccurredAt().Equal(restored.OccurredAt()))
	require.Equal(t, snapshot.Body, restored.Body())
	original := restored.Body()
	snapshot.Body[0] = 'x'
	copy := restored.Snapshot()
	copy.Body[0] = 'x'
	require.Equal(t, original, restored.Body())
	require.Equal(t, byte('{'), e.Body()[0])
	require.Equal(t, accepted, operationCount(t, "capture", "accepted"))
	for name, change := range map[string]func(*webhook.EventSnapshot){
		"zero-id":          func(s *webhook.EventSnapshot) { s.ID = uuid.Nil },
		"wrong-id":         func(s *webhook.EventSnapshot) { s.ID = uuid.New() },
		"wrong-type":       func(s *webhook.EventSnapshot) { s.Type = webhook.TypeKilled },
		"unknown-type":     func(s *webhook.EventSnapshot) { s.Type = "unknown" },
		"zero-time":        func(s *webhook.EventSnapshot) { s.OccurredAt = time.Time{} },
		"wrong-time":       func(s *webhook.EventSnapshot) { s.OccurredAt = s.OccurredAt.Add(time.Second) },
		"size":             func(s *webhook.EventSnapshot) { s.Body = []byte(strings.Repeat(" ", webhook.MaxDeliveryBodyBytes+1)) },
		"json":             func(s *webhook.EventSnapshot) { s.Body = []byte(`{`) },
		"null":             func(s *webhook.EventSnapshot) { s.Body = []byte(`null`) },
		"array":            func(s *webhook.EventSnapshot) { s.Body = []byte(`[]`) },
		"v1":               func(s *webhook.EventSnapshot) { s.Body = bytes.Replace(s.Body, []byte(`"v2"`), []byte(`"v1"`), 1) },
		"missing-envelope": func(s *webhook.EventSnapshot) { s.Body = []byte(`{}`) },
		"missing-sandbox": func(s *webhook.EventSnapshot) {
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(s.Body, &fields))
			delete(fields, "sandbox_id")
			s.Body, _ = json.Marshal(fields)
		},
		"null-identity": func(s *webhook.EventSnapshot) {
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(s.Body, &fields))
			fields["sandbox_execution_id"] = json.RawMessage(`null`)
			s.Body, _ = json.Marshal(fields)
		},
		"case-variant-cannot-override-id": func(s *webhook.EventSnapshot) {
			s.Body = bytes.Replace(s.Body, []byte(s.ID.String()), []byte(uuid.NewString()), 1)
			key, _ := json.Marshal(s.ID.String())
			s.Body = append(s.Body[:len(s.Body)-1], []byte(`,"ID":`)...)
			s.Body = append(s.Body, key...)
			s.Body = append(s.Body, '}')
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := e.Snapshot()
			change(&s)
			_, err := webhook.RestoreEvent(s)
			require.ErrorIs(t, err, webhook.ErrInvalidEvent)
		})
	}
}

func TestEventRestorePreservesUnknownFieldsWithoutNumericConversion(t *testing.T) {
	s := encodedEvent(t).Snapshot()
	s.Body = bytes.Replace(s.Body, []byte(`"version":"v2"`), []byte(`"version":"v2", "event_data":{"future":1e1000}, "ID":"unknown extension"`), 1)
	e, err := webhook.RestoreEvent(s)
	require.NoError(t, err)
	require.Equal(t, s.ID, e.ID())
	require.Equal(t, s.Body, e.Body())
}
