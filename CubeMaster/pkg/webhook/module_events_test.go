// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// moduleInput builds the minimal identity each event family requires: a
// template event names a template, a snapshot event names a snapshot, a volume
// event names a volume. Setting every id would hide a validation regression.
func moduleInput(eventType string) CaptureInput {
	in := CaptureInput{
		ID:         uuid.MustParse("d12727fc-ab97-43d8-80cf-556cc1215017"),
		Type:       eventType,
		Origin:     OriginLifecycle,
		OccurredAt: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
	}
	switch {
	case IsTemplateEvent(eventType):
		in.TemplateID = "tpl-1"
	case IsSnapshotEvent(eventType):
		in.SnapshotID = "snap-1"
	case IsVolumeEvent(eventType):
		in.VolumeID = "vol-1"
		in.Fields = PrepareVolumeLifecycle("nfs")
	}
	return in
}

var moduleEventTypes = []string{
	TypeTemplateBuildStarted, TypeTemplateBuildCompleted, TypeTemplateBuildFailed,
	TypeTemplateDeleted, TypeTemplateMigrated,
	TypeSnapshotCreated, TypeSnapshotDeleted, TypeSnapshotRollback,
	TypeVolumeCreated, TypeVolumeDeleted,
}

func TestModuleEventsEncodeAndRestore(t *testing.T) {
	for _, typ := range moduleEventTypes {
		t.Run(typ, func(t *testing.T) {
			c, err := Capture(moduleInput(typ))
			require.NoError(t, err)
			p, err := MapToProtocol(c)
			require.NoError(t, err)
			e, err := Encode(c, p)
			require.NoError(t, err)

			body := decodeBody(t, e)
			require.Equal(t, VersionV2, body["version"])
			require.Equal(t, typ, body["type"])

			switch {
			case IsTemplateEvent(typ):
				require.Equal(t, "tpl-1", body["sandbox_template_id"])
				require.NotContains(t, body, "snapshot_id")
				require.NotContains(t, body, "volume_id")
			case IsSnapshotEvent(typ):
				require.Equal(t, "snap-1", body["snapshot_id"])
				require.NotContains(t, body, "volume_id")
			case IsVolumeEvent(typ):
				require.Equal(t, "vol-1", body["volume_id"])
				require.NotContains(t, body, "snapshot_id")
				data, ok := body["event_data"].(map[string]any)
				require.True(t, ok)
				require.Equal(t, "nfs", data["volume_driver"])
			}

			restored, err := RestoreEvent(e.Snapshot())
			require.NoError(t, err)
			require.Equal(t, e.Body(), restored.Body())
			require.Equal(t, e.ID(), restored.ID())
		})
	}
}

func TestModuleEventsRejectMissingIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  string
		drop func(*CaptureInput)
	}{
		{"template", TypeTemplateDeleted, func(in *CaptureInput) { in.TemplateID = "" }},
		{"snapshot", TypeSnapshotDeleted, func(in *CaptureInput) { in.SnapshotID = "" }},
		{"volume", TypeVolumeDeleted, func(in *CaptureInput) { in.VolumeID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := moduleInput(tc.typ)
			tc.drop(&in)
			_, err := Capture(in)
			require.ErrorIs(t, err, ErrInvalidEvent)
		})
	}
}

// RestoreEvent revalidates what it is given, so a body whose identity does not
// match its snapshot must not be replayed through the delivery store.
func TestRestoreRejectsBodyWithWrongIdentity(t *testing.T) {
	c, err := Capture(moduleInput(TypeVolumeCreated))
	require.NoError(t, err)
	p, err := MapToProtocol(c)
	require.NoError(t, err)
	e, err := Encode(c, p)
	require.NoError(t, err)

	snap := e.Snapshot()
	snap.Type = TypeVolumeDeleted
	_, err = RestoreEvent(snap)
	require.ErrorIs(t, err, ErrInvalidEvent)
}

// The accepted set and the category predicates must describe the same events:
// a name added to one without the other is either uncapturable or unusable as a
// subscription filter.
func TestEventTableMatchesCategoryPredicates(t *testing.T) {
	require.Len(t, webhookEventNames, MaxEventsPerSubscription)
	seen := make(map[string]bool, len(webhookEventNames))
	for _, typ := range webhookEventNames {
		require.True(t, validType(typ), typ)
		require.True(t,
			IsSandboxEvent(typ) || IsTemplateEvent(typ) || IsSnapshotEvent(typ) || IsVolumeEvent(typ),
			"%s belongs to no category", typ)
		require.False(t, seen[typ], "%s is listed twice", typ)
		seen[typ] = true
	}
}
