// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package rpc implements a ControllerPlugin that calls an external gRPC server.
//
// The server must implement VolumeControllerService from
// Cubelet/api/services/volumeplugin/v1/volumeplugin.proto.
//
// Connection target (socket_path config):
//   - Unix socket: "unix:///run/cube/volume-<name>.sock" or "/run/cube/…"
//   - TCP:         "tcp://127.0.0.1:9100" or "127.0.0.1:9100"
package rpc

import (
	"context"
	"fmt"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/plugin"
	vpb "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/volumeplugin/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/grpctarget"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func init() {
	plugin.SetRPCFactory(func(name, socketPath string) (plugin.ControllerPlugin, error) {
		return New(name, socketPath)
	})
}

// Plugin is a ControllerPlugin backed by an external gRPC server.
type Plugin struct {
	name   string
	conn   *grpc.ClientConn
	client vpb.VolumeControllerServiceClient
}

// New dials the gRPC server and returns a Plugin.
func New(name, socketPath string) (*Plugin, error) {
	target := grpctarget.Normalize(socketPath)
	if target == "" {
		return nil, fmt.Errorf("rpc volume plugin %q: socket_path is empty", name)
	}

	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("rpc volume plugin %q: dial %s: %w", name, target, err)
	}

	return &Plugin{
		name:   name,
		conn:   conn,
		client: vpb.NewVolumeControllerServiceClient(conn),
	}, nil
}

func (p *Plugin) Name() string { return p.name }

// Create calls the remote Create RPC.
func (p *Plugin) Create(ctx context.Context, volumeID, name string) (*plugin.VolumeInfo, error) {
	resp, err := p.client.Create(ctx, &vpb.CreateRequest{
		VolumeId: volumeID,
		Name:     name,
	})
	if err != nil {
		return nil, fmt.Errorf("rpc volume plugin %q create: %w", p.name, err)
	}
	return &plugin.VolumeInfo{
		VolumeID:   volumeID,
		Name:       name,
		Token:      resp.GetToken(),
		PluginName: p.name,
	}, nil
}

// Destroy calls the remote Destroy RPC.
func (p *Plugin) Destroy(ctx context.Context, volumeID string) error {
	_, err := p.client.Destroy(ctx, &vpb.DestroyRequest{
		VolumeId: volumeID,
	})
	if err != nil {
		return fmt.Errorf("rpc volume plugin %q destroy: %w", p.name, err)
	}
	return nil
}

// Close closes the gRPC connection.
func (p *Plugin) Close() error {
	if p.conn != nil {
		return p.conn.Close()
	}
	return nil
}
