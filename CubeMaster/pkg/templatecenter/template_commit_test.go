// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"strings"
	"testing"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
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

	tests := []struct {
		name string
		req  *types.CreateCubeSandboxReq
	}{
		{"nil request", nil},
		{"missing Request envelope", &types.CreateCubeSandboxReq{}},
		{
			name: "empty request id",
			req: &types.CreateCubeSandboxReq{
				Request: &types.Request{RequestID: "   "},
			},
		},
	}
	for _, tc := range tests {
		_, err := SubmitTemplateCommit(context.Background(), "sb-1", "node-a", "10.0.0.1", tc.req)
		if err == nil || !strings.Contains(err.Error(), "requestID is required") {
			t.Fatalf("%s: expected requestID guard error, got %v", tc.name, err)
		}
	}
}
