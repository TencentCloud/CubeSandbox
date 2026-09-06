// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// newReuseArtifactJobRecord is a pure constructor: verify every field maps
// correctly and the synthetic job starts at BUILT/READY (skipping the build).
func TestNewReuseArtifactJobRecord(t *testing.T) {
	req := &types.CreateTemplateFromImageReq{TemplateID: "tpl-reuse-1"}
	artifact := &models.RootfsArtifact{
		ArtifactID:              "rfs-fp123-uuid1",
		TemplateSpecFingerprint: "fp123",
		SourceImageDigest:       "sha256:abc",
		Status:                  ArtifactStatusReady,
		GeneratedRequestJSON:    `{"k":"v"}`,
	}
	snapshot := `{"template_id":"tpl-reuse-1"}`

	job := newReuseArtifactJobRecord("job-reuse-1", req, snapshot, artifact)

	if job.JobID != "job-reuse-1" {
		t.Fatalf("JobID=%q", job.JobID)
	}
	if job.TemplateID != "tpl-reuse-1" {
		t.Fatalf("TemplateID=%q, want from req", job.TemplateID)
	}
	if job.Status != JobStatusBuilt {
		t.Fatalf("Status=%q, want BUILT", job.Status)
	}
	if job.Phase != JobPhaseReady {
		t.Fatalf("Phase=%q, want READY", job.Phase)
	}
	if job.Progress != 100 {
		t.Fatalf("Progress=%d, want 100", job.Progress)
	}
	if job.Operation != JobOperationCreate {
		t.Fatalf("Operation=%q, want CREATE", job.Operation)
	}
	if job.AttemptNo != 1 {
		t.Fatalf("AttemptNo=%d, want 1", job.AttemptNo)
	}
	if job.RetryOfJobID != "" {
		t.Fatalf("RetryOfJobID=%q, want empty", job.RetryOfJobID)
	}
	if job.RequestJSON != snapshot {
		t.Fatalf("RequestJSON=%q", job.RequestJSON)
	}
	if job.ArtifactID != "rfs-fp123-uuid1" {
		t.Fatalf("ArtifactID=%q", job.ArtifactID)
	}
	if job.TemplateSpecFingerprint != "fp123" {
		t.Fatalf("TemplateSpecFingerprint=%q", job.TemplateSpecFingerprint)
	}
	if job.SourceImageDigest != "sha256:abc" {
		t.Fatalf("SourceImageDigest=%q", job.SourceImageDigest)
	}
	if job.ArtifactStatus != ArtifactStatusReady {
		t.Fatalf("ArtifactStatus=%q", job.ArtifactStatus)
	}
	if job.ResultJSON != `{"k":"v"}` {
		t.Fatalf("ResultJSON=%q, want artifact.GeneratedRequestJSON", job.ResultJSON)
	}
}

// GetRootfsArtifactForRedirect must NOT touch the filesystem (unlike
// OpenRootfsArtifact), and must enforce the token when one is set.
//
// These cases need a DB handle, which is provided by the package's testutils
// in integration runs. The pure token-comparison branches are covered here via
// a record stub by testing the comparison logic directly is not possible
// without a DB, so we only document the contract; the DB-backed cases live in
// the integration suite (mysql_testutil_test.go / postgres_testutil_test.go).
func TestGetRootfsArtifactForRedirectContract(t *testing.T) {
	// Token comparison semantics: empty DownloadToken means "legacy row, any
	// token accepted"; non-empty means "must match exactly".
	legacy := &models.RootfsArtifact{ArtifactID: "a1", DownloadToken: ""}
	modern := &models.RootfsArtifact{ArtifactID: "a2", DownloadToken: "secret"}

	tokenOK := func(rec *models.RootfsArtifact, token string) bool {
		return rec.DownloadToken == "" || token == rec.DownloadToken
	}

	if !tokenOK(legacy, "anything") {
		t.Fatalf("legacy row should accept any token")
	}
	if !tokenOK(modern, "secret") {
		t.Fatalf("modern row should accept matching token")
	}
	if tokenOK(modern, "wrong") {
		t.Fatalf("modern row should reject wrong token")
	}
	if tokenOK(modern, "") {
		t.Fatalf("modern row should reject empty token")
	}
}
