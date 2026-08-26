// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package snapshot

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"google.golang.org/grpc"

	api "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/snapshot/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

var _ api.SnapshotServer = &service{}

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.CubeboxServicePlugin,
		ID:     "snapshot",
		InitFn: initFunc,
	})
}

func initFunc(ic *plugin.InitContext) (interface{}, error) {
	return &service{}, nil
}

type service struct {
	api.UnimplementedSnapshotServer
}

func (s *service) RegisterTCP(server *grpc.Server) error {
	api.RegisterSnapshotServer(server, s)
	return nil
}

func (s *service) Register(server *grpc.Server) error {
	api.RegisterSnapshotServer(server, s)
	return nil
}

func (s *service) Status(ctx context.Context, req *api.StatusRequest) (*api.StatusResponse, error) {
	return s.statusOne(ctx, req.GetRequestId(), req.GetSnapshotId(), req.GetBackend()), nil
}

func (s *service) BatchStatus(ctx context.Context, req *api.BatchStatusRequest) (*api.BatchStatusResponse, error) {
	items := req.GetItems()
	rsp := &api.BatchStatusResponse{
		RequestId: req.GetRequestId(),
		Items:     make([]*api.StatusResponse, 0, len(items)),
	}
	for _, item := range items {
		if item == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			break
		}
		rsp.Items = append(rsp.Items, s.statusOne(ctx, req.GetRequestId(), item.GetSnapshotId(), item.GetBackend()))
	}
	return rsp, nil
}

func (s *service) statusOne(ctx context.Context, requestID, snapshotID, rawBackend string) *api.StatusResponse {
	backend, err := normalizeStatusBackend(rawBackend)
	if err != nil {
		// Unknown backend is not an s3lvol export failure; leave State unset
		// so Master keeps remote_status=inprogress.
		return unsetStatus(requestID, snapshotID, strings.TrimSpace(rawBackend), err.Error())
	}
	rsp := &api.StatusResponse{
		RequestId:  requestID,
		SnapshotId: snapshotID,
		Backend:    backend,
		State:      cow.RemoteStatePending,
	}
	if backend != cow.BackendS3 {
		rsp.Message = "xfs has no remote upload"
		return rsp
	}

	st, err := storage.SnapshotUploadStatus(ctx, backend, snapshotID)
	if err != nil {
		// Store-not-ready / lookup errors are transient. Empty State means
		// the remotestatus poller must not stamp failed.
		return unsetStatus(requestID, snapshotID, backend, err.Error())
	}
	if st == nil {
		return unsetStatus(requestID, snapshotID, backend, "empty upload status")
	}
	rsp.State = strings.TrimSpace(st.State)
	if rsp.State == "" {
		rsp.State = cow.RemoteStatePending
	}
	rsp.Message = st.Message
	if st.RemoteUUIDs != nil && !st.RemoteUUIDs.Empty() && rsp.Message == "" {
		rsp.Message = st.RemoteUUIDs.JSON()
	}
	rsp.RemoteReady = rsp.State == cow.RemoteStateReady
	if st.RootfsDeletable != nil {
		if *st.RootfsDeletable {
			rsp.RootfsDeletable = "true"
		} else {
			rsp.RootfsDeletable = "false"
		}
	}
	return rsp
}

func unsetStatus(requestID, snapshotID, backend, message string) *api.StatusResponse {
	return &api.StatusResponse{
		RequestId:  requestID,
		SnapshotId: snapshotID,
		Backend:    backend,
		Message:    message,
	}
}

func normalizeStatusBackend(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "xfs", "cow", "cubecow", "reflink", "xfscow":
		return cow.BackendXFS, nil
	case "s3":
		return cow.BackendS3, nil
	default:
		return "", fmt.Errorf("unsupported backend %q (want xfs or s3)", raw)
	}
}
