// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package snapshot

import (
	"context"
	"testing"

	api "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/snapshot/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
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

func TestStatusS3WithoutStoreReportsFailedOrPending(t *testing.T) {
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
	// Without an initialized S3 store the query fails closed.
	if rsp.GetRemoteReady() {
		t.Fatal("s3 without store must not be remote_ready")
	}
	if rsp.GetState() != cow.RemoteStateFailed && rsp.GetState() != cow.RemoteStatePending {
		t.Fatalf("state=%q", rsp.GetState())
	}
}
