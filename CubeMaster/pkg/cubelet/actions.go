// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package cubelet provides the interface for the cube-master to interact with the cube-node.
package cubelet

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	imagesv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet/grpcconn"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
)

func Destroy(ctx context.Context, calleeEp string,
	req *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	defer conn.Close()
	c := cubebox.NewCubeboxMgrClient(conn.Value())
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(config.GetConfig().CubeletConf.CommonTimeoutInsec)*time.Second)
	defer cancel()
	return c.Destroy(ctx, req)
}

func Create(ctx context.Context, calleeEp string,
	req *cubebox.RunCubeSandboxRequest) (*cubebox.RunCubeSandboxResponse, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	defer conn.Close()
	c := cubebox.NewCubeboxMgrClient(conn.Value())

	return c.Create(ctx, req)
}

func AppSnapshot(ctx context.Context, calleeEp string,
	req *cubebox.AppSnapshotRequest) (*cubebox.AppSnapshotResponse, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	defer conn.Close()
	c := cubebox.NewCubeboxMgrClient(conn.Value())
	return c.AppSnapshot(ctx, req)
}

func CommitSandbox(ctx context.Context, calleeEp string,
	req *cubebox.CommitSandboxRequest) (*cubebox.CommitSandboxResponse, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	defer conn.Close()
	c := cubebox.NewCubeboxMgrClient(conn.Value())
	return c.CommitSandbox(ctx, req)
}

func RollbackSandbox(ctx context.Context, calleeEp string,
	req *cubebox.RollbackSandboxRequest) (*cubebox.RollbackSandboxResponse, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	defer conn.Close()
	c := cubebox.NewCubeboxMgrClient(conn.Value())
	return c.RollbackSandbox(ctx, req)
}

func CleanupTemplate(ctx context.Context, calleeEp string,
	req *cubebox.CleanupTemplateRequest) (*cubebox.CleanupTemplateResponse, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	defer conn.Close()
	c := cubebox.NewCubeboxMgrClient(conn.Value())
	return c.CleanupTemplate(ctx, req)
}

func ListSandboxSnapshots(ctx context.Context, calleeEp string,
	req *cubebox.ListSandboxSnapshotsRequest) (*cubebox.ListSandboxSnapshotsResponse, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	defer conn.Close()
	c := cubebox.NewCubeboxMgrClient(conn.Value())
	return c.ListSandboxSnapshots(ctx, req)
}

func ListLocalSnapshots(ctx context.Context, calleeEp string,
	req *cubebox.ListLocalSnapshotsRequest) (*cubebox.ListLocalSnapshotsResponse, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	defer conn.Close()
	c := cubebox.NewCubeboxMgrClient(conn.Value())
	return c.ListLocalSnapshots(ctx, req)
}

func GetLocalSnapshot(ctx context.Context, calleeEp string,
	req *cubebox.GetLocalSnapshotRequest) (*cubebox.GetLocalSnapshotResponse, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	defer conn.Close()
	c := cubebox.NewCubeboxMgrClient(conn.Value())
	return c.GetLocalSnapshot(ctx, req)
}

func GetStorageMetrics(ctx context.Context, calleeEp string,
	req *cubebox.GetStorageMetricsRequest) (*cubebox.GetStorageMetricsResponse, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	defer conn.Close()
	c := cubebox.NewCubeboxMgrClient(conn.Value())
	return c.GetStorageMetrics(ctx, req)
}

func List(ctx context.Context, calleeEp string,
	req *cubebox.ListCubeSandboxRequest) (*cubebox.ListCubeSandboxResponse, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	defer conn.Close()
	c := cubebox.NewCubeboxMgrClient(conn.Value())
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(config.GetConfig().CubeletConf.CommonTimeoutInsec)*time.Second)
	defer cancel()
	return c.List(ctx, req)
}

func CreateImage(ctx context.Context, calleeEp string,
	req *imagesv1.CreateImageRequest) (*imagesv1.CreateImageRequestResponse, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	defer conn.Close()
	c := imagesv1.NewImagesClient(conn.Value())
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(config.GetConfig().CubeletConf.CreateImageTimeoutInSec)*time.Second)
	defer cancel()

	return c.CreateImage(ctx, req)
}

func DeleteImage(ctx context.Context, calleeEp string, req *imagesv1.DestroyImageRequest) (*imagesv1.DestroyImageResponse,
	error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	defer conn.Close()
	c := imagesv1.NewImagesClient(conn.Value())
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(config.GetConfig().CubeletConf.CreateImageTimeoutInSec)*time.Second)
	defer cancel()
	return c.DestroyImage(ctx, req)
}

func GetCubeletAddr(hostIP string) string {
	return strings.Join([]string{hostIP,
		strconv.Itoa(config.GetConfig().CubeletConf.Grpc.GrpcPort)}, ":")
}

func Update(ctx context.Context, calleeEp string,
	req *cubebox.UpdateCubeSandboxRequest) (*cubebox.UpdateCubeSandboxResponse, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	defer conn.Close()
	c := cubebox.NewCubeboxMgrClient(conn.Value())
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(config.GetConfig().CubeletConf.CommonTimeoutInsec)*time.Second)
	defer cancel()
	return c.Update(ctx, req)
}

func Exec(ctx context.Context, calleeEp string,
	req *cubebox.ExecCubeSandboxRequest) (*cubebox.ExecCubeSandboxResponse, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	defer conn.Close()
	c := cubebox.NewCubeboxMgrClient(conn.Value())
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(config.GetConfig().CubeletConf.CommonTimeoutInsec)*time.Second)
	defer cancel()
	return c.Exec(ctx, req)
}

// TerminalStream keeps the worker-pool reference alive for the complete bidi
// stream lifetime. Unary actions release their reference when the RPC returns;
// terminal streams must release it only after both relay directions stop.
type TerminalStream struct {
	cubebox.CubeboxMgr_TerminalClient
	release func() error

	closeSendOnce sync.Once
	closeSendErr  error
	closeOnce     sync.Once
	closeErr      error
}

func Terminal(ctx context.Context, calleeEp string) (*TerminalStream, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEp)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_ConnHostFailed, err.Error())
	}
	client := cubebox.NewCubeboxMgrClient(conn.Value())
	stream, err := client.Terminal(ctx)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &TerminalStream{
		CubeboxMgr_TerminalClient: stream,
		release:                   conn.Close,
	}, nil
}

func (s *TerminalStream) CloseSend() error {
	if s == nil {
		return nil
	}
	s.closeSendOnce.Do(func() {
		s.closeSendErr = s.CubeboxMgr_TerminalClient.CloseSend()
	})
	return s.closeSendErr
}

func (s *TerminalStream) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		closeSendErr := s.CloseSend()
		var releaseErr error
		if s.release != nil {
			releaseErr = s.release()
		}
		s.closeErr = errors.Join(closeSendErr, releaseErr)
	})
	return s.closeErr
}
