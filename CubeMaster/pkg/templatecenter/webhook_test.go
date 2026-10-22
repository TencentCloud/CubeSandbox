// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/internal/webhooktest"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/webhook"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	ec "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"testing"
	"time"
)

func p4Checkpoint(t *testing.T, backend, failure string) error {
	t.Helper()
	p := gomonkey.NewPatches()
	defer p.Reset()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	if failure == "cancel-at-claim" {
		cancel()
	}
	p.ApplyFunc(cubelet.GetCubeletAddr, func(ip string) string { return ip })
	p.ApplyFunc(claimSnapshotJobExecution, func(ctx context.Context, _ string, _ string, _ int32) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if failure == "cancel-after-claim" {
			cancel()
		}
		return failure != "unclaimed", nil
	})
	p.ApplyFunc(resolveExistingSnapshotJobByID, func(context.Context, string) (*sandboxtypes.TemplateImageJobInfo, error) {
		return &sandboxtypes.TemplateImageJobInfo{Status: JobStatusReady}, nil
	})
	p.ApplyFunc(cubelet.CommitSandbox, func(ctx context.Context, _ string, _ *cubebox.CommitSandboxRequest) (*cubebox.CommitSandboxResponse, error) {
		if failure == "cancel-during" || failure == "cancel-failure" {
			cancel()
		}
		require.NoError(t, ctx.Err(), "claimed job must outlive caller cancellation")
		if failure == "cancel-after-claim" || failure == "cancel-during" || failure == "cancel-failure" {
			require.ErrorIs(t, parent.Err(), context.Canceled)
		}
		if failure == "cancel-failure" {
			return nil, errors.New("local commit failed after caller cancellation")
		}
		if failure == "commit" {
			return nil, errors.New("commit")
		}
		uuids := ""
		if failure == "uploading" {
			uuids = "export-pending"
		}
		return &cubebox.CommitSandboxResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}, SnapshotPath: "/snapshot", RemoteUuids: uuids}, nil
	})
	p.ApplyFunc(UpsertReplica, func(context.Context, string, string, ReplicaStatus) error {
		if failure == "replica" {
			return errors.New("replica")
		}
		return nil
	})
	p.ApplyFunc(updateSnapshotFields, func(context.Context, string, map[string]any) error {
		if failure == "snapshot" {
			return errors.New("snapshot")
		}
		return nil
	})
	p.ApplyFunc(updateTemplateImageJob, func(_ context.Context, _ string, m map[string]any) error {
		if m["status"] == JobStatusReady && failure == "ready" {
			return errors.New("ready")
		}
		return nil
	})
	p.ApplyFunc(setTemplateLocalityCache, func(string, []ReplicaStatus) {})
	p.ApplyFunc(registerTemplateReplicaForSnapshot, func(string, string, int64) {})
	p.ApplyFunc(setTemplateRequestCache, func(string, *sandboxtypes.CreateCubeSandboxReq) error { return errors.New("warn-only cache") })
	p.ApplyFunc(failSnapshotCreateJob, func(_ context.Context, _, _, _, _ string, _ *cubebox.CommitSandboxResponse, e error, _ string) error {
		return e
	})
	p.ApplyFunc(finalizeSnapshotJobByID, func(context.Context, string) (*sandboxtypes.TemplateImageJobInfo, error) {
		if failure == "query" {
			return nil, errors.New("query")
		}
		return &sandboxtypes.TemplateImageJobInfo{Status: JobStatusReady}, nil
	})
	req := &sandboxtypes.CreateCubeSandboxReq{Request: &sandboxtypes.Request{RequestID: "req"}, InstanceType: "cubebox", Backend: backend, Annotations: map[string]string{constants.CubeAnnotationAppSnapshotTemplateID: "original-template"}, WebhookMetadata: webhook.UserMetadata(map[string]string{"user_token": "kept"})}
	_, err := executeSnapshotCreateJob(parent, &sandboxtypes.TemplateImageJobInfo{JobID: "job", TemplateID: "snap-new", Status: JobStatusPending}, req, "sb", "node", "127.0.0.1")
	require.Equal(t, "original-template", req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	return err
}
func TestP4CheckpointFinalBoundary(t *testing.T) {
	for _, backend := range []string{"xfs", "s3"} {
		for _, failure := range []string{"", "commit", "replica", "snapshot", "ready", "query", "unclaimed", "uploading", "cancel-after-claim", "cancel-during", "cancel-failure", "cancel-at-claim"} {
			t.Run(backend+failure, func(t *testing.T) {
				p, e := webhook.NewBufferedPublisher(2)
				require.NoError(t, e)
				SetWebhookPublisher(p)
				defer SetWebhookPublisher(nil)
				e = p4Checkpoint(t, backend, failure)
				if failure == "" || failure == "unclaimed" || failure == "uploading" || failure == "cancel-after-claim" || failure == "cancel-during" {
					require.NoError(t, e)
				} else {
					require.Error(t, e)
				}
				if failure != "" && failure != "query" && failure != "uploading" && failure != "cancel-after-claim" && failure != "cancel-during" {
					require.Empty(t, p.Events())
					return
				}
				select {
				case c := <-p.Events():
					d, e := webhook.MapToProtocol(c)
					require.NoError(t, e)
					ev, e := webhook.Encode(c, d)
					require.NoError(t, e)
					var b map[string]any
					require.NoError(t, json.Unmarshal(ev.Body(), &b))
					require.Equal(t, "original-template", b["sandbox_template_id"])
					require.Equal(t, webhook.TypeCheckpointed, ev.Type())
				default:
					t.Fatal("missing checkpoint")
				}
				_, e = executeSnapshotCreateJob(context.Background(), &sandboxtypes.TemplateImageJobInfo{Status: JobStatusReady}, nil, "sb", "node", "ip")
				require.NoError(t, e)
				require.Empty(t, p.Events())
			})
		}
	}
}
func TestP4CheckpointRedisHTTP(t *testing.T) {
	r, bodies := webhooktest.Runtime(t, 11, webhook.TypeCheckpointed)
	SetWebhookPublisher(r.Publisher())
	defer SetWebhookPublisher(nil)
	require.NoError(t, p4Checkpoint(t, "s3", ""))
	select {
	case raw := <-bodies:
		var b map[string]any
		require.NoError(t, json.Unmarshal(raw, &b))
		require.Equal(t, webhook.TypeCheckpointed, b["type"])
		require.Equal(t, "original-template", b["sandbox_template_id"])
	case <-time.After(8 * time.Second):
		t.Fatal("checkpoint pipeline timeout")
	}
}
func TestP4CheckpointFailureIsolation(t *testing.T) {
	webhooktest.FaultPublishers(t, func(t *testing.T, p webhook.Publisher) {
		SetWebhookPublisher(p)
		defer SetWebhookPublisher(nil)
		require.NoError(t, p4Checkpoint(t, "s3", ""))
	})
}

func TestP4CheckpointUploadResult(t *testing.T) {
	for _, terminal := range []string{constants.RemoteStatusReady, constants.RemoteStatusFailed} {
		t.Run(terminal, func(t *testing.T) {
			p, err := webhook.NewBufferedPublisher(4)
			require.NoError(t, err)
			SetWebhookPublisher(p)
			defer SetWebhookPublisher(nil)
			require.NoError(t, p4Checkpoint(t, "s3", "uploading"))
			db := webhooktest.SnapshotDB(t)
			// Seed the persisted READY result of the controlled storage dependency.
			require.NoError(t, db.Create(&models.SnapshotRecord{SnapshotID: "snap-new", OriginNodeIP: "127.0.0.1", Backend: "s3", Status: "READY", RemoteStatus: constants.RemoteStatusInProgress}).Error)
			webhooktest.ReconcileUpload(t, db, constants.SnapshotTableName, "snap-new", terminal, p)
			_, err = executeSnapshotCreateJob(context.Background(), &sandboxtypes.TemplateImageJobInfo{Status: JobStatusReady}, nil, "sb", "node", "ip")
			require.NoError(t, err)
			require.Len(t, p.Events(), 1)
		})
	}
}
