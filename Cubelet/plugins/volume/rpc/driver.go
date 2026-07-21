// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package rpc provides a VolumePlugin driver that delegates to an external
// gRPC server.  The server must implement the VolumePluginService defined in
// api/services/volumeplugin/v1/volumeplugin.proto.
//
// Connection target format:
//   - Unix socket: "unix:///run/cube/volume-<name>.sock"
//   - TCP:         "127.0.0.1:9100"
//
// The driver stores the target in PluginConfig.SocketPath.  A "unix://" prefix
// is added automatically if the path starts with "/" and has no scheme.
package rpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	vpb "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/volumeplugin/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/grpctarget"
)

// Plugin is a VolumePlugin backed by an external gRPC process.
type Plugin struct {
	name   string
	conn   *grpc.ClientConn
	client vpb.VolumePluginServiceClient
}

// New creates a Plugin for the given driver name.
// Init must be called before Attach/Detach.
func New(name string) *Plugin {
	return &Plugin{name: name}
}

func (p *Plugin) Name() string                  { return p.name }
func (p *Plugin) PluginType() volume.PluginType { return volume.PluginTypeRPC }

// Init dials the gRPC server at cfg.SocketPath.
func (p *Plugin) Init(ctx context.Context, cfg volume.PluginConfig) error {
	if cfg.SocketPath == "" {
		return fmt.Errorf("rpc volume plugin %q: socket_path is empty", p.name)
	}

	target := grpctarget.Normalize(cfg.SocketPath)

	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("rpc volume plugin %q: dial %s: %w", p.name, target, err)
	}
	p.conn = conn
	p.client = vpb.NewVolumePluginServiceClient(conn)

	return nil
}

// Attach provisions the volume described by req via gRPC.
func (p *Plugin) Attach(ctx context.Context, req *volume.AttachRequest) (*volume.AttachResult, error) {
	resp, err := p.client.Attach(ctx, &vpb.AttachRequest{
		SandboxId:     req.SandboxID,
		Namespace:     req.Namespace,
		VolumeId:      req.VolumeID,
		RefCount:      req.RefCount,
		VolumeBaseDir: req.VolumeBaseDir,
	})
	if err != nil {
		return nil, fmt.Errorf("rpc volume plugin %q attach: %w", p.name, err)
	}
	return &volume.AttachResult{
		VolumeID: req.VolumeID,
		HostPath: resp.GetHostPath(),
		Metadata: resp.GetMetadata(),
	}, nil
}

// Detach tears down the volume attachment via gRPC.
func (p *Plugin) Detach(ctx context.Context, req *volume.DetachRequest) error {
	_, err := p.client.Detach(ctx, &vpb.DetachRequest{
		SandboxId: req.SandboxID,
		Namespace: req.Namespace,
		VolumeId:  req.VolumeID,
		Metadata:  req.Metadata,
		RefCount:  req.RefCount,
	})
	if err != nil {
		return fmt.Errorf("rpc volume plugin %q detach: %w", p.name, err)
	}
	return nil
}

// Close closes the underlying gRPC connection.
func (p *Plugin) Close() error {
	if p.conn != nil {
		return p.conn.Close()
	}
	return nil
}
