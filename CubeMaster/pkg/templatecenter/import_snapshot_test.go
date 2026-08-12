// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

func TestComputeSpecDir(t *testing.T) {
	t.Parallel()

	spec := func(resources ...*sandboxtypes.Resource) *sandboxtypes.CreateCubeSandboxReq {
		req := &sandboxtypes.CreateCubeSandboxReq{}
		for _, r := range resources {
			req.Containers = append(req.Containers, &sandboxtypes.Container{Resources: r})
		}
		return req
	}

	cases := []struct {
		name string
		req  *sandboxtypes.CreateCubeSandboxReq
		want string
	}{
		{"whole units", spec(&sandboxtypes.Resource{Cpu: "1", Mem: "1Gi"}), "1C1024M"},
		{"millicpu rounds up", spec(&sandboxtypes.Resource{Cpu: "500m", Mem: "512Mi"}), "1C512M"},
		{"mem truncates to MiB", spec(&sandboxtypes.Resource{Cpu: "2", Mem: "1000000000"}), "2C953M"},
		{"multi-container uses the first, matching cubelet's inference", spec(
			&sandboxtypes.Resource{Cpu: "500m", Mem: "512Mi"},
			&sandboxtypes.Resource{Cpu: "1500m", Mem: "512Mi"},
		), "1C512M"},
		{"unparsable ignored", spec(&sandboxtypes.Resource{Cpu: "bogus", Mem: ""}), "0C0M"},
		{"nil resources", spec(nil), "0C0M"},
		{"no containers", spec(), "0C0M"},
	}
	for _, tc := range cases {
		if got := computeSpecDir(tc.req); got != tc.want {
			t.Errorf("%s: computeSpecDir=%q, want %q", tc.name, got, tc.want)
		}
	}
}

func importTestSpec() *sandboxtypes.CreateCubeSandboxReq {
	return &sandboxtypes.CreateCubeSandboxReq{
		InstanceType: "cubebox",
		Containers: []*sandboxtypes.Container{
			{
				Name:      "c0",
				Image:     &sandboxtypes.ImageSpec{Image: "busybox:latest"},
				Resources: &sandboxtypes.Resource{Cpu: "1", Mem: "1Gi"},
			},
		},
	}
}

func patchImportNodeInventory(patches *gomonkey.Patches) {
	patches.ApplyFunc(localcache.GetNodesByIp, func(ip string) (*node.Node, bool) {
		return &node.Node{InsID: "node-1", IP: ip}, true
	})
}

func TestImportSnapshotReusesExistingRequest(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchImportNodeInventory(patches)

	patches.ApplyFunc(getTemplateImageJobByRequestID, func(ctx context.Context, requestID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:     "job-existing",
			Operation: JobOperationSnapshotImport,
		}, nil
	})
	patches.ApplyFunc(snapshotImportRequestMatches, func(_, _, _, _, _ string, _ *sandboxtypes.CreateCubeSandboxReq) bool {
		return true
	})
	patches.ApplyFunc(GetTemplateImageJobInfo, func(ctx context.Context, jobID string) (*sandboxtypes.TemplateImageJobInfo, error) {
		return &sandboxtypes.TemplateImageJobInfo{
			JobID:      jobID,
			TemplateID: "snap-existing",
			Status:     JobStatusReady,
		}, nil
	})

	info, err := ImportSnapshot(context.Background(), "req-existing", "node-1", "10.0.0.1", "/data/import/rootfs.vol", importTestSpec())
	if err != nil {
		t.Fatalf("ImportSnapshot returned error: %v", err)
	}
	if info == nil || info.JobID != "job-existing" {
		t.Fatalf("unexpected job info: %#v", info)
	}
}

