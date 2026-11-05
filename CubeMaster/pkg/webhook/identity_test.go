// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gomodule/redigo/redis"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

// encodeBody is the wire body a subscriber would receive for one published event.
func encodeBody(t *testing.T, c *CapturedEvent) map[string]any {
	t.Helper()
	p, err := MapToProtocol(c)
	require.NoError(t, err)
	ev, err := Encode(c, p)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(ev.Body(), &body))
	return body
}

func takeEvent(t *testing.T, p *BufferedPublisher) *CapturedEvent {
	t.Helper()
	select {
	case c := <-p.Events():
		return c
	default:
		t.Fatal("expected a published event")
		return nil
	}
}

// A build id in the context is what makes sandbox_build_id on a template event
// non-empty; the field used to be carried in a private context key that nothing
// read, so every template build event shipped with an empty one.
func TestPublishContextCarriesBuildID(t *testing.T) {
	p, err := NewBufferedPublisher(2)
	require.NoError(t, err)
	var h PublisherHolder
	h.Set(p)

	h.PublishContext(WithBuildID(context.Background(), "job-456"), TypeTemplateBuildStarted, "", "tmpl-1", LifecycleFields{})

	body := encodeBody(t, takeEvent(t, p))
	require.Equal(t, "job-456", body["sandbox_build_id"])
	require.Equal(t, "tmpl-1", body["sandbox_template_id"])
}

// The build id must survive the same path the template flow uses: a context
// that already carries an origin, which PublishContext also reads.
func TestPublishContextCarriesBuildIDWithOrigin(t *testing.T) {
	p, err := NewBufferedPublisher(2)
	require.NoError(t, err)
	var h PublisherHolder
	h.Set(p)

	ctx := WithOrigin(context.Background(), OriginLifecycle)
	h.PublishContext(WithBuildID(ctx, "job-789"), TypeTemplateBuildCompleted, "", "tmpl-1", LifecycleFields{})

	require.Equal(t, "job-789", encodeBody(t, takeEvent(t, p))["sandbox_build_id"])
}

// Snapshot events used to carry the snapshot id in sandbox_template_id, so a
// consumer could not tell a snapshot from a template, and snapshot.created could
// not name both the snapshot it produced and the sandbox it came from.
func TestSnapshotEventCarriesSnapshotID(t *testing.T) {
	p, err := NewBufferedPublisher(2)
	require.NoError(t, err)
	var h PublisherHolder
	h.Set(p)

	h.PublishSnapshotContext(context.Background(), TypeSnapshotCreated, "sb-1", "tpl-1", "snap-9", LifecycleFields{InstanceType: "s3"})

	body := encodeBody(t, takeEvent(t, p))
	require.Equal(t, "snap-9", body["snapshot_id"])
	require.Equal(t, "sb-1", body["sandbox_id"])
	require.Equal(t, "tpl-1", body["sandbox_template_id"])
}

// A snapshot event without a snapshot id is an unusable event, not a template
// event with a misplaced id.
func TestSnapshotEventRequiresSnapshotID(t *testing.T) {
	in := captureInput(TypeSnapshotDeleted)
	in.SnapshotID = ""
	_, err := Capture(in)
	require.ErrorIs(t, err, ErrInvalidEvent)

	in.SnapshotID = "snap-9"
	_, err = Capture(in)
	require.NoError(t, err)
}

// CAPACITY is the expected reply once the shared queue is full, so treating it
// like a broken socket threw away a healthy pooled connection and re-dialed
// exactly when the store was saturated.
func TestConnectionUnusableOnlyOnBrokenSockets(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"capacity reply", redis.Error("CAPACITY"), false},
		{"conflict reply", redis.Error("CONFLICT"), false},
		{"stale reply", redis.Error("STALE"), false},
		{"invalid reply", redis.Error("ERR INVALID"), false},
		{"noscript", redis.Error("NOSCRIPT No matching script. Please use EVAL."), false},
		{"readonly after failover", redis.Error("READONLY You can't write against a read only replica."), true},
		{"loading", redis.Error("LOADING Redis is loading the dataset in memory"), true},
		{"io error", errors.New("connection reset by peer"), true},
		{"context deadline", context.DeadlineExceeded, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, connectionUnusable(tc.err))
		})
	}
}

// A disabled or invalid configuration must not report itself as a live
// publisher: every create/update/delete/pause path checks Enabled() before doing
// the Prepare/Capture work, and *Runtime is not a NopPublisher.
func TestDisabledRuntimePublisherIsNop(t *testing.T) {
	disabled, err := newRuntime(context.Background(), config.WebhookConf{}, nil, nil, nil)
	require.NoError(t, err)
	_, nop := disabled.Publisher().(NopPublisher)
	require.True(t, nop, "a disabled runtime must publish nothing")

	var h PublisherHolder
	h.Set(disabled.Publisher())
	require.False(t, h.Enabled())

	invalid, err := newRuntime(context.Background(), config.WebhookConf{Invalid: true}, nil, nil, nil)
	require.ErrorIs(t, err, config.ErrWebhookConfig)
	_, nop = invalid.Publisher().(NopPublisher)
	require.True(t, nop, "an invalid configuration must publish nothing")

	h.Set(invalid.Publisher())
	require.False(t, h.Enabled())
}
