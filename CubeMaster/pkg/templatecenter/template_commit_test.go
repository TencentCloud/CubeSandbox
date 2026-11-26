// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"gorm.io/gorm"
)

func newCommitFixtureRequests() (*types.CreateCubeSandboxReq, *types.CreateCubeSandboxReq) {
	createReq := &types.CreateCubeSandboxReq{
		Request:      &types.Request{RequestID: "req-1"},
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:  cubeboxv1.NetworkType_tap.String(),
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateID:      "tpl-commit-1",
			constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
		},
	}
	storedReq := *createReq
	storedReq.Request = nil
	return createReq, &storedReq
}

func TestNewCommitTemplateImageJobRecordPersistsIdentityFields(t *testing.T) {
	createReq, storedReq := newCommitFixtureRequests()
	requestSnapshot, err := marshalTemplateCommitJobRequest(storedReq)
	if err != nil {
		t.Fatalf("marshalTemplateCommitJobRequest failed: %v", err)
	}
	record := newCommitTemplateImageJobRecord(
		"job-commit-1",
		"req-123",
		"tpl-commit-1",
		"sb-source",
		"node-a",
		"10.0.0.1",
		createReq,
		requestSnapshot,
		2,
		"job-prev",
	)
	if record.RequestID != "req-123" {
		t.Fatalf("RequestID=%q, want %q", record.RequestID, "req-123")
	}
	if record.SandboxID != "sb-source" {
		t.Fatalf("SandboxID=%q, want sb-source", record.SandboxID)
	}
	if record.Operation != JobOperationCommit {
		t.Fatalf("Operation=%q, want %q", record.Operation, JobOperationCommit)
	}
	if record.NodeID != "node-a" || record.NodeIP != "10.0.0.1" {
		t.Fatalf("Node identity not persisted: %#v", record)
	}
	if record.AttemptNo != 2 || record.RetryOfJobID != "job-prev" {
		t.Fatalf("Attempt metadata not persisted: %+v", record)
	}
	if record.TemplateSpecFingerprint == "" {
		t.Fatal("TemplateSpecFingerprint should be populated for commit jobs")
	}
	if record.TemplateSpecFingerprint != buildCommitTemplateSpecFingerprintFromSnapshot(requestSnapshot) {
		t.Fatalf("TemplateSpecFingerprint=%q, want fingerprint from request snapshot", record.TemplateSpecFingerprint)
	}
	if record.Status != JobStatusPending || record.Phase != JobPhaseSnapshotting {
		t.Fatalf("unexpected initial state: status=%q phase=%q", record.Status, record.Phase)
	}
}