func TestImportSnapshotResumesPendingExistingRequest(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchImportNodeInventory(patches)
	ran := false
	infoCalls := 0

	patches.ApplyFunc(getTemplateImageJobByRequestID, func(ctx context.Context, requestID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:     "job-pending",
			Operation: JobOperationSnapshotImport,
		}, nil
	})
	patches.ApplyFunc(snapshotImportRequestMatches, func(_, _, _, _, _ string, _ *sandboxtypes.CreateCubeSandboxReq) bool {
		return true
	})
	patches.ApplyFunc(GetTemplateImageJobInfo, func(ctx context.Context, jobID string) (*sandboxtypes.TemplateImageJobInfo, error) {
		infoCalls++
		status := JobStatusPending
		if infoCalls > 1 {
			status = JobStatusReady
		}
		return &sandboxtypes.TemplateImageJobInfo{
			JobID:      jobID,
			RequestID:  "req-pending",
			TemplateID: "snap-existing",
			Status:     status,
		}, nil
	})
	patches.ApplyFunc(claimSnapshotJobExecution, func(ctx context.Context, jobID, phase string, progress int32) (bool, error) {
		return true, nil
	})
	patches.ApplyFunc(runSnapshotImportJob, func(ctx context.Context, jobID, rootfsSource, nodeID, nodeIP string, createReq, storedReq *sandboxtypes.CreateCubeSandboxReq) error {
		ran = true
		return nil
	})

	info, err := ImportSnapshot(context.Background(), "req-pending", "node-1", "10.0.0.1", "/data/import/rootfs.vol", importTestSpec())
	if err != nil {
		t.Fatalf("expected pending existing request to resume, got %v", err)
	}
	if !ran {
		t.Fatal("expected pending existing request to execute import job")
	}
	if info == nil || info.Status != JobStatusReady {
		t.Fatalf("unexpected job info: %#v", info)
	}
}

func TestImportSnapshotReturnsStoredFailureForExistingRequest(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchImportNodeInventory(patches)

	patches.ApplyFunc(getTemplateImageJobByRequestID, func(ctx context.Context, requestID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:     "job-failed",
			Operation: JobOperationSnapshotImport,
		}, nil
	})
	patches.ApplyFunc(snapshotImportRequestMatches, func(_, _, _, _, _ string, _ *sandboxtypes.CreateCubeSandboxReq) bool {
		return true
	})
	patches.ApplyFunc(GetTemplateImageJobInfo, func(ctx context.Context, jobID string) (*sandboxtypes.TemplateImageJobInfo, error) {
		return &sandboxtypes.TemplateImageJobInfo{
			JobID:        jobID,
			RequestID:    "req-failed",
			TemplateID:   "snap-existing",
			Status:       JobStatusFailed,
			ErrorMessage: "snapshot import failed",
		}, nil
	})

	_, err := ImportSnapshot(context.Background(), "req-failed", "node-1", "10.0.0.1", "/data/import/rootfs.vol", importTestSpec())
	if err == nil {
		t.Fatal("expected stored failure for existing request")
	}
	if err.Error() != "snapshot import failed" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestImportSnapshotRejectsMismatchedPayload(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchImportNodeInventory(patches)

	patches.ApplyFunc(getTemplateImageJobByRequestID, func(ctx context.Context, requestID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:       "job-existing",
			Operation:   JobOperationSnapshotImport,
			RequestJSON: `{"request_id":"req-1","node_id":"node-1","node_ip":"10.0.0.1","rootfs_source_path":"/data/import/other.vol"}`,
		}, nil
	})

	_, err := ImportSnapshot(context.Background(), "req-1", "node-1", "10.0.0.1", "/data/import/rootfs.vol", importTestSpec())
	if !errors.Is(err, ErrTemplateAttemptInProgress) {
		t.Fatalf("expected payload mismatch to return ErrTemplateAttemptInProgress, got %v", err)
	}
}

func TestImportSnapshotRejectsUnknownNodeAndInstanceType(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetNodesByIp, func(ip string) (*node.Node, bool) {
		return nil, false
	})

	if _, err := ImportSnapshot(context.Background(), "req-1", "", "10.9.9.9", "/data/import/rootfs.vol", importTestSpec()); err == nil || !strings.Contains(err.Error(), "not a registered node") {
		t.Fatalf("expected unknown-node rejection, got %v", err)
	}

	patchImportNodeInventory(patches)
	spec := importTestSpec()
	spec.InstanceType = "cube-v2"
	if _, err := ImportSnapshot(context.Background(), "req-1", "", "10.0.0.1", "/data/import/rootfs.vol", spec); err == nil || !strings.Contains(err.Error(), "does not support snapshot import") {
		t.Fatalf("expected instance-type rejection, got %v", err)
	}

	if _, err := ImportSnapshot(context.Background(), "req-1", "node-OTHER", "10.0.0.1", "/data/import/rootfs.vol", importTestSpec()); err == nil || !strings.Contains(err.Error(), "does not match node") {
		t.Fatalf("expected host_id mismatch rejection, got %v", err)
	}
}

