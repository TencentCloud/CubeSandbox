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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
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

func runCheckpointFixture(t *testing.T, backend, failure string) error {
	t.Helper()
	p := gomonkey.NewPatches()
	defer p.Reset()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	var failedJob error
	if failure == "cancel-at-claim" {
		cancel()
	}
	p.ApplyFunc(cubelet.GetCubeletAddr, func(ip string) string { return ip })
	// The claimed job reads the snapshot record committed with its job row.
	p.ApplyFunc(getSnapshotRecord, func(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
		require.NoError(t, ctx.Err(), "claimed job must outlive caller cancellation")
		require.Equal(t, "snap-new", snapshotID)
		return &models.SnapshotRecord{SnapshotID: snapshotID, Backend: backend}, nil
	})
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
	p.ApplyFunc(cubelet.CommitSandbox, func(ctx context.Context, _ string, req *cubebox.CommitSandboxRequest) (*cubebox.CommitSandboxResponse, error) {
		require.Equal(t, backend, req.GetBackend())
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
	p.ApplyFunc(updateSnapshotFields, func(_ context.Context, _ string, m map[string]any) error {
		if m["status"] == StatusReady && failure == "snapshot" {
			return errors.New("snapshot")
		}
		return nil
	})
	p.ApplyFunc(updateTemplateImageJob, func(_ context.Context, _ string, m map[string]any) error {
		if m["status"] == JobStatusFailed {
			failedJob = errors.New(m["error_message"].(string))
		}
		if m["status"] == JobStatusReady && failure == "ready" {
			return errors.New("ready")
		}
		return nil
	})
	p.ApplyFunc(setTemplateLocalityCache, func(string, []ReplicaStatus) {})
	p.ApplyFunc(registerTemplateReplicaForSnapshot, func(string, string, int64) {})
	p.ApplyFunc(setTemplateRequestCache, func(_ string, req *sandboxtypes.CreateCubeSandboxReq) error {
		require.Equal(t, "snapshot-name", req.Annotations[constants.CubeAnnotationAppSnapshotTemplateAlias])
		return errors.New("warn-only cache")
	})
	// Exercise the real failure cleanup, whose successful FAILED writes can
	// return nil. Only its external RPC/database/cache dependencies are stubbed.
	p.ApplyFunc(cubelet.CleanupTemplate, func(context.Context, string, *cubebox.CleanupTemplateRequest) (*cubebox.CleanupTemplateResponse, error) {
		return &cubebox.CleanupTemplateResponse{Ret: &ec.Ret{RetCode: ec.ErrorCode_Success}}, nil
	})
	p.ApplyFunc(deleteReplicasByTemplateID, func(context.Context, string) error { return nil })
	p.ApplyFunc(invalidateTemplateCaches, func(string) {})
	p.ApplyFunc(finalizeSnapshotJobByID, func(context.Context, string) (*sandboxtypes.TemplateImageJobInfo, error) {
		if failedJob != nil {
			return nil, failedJob
		}
		if failure == "query" {
			return nil, errors.New("query")
		}
		return &sandboxtypes.TemplateImageJobInfo{Status: JobStatusReady}, nil
	})
	req := &sandboxtypes.CreateCubeSandboxReq{Request: &sandboxtypes.Request{RequestID: "req"}, InstanceType: "cubebox", Backend: backend, Annotations: map[string]string{constants.CubeAnnotationAppSnapshotTemplateID: "original-template"}}
	_, err := executeSnapshotCreateJob(parent, &sandboxtypes.TemplateImageJobInfo{JobID: "job", TemplateID: "snap-new", Status: JobStatusPending}, req, "sb", "node", "127.0.0.1", "snapshot-name")
	require.Equal(t, "original-template", req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	return err
}
func TestCheckpointFinalBoundary(t *testing.T) {
	for _, backend := range []string{"xfs", "s3"} {
		for _, failure := range []string{"", "commit", "replica", "snapshot", "ready", "query", "unclaimed", "uploading", "cancel-after-claim", "cancel-during", "cancel-failure", "cancel-at-claim"} {
			t.Run(backend+failure, func(t *testing.T) {
				p, e := webhook.NewBufferedPublisher(2)
				require.NoError(t, e)
				SetWebhookPublisher(p)
				defer SetWebhookPublisher(nil)
				e = runCheckpointFixture(t, backend, failure)
				switch failure {
				case "", "unclaimed", "uploading", "cancel-after-claim", "cancel-during":
					require.NoError(t, e)
				case "cancel-at-claim":
					require.ErrorIs(t, e, context.Canceled)
				case "cancel-failure":
					require.EqualError(t, e, "local commit failed after caller cancellation")
				default:
					require.EqualError(t, e, failure)
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
				_, e = executeSnapshotCreateJob(context.Background(), &sandboxtypes.TemplateImageJobInfo{Status: JobStatusReady}, nil, "sb", "node", "ip", "")
				require.NoError(t, e)
				require.Empty(t, p.Events())
			})
		}
	}
}
func TestCheckpointRuntimeHTTP(t *testing.T) {
	r, bodies := webhooktest.Runtime(t, webhook.TypeCheckpointed)
	SetWebhookPublisher(r.Publisher())
	defer SetWebhookPublisher(nil)
	require.NoError(t, runCheckpointFixture(t, "s3", ""))
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
func TestCheckpointFailureIsolation(t *testing.T) {
	webhooktest.FaultPublishers(t, func(t *testing.T, p webhook.Publisher) {
		SetWebhookPublisher(p)
		defer SetWebhookPublisher(nil)
		require.NoError(t, runCheckpointFixture(t, "s3", ""))
	})
}

func TestCheckpointUploadResult(t *testing.T) {
	for _, terminal := range []string{constants.RemoteStatusReady, constants.RemoteStatusFailed} {
		t.Run(terminal, func(t *testing.T) {
			p, err := webhook.NewBufferedPublisher(4)
			require.NoError(t, err)
			SetWebhookPublisher(p)
			defer SetWebhookPublisher(nil)
			require.NoError(t, runCheckpointFixture(t, "s3", "uploading"))
			db := webhooktest.SnapshotDB(t)
			// Seed the persisted READY result of the controlled storage dependency.
			require.NoError(t, db.Create(&models.SnapshotRecord{SnapshotID: "snap-new", OriginNodeIP: "127.0.0.1", Backend: "s3", Status: "READY", RemoteStatus: constants.RemoteStatusInProgress}).Error)
			webhooktest.ReconcileUpload(t, db, constants.SnapshotTableName, "snap-new", terminal, p)
			_, err = executeSnapshotCreateJob(context.Background(), &sandboxtypes.TemplateImageJobInfo{Status: JobStatusReady}, nil, "sb", "node", "ip", "")
			require.NoError(t, err)
			require.Len(t, p.Events(), 1)
		})
	}
}

func TestCheckpointDisabledRuntimeSkipsPreparation(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		cfg := config.DefaultWebhookConf()
		cfg.Enabled, cfg.Invalid = invalid, invalid
		runtime, err := webhook.NewRuntime(context.Background(), cfg)
		if invalid {
			require.ErrorIs(t, err, config.ErrWebhookConfig)
		} else {
			require.NoError(t, err)
		}
		SetWebhookPublisher(runtime.Publisher())
		before := webhooktest.CaptureAccepted(t)
		patches := gomonkey.NewPatches()
		patches.ApplyFunc(webhook.PrepareLifecycle, func(*sandboxtypes.CreateCubeSandboxReq) (string, webhook.LifecycleFields) {
			panic("disabled checkpoint fields")
		})
		patches.ApplyFunc(webhook.Capture, func(webhook.CaptureInput) (*webhook.CapturedEvent, error) { panic("disabled checkpoint capture") })
		require.NoError(t, runCheckpointFixture(t, "xfs", ""))
		patches.Reset()
		require.Equal(t, before, webhooktest.CaptureAccepted(t))
		SetWebhookPublisher(nil)
		runtime.Shutdown(context.Background())
	}
}