func TestTemplateCommitWorkerSettlesDatabaseErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failPhase  string
		unreadable bool
		captures   int
		cleanups   int
	}{
		{"claim", JobPhaseSnapshotting, false, 0, 0},
		{"register while owned", JobPhaseRegistering, false, 1, 1},
		{"register with unknown ownership", JobPhaseRegistering, true, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := snapshotReservationTestDB(t)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			createReq, storedReq := newCommitFixtureRequests()
			job := newCommitTemplateImageJobRecord("job", "req", "tpl-commit-1", "sb", "node", "127.0.0.1", createReq, "{}", 1, "")
			require.NoError(t, db.Create(job).Error)
			require.NoError(t, db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_commit_phase BEFORE UPDATE ON %s
				WHEN NEW.status = '%s' AND NEW.phase = '%s'
				BEGIN SELECT RAISE(FAIL, 'injected phase failure'); END`, constants.TemplateImageJobTableName, JobStatusRunning, tc.failPhase)).Error)
			patches := gomonkey.NewPatches()
			t.Cleanup(patches.Reset)
			var captures, cleanups int
			patches.ApplyFunc(cubelet.GetCubeletAddr, func(ip string) string { return ip })
			patches.ApplyFunc(cubelet.CommitSandbox, func(_ context.Context, ip string, _ *cubeboxv1.CommitSandboxRequest) (*cubeboxv1.CommitSandboxResponse, error) {
				captures++
				return okCommit(ip)
			})
			patches.ApplyFunc(cubelet.CleanupTemplate, func(context.Context, string, *cubeboxv1.CleanupTemplateRequest) (*cubeboxv1.CleanupTemplateResponse, error) {
				cleanups++
				return successCleanup(), nil
			})
			if tc.unreadable {
				patches.ApplyFunc(getTemplateImageJobRecordByID, func(context.Context, string) (*models.TemplateImageJob, error) {
					return nil, errors.New("ownership read failed")
				})
			}
			runTemplateCommitJob(t.Context(), job.JobID, job.SandboxID, job.NodeID, job.NodeIP, createReq, storedReq)
			require.NoError(t, db.First(job, job.ID).Error)
			require.Equal(t, JobStatusFailed, job.Status)
			require.Contains(t, job.ErrorMessage, "injected phase failure")
			require.Equal(t, tc.captures, captures)
			require.Equal(t, tc.cleanups, cleanups)
		})
	}
}

func TestIsDuplicateKeyErrorClassifiesMySQLAndGormErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"gorm duplicated key sentinel", gorm.ErrDuplicatedKey, true},
		{"raw mysql 1062 text", errors.New("Error 1062 (23000): Duplicate entry '' for key 'idx_x'"), true},
		{"just contains 1062", errors.New("driver: 1062 conflict"), true},
		{"contains Duplicate entry", errors.New("Duplicate entry 'x' for key 'idx_y'"), true},
		{"postgres unique constraint", errors.New("ERROR: duplicate key value violates unique constraint \"uniq_cube_snapshot_id\" (SQLSTATE 23505)"), true},
		{"sqlite UNIQUE constraint", errors.New("UNIQUE constraint failed: t_cube_snapshot.snapshot_id"), true},
		{"unrelated error", errors.New("some other failure"), false},
	}
	for _, tc := range tests {
		if got := isDuplicateKeyError(tc.err); got != tc.want {
			t.Fatalf("%s: isDuplicateKeyError(%v)=%v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestBuildCommitFailureMessageNeverEmpty pins the regression that produced
// FAILED jobs with empty error_message: when cubelet returned a non-success Ret
// without filling RetMsg (or returned nil Ret entirely), the previous code path
// stored "" in t_cube_template_image_job, defeating post-mortem.
func TestBuildCommitFailureMessageNeverEmpty(t *testing.T) {
	tests := []struct {
		name      string
		rsp       *cubeboxv1.CommitSandboxResponse
		wantParts []string
	}{
		{
			name:      "nil response",
			rsp:       nil,
			wantParts: []string{"commit sandbox failed", "nil response"},
		},
		{
			name:      "nil Ret",
			rsp:       &cubeboxv1.CommitSandboxResponse{},
			wantParts: []string{"commit sandbox failed", "empty Ret"},
		},
		{
			name: "non-success code with empty retMsg",
			rsp: &cubeboxv1.CommitSandboxResponse{
				Ret: &errorcodev1.Ret{RetCode: 500, RetMsg: ""},
			},
			wantParts: []string{"commit sandbox failed", "retCode=500", "empty retMsg"},
		},
		{
			name: "non-success code with retMsg",
			rsp: &cubeboxv1.CommitSandboxResponse{
				Ret: &errorcodev1.Ret{RetCode: 500, RetMsg: "snapshot timed out"},
			},
			wantParts: []string{"commit sandbox failed", "retCode=500", "snapshot timed out"},
		},
		{
			name: "non-success code with whitespace-only retMsg is treated as empty",
			rsp: &cubeboxv1.CommitSandboxResponse{
				Ret: &errorcodev1.Ret{RetCode: 130599, RetMsg: "   \t\n"},
			},
			wantParts: []string{"commit sandbox failed", "retCode=130599", "empty retMsg"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildCommitFailureMessage(tc.rsp)
			if got == "" {
				t.Fatal("error_message must never be empty")
			}
			for _, part := range tc.wantParts {
				if !strings.Contains(got, part) {
					t.Fatalf("missing %q in %q", part, got)
				}
			}
		})
	}
}

func TestSubmitTemplateCommitRejectsEmptyRequestID(t *testing.T) {
	// Pretend the store is initialised so we exercise the requestID guard
	// instead of the "store not initialised" early return.
	origDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = origDB }()

	_, err := SubmitTemplateCommit(context.Background(), "   ", "sb-1", "node-a", "10.0.0.1", "tpl-out", nil)
	if err == nil || !strings.Contains(err.Error(), "requestID is required") {
		t.Fatalf("expected requestID guard error, got %v", err)
	}
}

func TestPrepareTemplateCommitRequestLoadsSandboxSpec(t *testing.T) {
	origLoad := loadSandboxCreateRequestFn
	defer func() { loadSandboxCreateRequestFn = origLoad }()

	loadSandboxCreateRequestFn = func(ctx context.Context, sandboxID string) (*types.CreateCubeSandboxReq, error) {
		if sandboxID != "sb-1" {
			t.Fatalf("sandboxID=%q", sandboxID)
		}
		return &types.CreateCubeSandboxReq{
			Request: &types.Request{RequestID: "old-request"},
			Annotations: map[string]string{
				constants.CubeAnnotationAppSnapshotTemplateID: "tpl-old",
			},
			InstanceType: "cubebox",
		}, nil
	}

	got, err := prepareTemplateCommitRequest(context.Background(), "req-new", "sb-1", "tpl-new", nil)
	if err != nil {
		t.Fatalf("prepareTemplateCommitRequest: %v", err)
	}
	if got.RequestID != "req-new" {
		t.Fatalf("requestID=%q", got.RequestID)
	}
	if got.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] != "tpl-new" {
		t.Fatalf("template annotation=%q", got.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	}
}

func TestPrepareTemplateCommitRequestUsesExplicitOverride(t *testing.T) {
	origLoad := loadSandboxCreateRequestFn
	defer func() { loadSandboxCreateRequestFn = origLoad }()
	loadCalls := 0
	loadSandboxCreateRequestFn = func(context.Context, string) (*types.CreateCubeSandboxReq, error) {
		loadCalls++
		return &types.CreateCubeSandboxReq{Annotations: map[string]string{}}, nil
	}

	override := &types.CreateCubeSandboxReq{
		InstanceType: "custom",
		Annotations:  map[string]string{},
	}
	got, err := prepareTemplateCommitRequest(context.Background(), "req-1", "sb-1", "tpl-new", override)
	if err != nil {
		t.Fatalf("prepareTemplateCommitRequest: %v", err)
	}
	if got.InstanceType != "custom" || got.RequestID != "req-1" {
		t.Fatalf("unexpected request: %#v", got)
	}
	if got == override {
		t.Fatal("explicit override should be cloned before normalization")
	}
	if loadCalls != 1 {
		t.Fatalf("sandbox request resolver calls=%d, want 1", loadCalls)
	}
}

func TestPrepareTemplateCommitRequestInheritsTemplateManagedQos(t *testing.T) {
	origLoad := loadSandboxCreateRequestFn
	defer func() { loadSandboxCreateRequestFn = origLoad }()

	loadSandboxCreateRequestFn = func(ctx context.Context, sandboxID string) (*types.CreateCubeSandboxReq, error) {
		if sandboxID != "sb-1" {
			t.Fatalf("sandboxID=%q", sandboxID)
		}
		return &types.CreateCubeSandboxReq{
			Annotations: map[string]string{
				constants.CubeAnnotationsNetWork: "network-qos",
				constants.CubeAnnotationsBlkQos:  "block-qos",
				constants.CubeAnnotationsFSQos:   "fs-qos",
			},
		}, nil
	}

	// This models the sanitized create_request returned by the public API:
	// template-managed network and block I/O annotations are absent, while the
	// legacy caller-managed filesystem annotation remains visible and mutable.
	override := &types.CreateCubeSandboxReq{
		InstanceType: "custom",
		Annotations: map[string]string{
			constants.CubeAnnotationsFSQos: "override-fs-qos",
		},
	}
	got, err := prepareTemplateCommitRequest(context.Background(), "req-1", "sb-1", "tpl-new", override)
	if err != nil {
		t.Fatalf("prepareTemplateCommitRequest: %v", err)
	}
	if got.Annotations[constants.CubeAnnotationsNetWork] != "network-qos" ||
		got.Annotations[constants.CubeAnnotationsBlkQos] != "block-qos" ||
		got.Annotations[constants.CubeAnnotationsFSQos] != "override-fs-qos" {
		t.Fatalf("commit annotations=%v, want inherited template-managed qos and caller-managed filesystem qos", got.Annotations)
	}
	if got.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] != "tpl-new" {
		t.Fatalf("template annotation=%q, want tpl-new", got.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	}
}

// TestPrepareTemplateCommitRequestScrubsInheritedAlias verifies the commit
// path strips the source template's alias annotation and label.
func TestPrepareTemplateCommitRequestScrubsInheritedAlias(t *testing.T) {
	origLoad := loadSandboxCreateRequestFn
	defer func() { loadSandboxCreateRequestFn = origLoad }()

	loadSandboxCreateRequestFn = func(ctx context.Context, sandboxID string) (*types.CreateCubeSandboxReq, error) {
		return &types.CreateCubeSandboxReq{
			Request: &types.Request{RequestID: "old-request"},
			Annotations: map[string]string{
				constants.CubeAnnotationAppSnapshotTemplateID:    "tpl-old",
				constants.CubeAnnotationAppSnapshotTemplateAlias: "source-alias",
			},
			Labels: map[string]string{
				constants.CubeAnnotationAppSnapshotTemplateAlias: "source-alias",
			},
			InstanceType: "cubebox",
		}, nil
	}

	got, err := prepareTemplateCommitRequest(context.Background(), "req-new", "sb-1", "tpl-new", nil)
	if err != nil {
		t.Fatalf("prepareTemplateCommitRequest: %v", err)
	}
	if _, ok := got.Annotations[constants.CubeAnnotationAppSnapshotTemplateAlias]; ok {
		t.Fatal("commit request must not carry the source alias annotation")
	}
	if _, ok := got.Labels[constants.CubeAnnotationAppSnapshotTemplateAlias]; ok {
		t.Fatal("commit request must not carry the source alias label")
	}
	if got.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] != "tpl-new" {
		t.Fatalf("template annotation=%q", got.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	}
}

func TestPrepareTemplateCommitRequestPropagatesResolverError(t *testing.T) {
	origLoad := loadSandboxCreateRequestFn
	defer func() { loadSandboxCreateRequestFn = origLoad }()
	loadSandboxCreateRequestFn = func(context.Context, string) (*types.CreateCubeSandboxReq, error) {
		return nil, fmt.Errorf("base template lookup failed: %w", ErrTemplateNotFound)
	}

	for _, override := range []*types.CreateCubeSandboxReq{nil, {InstanceType: "custom"}} {
		_, err := prepareTemplateCommitRequest(context.Background(), "req-1", "sb-legacy", "tpl-new", override)
		if !errors.Is(err, ErrTemplateNotFound) {
			t.Fatalf("override=%v: expected ErrTemplateNotFound, got %v", override != nil, err)
		}
	}
}

func TestPrepareTemplateCommitRequestRejectsMissingSandboxSpec(t *testing.T) {
	origLoad := loadSandboxCreateRequestFn
	defer func() { loadSandboxCreateRequestFn = origLoad }()
	loadSandboxCreateRequestFn = func(context.Context, string) (*types.CreateCubeSandboxReq, error) {
		return nil, nil
	}

	_, err := prepareTemplateCommitRequest(
		context.Background(),
		"req-1",
		"sb-missing",
		"tpl-new",
		&types.CreateCubeSandboxReq{InstanceType: "custom"},
	)
	if err == nil || !strings.Contains(err.Error(), "sandbox create request is empty") {
		t.Fatalf("expected missing sandbox spec error, got %v", err)
	}
}