func TestSnapshotImportRequestMatchesRetryAfterIDStamp(t *testing.T) {
	t.Parallel()

	build := func() *sandboxtypes.CreateCubeSandboxReq {
		spec := importTestSpec()
		spec.Request = &sandboxtypes.Request{RequestID: "req-1"}
		_, storedReq, err := buildSnapshotRequests(spec, "")
		if err != nil {
			t.Fatalf("buildSnapshotRequests: %v", err)
		}
		return storedReq
	}

	submitted := build()
	fingerprint, err := importSpecFingerprint(submitted)
	if err != nil {
		t.Fatalf("importSpecFingerprint: %v", err)
	}
	submitted.Annotations["cube.master.appsnapshot.template.id"] = "snap-generated"
	raw, marshalErr := json.Marshal(snapshotImportJobRequest{
		RequestID:        "req-1",
		SnapshotID:       "snap-generated",
		NodeID:           "node-1",
		NodeIP:           "10.0.0.1",
		RootfsSourcePath: "/data/import/rootfs.vol",
		SpecFingerprint:  fingerprint,
	})
	if marshalErr != nil {
		t.Fatalf("marshal: %v", marshalErr)
	}

	if !snapshotImportRequestMatches(string(raw), "req-1", "node-1", "10.0.0.1", "/data/import/rootfs.vol", build()) {
		t.Fatal("identical retry payload must match the stored job")
	}
	if snapshotImportRequestMatches(string(raw), "req-1", "node-1", "10.0.0.1", "/data/import/other.vol", build()) {
		t.Fatal("different rootfs path must not match")
	}
}

func TestImportSnapshotRejectsInvalidResources(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchImportNodeInventory(patches)

	spec := importTestSpec()
	spec.Containers[0].Resources = &sandboxtypes.Resource{Cpu: "bogus", Mem: "1Gi"}
	if _, err := ImportSnapshot(context.Background(), "req-1", "", "10.0.0.1", "/data/import/rootfs.vol", spec); err == nil || !strings.Contains(err.Error(), "invalid container cpu") {
		t.Fatalf("expected cpu rejection, got %v", err)
	}
}

func TestImportSnapshotRejectsMultiContainerSpec(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchImportNodeInventory(patches)

	spec := importTestSpec()
	spec.Containers = append(spec.Containers, &sandboxtypes.Container{
		Name:      "c1",
		Image:     &sandboxtypes.ImageSpec{Image: "busybox:latest"},
		Resources: &sandboxtypes.Resource{Cpu: "1", Mem: "1Gi"},
	})
	if _, err := ImportSnapshot(context.Background(), "req-1", "", "10.0.0.1", "/data/import/rootfs.vol", spec); err == nil || !strings.Contains(err.Error(), "single-container create spec") {
		t.Fatalf("expected multi-container rejection, got %v", err)
	}
}

func TestImportSnapshotRejectsNonPositiveFirstContainerSpec(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchImportNodeInventory(patches)

	missing := importTestSpec()
	missing.Containers[0].Resources = nil
	if _, err := ImportSnapshot(context.Background(), "req-1", "", "10.0.0.1", "/data/import/rootfs.vol", missing); err == nil || !strings.Contains(err.Error(), "first container resources are required") {
		t.Fatalf("expected missing resources rejection, got %v", err)
	}

	zeroed := importTestSpec()
	zeroed.Containers[0].Resources = &sandboxtypes.Resource{Cpu: "0", Mem: "1Gi"}
	if _, err := ImportSnapshot(context.Background(), "req-1", "", "10.0.0.1", "/data/import/rootfs.vol", zeroed); err == nil || !strings.Contains(err.Error(), "positive cpu and memory") {
		t.Fatalf("expected non-positive cpu rejection, got %v", err)
	}
}

func TestImportSnapshotRejectsNilContainer(t *testing.T) {
	oldDB := store.db
	store.db = &gorm.DB{}
	defer func() { store.db = oldDB }()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patchImportNodeInventory(patches)

	spec := importTestSpec()
	spec.Containers = []*sandboxtypes.Container{nil}
	if _, err := ImportSnapshot(context.Background(), "req-1", "", "10.0.0.1", "/data/import/rootfs.vol", spec); err == nil || !strings.Contains(err.Error(), "at least one container") {
		t.Fatalf("expected nil container rejection, got %v", err)
	}
}

func TestSnapshotImportRequestMatchesFailsClosedOnCorruptRow(t *testing.T) {
	t.Parallel()

	if snapshotImportRequestMatches("", "req-1", "node-1", "10.0.0.1", "/data/import/rootfs.vol", importTestSpec()) {
		t.Fatal("empty RequestJSON must not match")
	}
	raw := `{"request_id":"req-1","node_id":"node-1","node_ip":"10.0.0.1","rootfs_source_path":"/data/import/rootfs.vol"}`
	if snapshotImportRequestMatches(raw, "req-1", "node-1", "10.0.0.1", "/data/import/rootfs.vol", importTestSpec()) {
		t.Fatal("empty SpecFingerprint must not match")
	}
}
