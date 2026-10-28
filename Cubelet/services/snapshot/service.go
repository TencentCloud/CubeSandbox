// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package snapshot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"google.golang.org/grpc"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	api "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/snapshot/v1"
)

var _ api.SnapshotServer = &service{}

var snapshotUploadStatus = storage.SnapshotUploadStatus

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

const (
	// statusWaitPoll is how often a Status call held by wait_ready_ms checks
	// again; a jfs check runs `juicefs info` on the package files.
	statusWaitPoll = 100 * time.Millisecond
	statusWaitMax  = time.Minute
)

func (s *service) Status(ctx context.Context, req *api.StatusRequest) (*api.StatusResponse, error) {
	rsp := s.statusOne(ctx, req.GetRequestId(), req.GetSnapshotId(), req.GetBackend())
	wait := min(time.Duration(req.GetWaitReadyMs())*time.Millisecond, statusWaitMax)
	if wait <= 0 || !storage.CrossNodeImportBackend(rsp.GetBackend()) {
		return rsp, nil
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(statusWaitPoll)
	defer poll.Stop()
	for !uploadSettled(rsp) {
		select {
		case <-ctx.Done():
			return rsp, nil
		case <-deadline.C:
			return rsp, nil
		case <-poll.C:
		}
		rsp = s.statusOne(ctx, req.GetRequestId(), req.GetSnapshotId(), req.GetBackend())
	}
	return rsp, nil
}

func uploadSettled(rsp *api.StatusResponse) bool {
	switch rsp.GetState() {
	case cow.RemoteStateReady, cow.RemoteStateFailed:
		return true
	}
	return false
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
	if !storage.CrossNodeImportBackend(backend) {
		rsp.Message = backend + " has no remote upload"
		return rsp
	}

	st, err := snapshotUploadStatus(ctx, backend, snapshotID)
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
	case "jfs", "juicefs":
		return cow.BackendJFS, nil
	default:
		return "", fmt.Errorf("unsupported backend %q (want xfs, s3 or jfs)", raw)
	}
}
