// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package vsockets

import (
	"context"
	"net"
	"time"
)

type VsocketFactory interface {
	CreateVsocket(ctx context.Context, sandboxID string) error
	DeleteVsocket(ctx context.Context, sandboxID string) error
	NewSandboxVscoketConnFactory(sandboxID string) SandboxVscoketConnFactory
}

type SandboxVscoketConnOption struct {
	Type    CubeConn
	Timeout time.Duration
}

type CubeConn string

const (
	CubeConnHttpProxy CubeConn = "HTTP_PROXY"
	CubeConnCubeHost  CubeConn = "CUBE_HOST"
)

type SandboxVscoketConnFactory interface {
	ID() string
	NewConn(opts SandboxVscoketConnOption) (net.Conn, error)
}
