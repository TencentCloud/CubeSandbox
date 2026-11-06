// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var eventTypes = []string{TypeCreated, TypeKilled, TypePaused, TypeResumed, TypeUpdated, TypeCheckpointed}

func captureInput(eventType string) CaptureInput {
	return CaptureInput{
		ID:   uuid.MustParse("d12727fc-ab97-43d8-80cf-556cc1215017"),
		Type: eventType, Origin: OriginLifecycle, SandboxID: "sb-1", TemplateID: "tpl-1",
		OccurredAt: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
	}
}

func captured(t *testing.T, eventType string) *CapturedEvent {
	t.Helper()
	c, err := Capture(captureInput(eventType))
	require.NoError(t, err)
	return c
}

func TestOriginAllows(t *testing.T) {
	for _, origin := range []Origin{
		OriginLifecycle, OriginResumeTimeoutRefresh, OriginCreateFailover,
		OriginPauseTeardown, "", "unknown",
	} {
		for _, typ := range append(append([]string(nil), eventTypes...), "", "unknown") {
			t.Run(string(origin)+"/"+typ, func(t *testing.T) {
				allowed := origin == OriginLifecycle && typ != "" && typ != "unknown"
				require.Equal(t, allowed, origin.Allows(typ))
				in := captureInput(typ)
				in.Origin = origin
				c, err := Capture(in)
				if allowed {
					require.NoError(t, err)
					require.NotNil(t, c)
				} else {
					require.Error(t, err)
					require.Nil(t, c)
				}
			})
		}
	}
}

func TestCaptureValueSnapshot(t *testing.T) {
	in := captureInput(TypeCreated)
	in.ExecutionID, in.BuildID, in.TeamID = "execution", "build", "team"
	in.OccurredAt = time.Date(2026, 10, 14, 20, 0, 0, 123, time.FixedZone("east", 8*3600))
	before := in
	c, err := Capture(in)
	require.NoError(t, err)
	in = CaptureInput{} // Overwrite every caller-owned field.
	require.Equal(t, CaptureInput{}, in)
	p, err := MapToProtocol(c)
	require.NoError(t, err)
	e, err := Encode(c, p)
	require.NoError(t, err)
	require.Equal(t, before.ID, e.ID())
	require.Equal(t, before.Type, e.Type())
	require.True(t, before.OccurredAt.Equal(e.OccurredAt()))
	require.Same(t, time.UTC, e.OccurredAt().Location())
	body := decodeBody(t, e)
	require.Equal(t, "2026-10-14T12:00:00.000000123Z", body["timestamp"])
	require.Equal(t, before.SandboxID, body["sandbox_id"])
	require.Equal(t, before.TemplateID, body["sandbox_template_id"])
	require.Equal(t, before.ExecutionID, body["sandbox_execution_id"])
	require.Equal(t, before.BuildID, body["sandbox_build_id"])
	require.Equal(t, before.TeamID, body["sandbox_team_id"])
}

func TestCaptureStringLimits(t *testing.T) {
	fields := []struct {
		name       string
		set        func(*CaptureInput, string)
		atLimitErr error
	}{
		{"sandbox", func(in *CaptureInput, s string) { in.SandboxID = s }, nil},
		{"template", func(in *CaptureInput, s string) { in.TemplateID = s }, nil},
		{"execution", func(in *CaptureInput, s string) { in.ExecutionID = s }, nil},
		{"build", func(in *CaptureInput, s string) { in.BuildID = s }, nil},
		{"team", func(in *CaptureInput, s string) { in.TeamID = s }, nil},
		// Enumerations remain subject to semantic validation at the size limit.
		{"type", func(in *CaptureInput, s string) { in.Type = s }, ErrInvalidEvent},
		{"origin", func(in *CaptureInput, s string) { in.Origin = Origin(s) }, ErrSuppressed},
	}
	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			in := captureInput(TypeCreated)
			field.set(&in, strings.Repeat("x", maxStringBytes))
			c, err := Capture(in)
			if field.atLimitErr == nil {
				require.NoError(t, err)
				require.NotNil(t, c)
			} else {
				require.ErrorIs(t, err, field.atLimitErr)
				require.Nil(t, c)
			}
			field.set(&in, strings.Repeat("x", maxStringBytes+1))
			c, err = Capture(in)
			require.ErrorIs(t, err, ErrCaptureTooLarge)
			require.Nil(t, c)
		})
	}
	// The bound is bytes, not rune count; all identity fields may reach it.
	in := captureInput(TypeCreated)
	in.SandboxID = strings.Repeat("é", maxStringBytes/2)
	in.TemplateID, in.ExecutionID, in.BuildID, in.TeamID = in.SandboxID, in.SandboxID, in.SandboxID, in.SandboxID
	_, err := Capture(in)
	require.NoError(t, err)
	in.SandboxID += "é"
	_, err = Capture(in)
	require.ErrorIs(t, err, ErrCaptureTooLarge)
}

func TestInvalidCapture(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*CaptureInput)
	}{
		{"zero ID", func(in *CaptureInput) { in.ID = uuid.Nil }},
		{"zero time", func(in *CaptureInput) { in.OccurredAt = time.Time{} }},
		{"empty sandbox", func(in *CaptureInput) { in.SandboxID = "" }},
		{"blank sandbox", func(in *CaptureInput) { in.SandboxID = " \t\n" }},
		{"unknown type", func(in *CaptureInput) { in.Type = "created" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := captureInput(TypeCreated)
			tc.change(&in)
			c, err := Capture(in)
			require.ErrorIs(t, err, ErrInvalidEvent)
			require.Nil(t, c)
		})
	}
}
