// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
)

// Every event type this package advertises to subscribers must have a producer
// that survives capture and encoding. A type that only exists in webhook's
// validType() is a silent no-op subscription: the config validates, the
// subscriber waits forever, and nothing fails.
func TestTemplateLifecycleProducersPublishDocumentedEvents(t *testing.T) {
	p, err := webhook.NewBufferedPublisher(16)
	require.NoError(t, err)
	SetWebhookPublisher(p)
	defer SetWebhookPublisher(nil)

	ctx := context.Background()
	PublishTemplateBuildStarted(ctx, "tmpl-1", "job-1", "ubuntu:22.04", "cubebox", "remote")
	PublishTemplateBuildCompleted(ctx, "tmpl-1", "job-1", "ubuntu:22.04", "cubebox", "remote", "art-1", 2)
	PublishTemplateBuildFailed(ctx, "tmpl-1", "job-1", "ubuntu:22.04", "cubebox", "remote", "boom")
	PublishTemplateDeleted(ctx, "tmpl-1", "cubebox")
	PublishTemplateMigrated(ctx, "tmpl-1", "job-2", "cubebox", 3)
	PublishSnapshotCreated(ctx, "snap-1", "sb-1", "tmpl-1", "s3")
	PublishSnapshotDeleted(ctx, "snap-1", "s3", "")
	PublishSnapshotRollback(ctx, "snap-1", "sb-1", 7)

	got := map[string]map[string]any{}
	for len(p.Events()) > 0 {
		c := <-p.Events()
		data, err := webhook.MapToProtocol(c)
		require.NoError(t, err)
		ev, err := webhook.Encode(c, data)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.Unmarshal(ev.Body(), &body))
		got[ev.Type()] = body
	}

	for _, typ := range []string{
		webhook.TypeTemplateBuildStarted, webhook.TypeTemplateBuildCompleted,
		webhook.TypeTemplateBuildFailed, webhook.TypeTemplateDeleted,
		webhook.TypeTemplateMigrated,
		webhook.TypeSnapshotCreated, webhook.TypeSnapshotDeleted, webhook.TypeSnapshotRollback,
	} {
		require.Contains(t, got, typ, "advertised event type has no producer that publishes %s", typ)
	}

	require.Equal(t, "job-1", got[webhook.TypeTemplateBuildStarted]["sandbox_build_id"])
	require.Equal(t, "tmpl-1", got[webhook.TypeTemplateBuildStarted]["sandbox_template_id"])
	require.Equal(t, "job-2", got[webhook.TypeTemplateMigrated]["sandbox_build_id"])
	migratedData, ok := got[webhook.TypeTemplateMigrated]["event_data"].(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, 3, migratedData["ready_node_count"])
	// A snapshot is its own resource: the id it is keyed by must be on the wire,
	// and must not be smuggled through sandbox_template_id.
	require.Equal(t, "snap-1", got[webhook.TypeSnapshotCreated]["snapshot_id"])
	require.Equal(t, "sb-1", got[webhook.TypeSnapshotCreated]["sandbox_id"])
	require.Equal(t, "snap-1", got[webhook.TypeSnapshotDeleted]["snapshot_id"])
	require.Equal(t, "snap-1", got[webhook.TypeSnapshotRollback]["snapshot_id"])
	require.Equal(t, "sb-1", got[webhook.TypeSnapshotRollback]["sandbox_id"])
}
