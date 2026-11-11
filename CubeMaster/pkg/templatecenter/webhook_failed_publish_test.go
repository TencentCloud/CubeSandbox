// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
)

// A builder-side failure only ever reaches CubeMaster as a FAILED job row, so
// build.failed has to be published from that row. Producing it only on the
// resume error branch leaves "build failed" subscribers silent on the most
// common failure (the builder reporting FAILED through the status callback,
// and the reconcile path that replays after a restart).
func TestFailTemplateImageJobPublishesBuildFailedFromTheJobRow(t *testing.T) {
	db := newBuiltCallbackTestDB(t)
	require.NoError(t, db.Create(&models.TemplateImageJob{
		JobID:          "job-fail",
		TemplateID:     "tmpl-fail",
		Operation:      JobOperationCreate,
		SourceImageRef: "ubuntu:22.04",
		InstanceType:   "cubebox",
		Status:         JobStatusRunning,
		Phase:          JobPhaseBuildingExt4,
	}).Error)

	p, err := webhook.NewBufferedPublisher(4)
	require.NoError(t, err)
	SetWebhookPublisher(p)
	defer SetWebhookPublisher(nil)

	require.NoError(t, failTemplateImageJob(context.Background(), "job-fail", map[string]any{
		"phase":         JobPhaseBuildingExt4,
		"error_message": "builder exploded",
	}))

	job, err := getTemplateImageJobRecordByID(context.Background(), "job-fail")
	require.NoError(t, err)
	require.Equal(t, JobStatusFailed, job.Status)
	require.Equal(t, "builder exploded", job.ErrorMessage)

	captured := <-p.Events()
	data, err := webhook.MapToProtocol(captured)
	require.NoError(t, err)
	ev, err := webhook.Encode(captured, data)
	require.NoError(t, err)
	require.Equal(t, webhook.TypeTemplateBuildFailed, ev.Type())

	var body map[string]any
	require.NoError(t, json.Unmarshal(ev.Body(), &body))
	require.Equal(t, "job-fail", body["sandbox_build_id"])
	require.Equal(t, "tmpl-fail", body["sandbox_template_id"])
	eventData, ok := body["event_data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "builder exploded", eventData["error_message"])
	require.Equal(t, "ubuntu:22.04", eventData["source_image_ref"])
	require.Equal(t, "remote:cubebox", eventData["build_mode"])
}

// build.started and build.failed must describe the same build: the mode string
// on the failure is rebuilt from the job row, so it has to match what the
// forwarding path put on the start event.
func TestTemplateBuildModeMirrorsStartedEvent(t *testing.T) {
	require.Equal(t, "remote:cubebox",
		templateBuildMode(&models.TemplateImageJob{Operation: JobOperationCreate, InstanceType: "cubebox"}))
	require.Equal(t, "remote",
		templateBuildMode(&models.TemplateImageJob{Operation: JobOperationCreate}))
	require.Equal(t, "redo:cubebox",
		templateBuildMode(&models.TemplateImageJob{Operation: JobOperationRedo, InstanceType: "cubebox"}))
}
