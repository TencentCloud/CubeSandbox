// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package snapshot

import (
	"context"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	api "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/snapshot/v1"
)

func TestStatusXFSHasNoRemoteUpload(t *testing.T) {
	t.Parallel()
	s := &service{}
	rsp, err := s.Status(context.Background(), &api.StatusRequest{
		RequestId:  "r1",
		SnapshotId: "snap-1",
		Backend:    "xfs",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rsp.GetRemoteReady() {
		t.Fatal("xfs must not be remote_ready")
	}
	if rsp.GetState() != cow.RemoteStatePending {
		t.Fatalf("state=%q", rsp.GetState())
	}
	if rsp.GetBackend() != cow.BackendXFS {
		t.Fatalf("backend=%q", rsp.GetBackend())
	}
}

func TestBatchStatusPreservesOrderAndSkipsNil(t *testing.T) {
	t.Parallel()
	s := &service{}
	rsp, err := s.BatchStatus(context.Background(), &api.BatchStatusRequest{
		RequestId: "r-batch",
		Items: []*api.StatusQuery{
			{SnapshotId: "snap-a", Backend: "xfs"},
			nil,
			{SnapshotId: "snap-b", Backend: "xfs"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rsp.GetRequestId() != "r-batch" {
		t.Fatalf("request_id=%q", rsp.GetRequestId())
	}
	if len(rsp.GetItems()) != 2 {
		t.Fatalf("items=%d", len(rsp.GetItems()))
	}
	if rsp.GetItems()[0].GetSnapshotId() != "snap-a" || rsp.GetItems()[1].GetSnapshotId() != "snap-b" {
		t.Fatalf("items=%v", rsp.GetItems())
	}
}

func TestStatusS3WithoutStoreLeavesStateUnset(t *testing.T) {
	t.Parallel()
	s := &service{}
	rsp, err := s.Status(context.Background(), &api.StatusRequest{
		RequestId:  "r1",
		SnapshotId: "snap-1",
		Backend:    "s3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rsp.GetBackend() != cow.BackendS3 {
		t.Fatalf("backend=%q", rsp.GetBackend())
	}
	if rsp.GetRemoteReady() {
		t.Fatal("s3 without store must not be remote_ready")
	}
	if rsp.GetState() != "" {
		t.Fatalf("store error must leave state unset, got %q", rsp.GetState())
	}
	if rsp.GetMessage() == "" {
		t.Fatal("expected an error message")
	}
}

func TestStatusUnsupportedBackendLeavesStateUnset(t *testing.T) {
	t.Parallel()
	s := &service{}
	rsp, err := s.Status(context.Background(), &api.StatusRequest{
		RequestId:  "r1",
		SnapshotId: "snap-1",
		Backend:    "bogus",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rsp.GetState() != "" {
		t.Fatalf("unsupported backend must leave state unset, got %q", rsp.GetState())
	}
	if rsp.GetRemoteReady() {
		t.Fatal("unsupported backend must not be remote_ready")
	}
}

func fakeUploadStatus(t *testing.T, states ...string) *int {
	t.Helper()
	calls := 0
	prev := snapshotUploadStatus
	snapshotUploadStatus = func(_ context.Context, _, id string) (*cow.RemoteStatus, error) {
		st := states[min(calls, len(states)-1)]
		calls++
		return &cow.RemoteStatus{SnapshotID: id, State: st}, nil
	}
	t.Cleanup(func() { snapshotUploadStatus = prev })
	return &calls
}

func TestStatusWaitReadyReturnsOnceSettled(t *testing.T) {
	calls := fakeUploadStatus(t, cow.RemoteStateRunning, cow.RemoteStateRunning, cow.RemoteStateReady)
	start := time.Now()
	rsp, err := (&service{}).Status(context.Background(), &api.StatusRequest{
		SnapshotId: "snap-1", Backend: "jfs", WaitReadyMs: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rsp.GetState() != cow.RemoteStateReady || !rsp.GetRemoteReady() {
		t.Fatalf("state=%q remote_ready=%v", rsp.GetState(), rsp.GetRemoteReady())
	}
	if *calls != 3 {
		t.Fatalf("calls=%d", *calls)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("held for %s after the upload settled", d)
	}
}

func TestStatusWaitReadyGivesUpAtDeadline(t *testing.T) {
	fakeUploadStatus(t, cow.RemoteStateRunning)
	rsp, err := (&service{}).Status(context.Background(), &api.StatusRequest{
		SnapshotId: "snap-1", Backend: "s3", WaitReadyMs: 300,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rsp.GetState() != cow.RemoteStateRunning {
		t.Fatalf("state=%q", rsp.GetState())
	}
}

func TestStatusWithoutWaitAnswersAtOnce(t *testing.T) {
	calls := fakeUploadStatus(t, cow.RemoteStateRunning)
	rsp, err := (&service{}).Status(context.Background(), &api.StatusRequest{SnapshotId: "snap-1", Backend: "jfs"})
	if err != nil {
		t.Fatal(err)
	}
	if rsp.GetState() != cow.RemoteStateRunning || *calls != 1 {
		t.Fatalf("state=%q calls=%d", rsp.GetState(), *calls)
	}
	rsp, err = (&service{}).Status(context.Background(), &api.StatusRequest{SnapshotId: "snap-1", Backend: "xfs", WaitReadyMs: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if rsp.GetState() != cow.RemoteStatePending || *calls != 1 {
		t.Fatalf("xfs state=%q calls=%d", rsp.GetState(), *calls)
	}
}
