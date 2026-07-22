// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubelet

import (
	"context"
	"errors"
	"sync"

	cubebox "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	grpcpool "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/grpc-middleware/pool"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet/grpcconn"
)

type TerminalStream struct {
	stream cubebox.CubeboxMgr_AttachTerminalClient
	conn   grpcpool.Conn
	once   sync.Once
}

func OpenTerminal(ctx context.Context, calleeEndpoint string) (*TerminalStream, error) {
	conn, err := grpcconn.GetWorkerConn(ctx, calleeEndpoint)
	if err != nil {
		return nil, err
	}
	stream, err := cubebox.NewCubeboxMgrClient(conn.Value()).AttachTerminal(ctx)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &TerminalStream{stream: stream, conn: conn}, nil
}

func (s *TerminalStream) Send(frame *cubebox.TerminalClientMessage) error {
	return s.stream.Send(frame)
}

func (s *TerminalStream) Recv() (*cubebox.TerminalServerMessage, error) {
	return s.stream.Recv()
}

func (s *TerminalStream) CloseSend() error {
	var closeErr error
	s.once.Do(func() {
		closeErr = errors.Join(s.stream.CloseSend(), s.conn.Close())
	})
	return closeErr
}
