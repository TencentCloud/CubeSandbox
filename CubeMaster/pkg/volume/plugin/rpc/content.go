// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package rpc

import (
	"context"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/plugin"
	vpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/volumeplugin/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/volumeplugin/grpctarget"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ContentPlugin connects to a dedicated credential-vending gRPC hook.
type ContentPlugin struct {
	name   string
	conn   *grpc.ClientConn
	client vpb.VolumeContentServiceClient
}

func NewContent(name, socketPath string) (*ContentPlugin, error) {
	target := grpctarget.Normalize(socketPath)
	if target == "" {
		return nil, fmt.Errorf("rpc volume credential hook %q: socket_path is empty", name)
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("rpc volume credential hook %q: dial %s: %w", name, target, err)
	}
	return &ContentPlugin{name: name, conn: conn, client: vpb.NewVolumeContentServiceClient(conn)}, nil
}

func (p *ContentPlugin) Name() string { return p.name }

func (p *ContentPlugin) IssueCredentials(ctx context.Context, volumeID, privateData, subject string) (*plugin.DirectAccessCredentials, error) {
	response, err := p.client.IssueCredentials(ctx, &vpb.IssueVolumeCredentialsRequest{
		VolumeId: volumeID, PrivateData: privateData, Subject: subject,
	})
	if err != nil {
		return nil, fmt.Errorf("rpc content plugin %q issue credentials: %w", p.name, err)
	}
	return &plugin.DirectAccessCredentials{
		Provider: response.GetProvider(), Endpoint: response.GetEndpoint(), Bucket: response.GetBucket(),
		Region: response.GetRegion(), Prefix: response.GetPrefix(), AddressingStyle: response.GetAddressingStyle(),
		AccessKeyID:     response.GetAccessKeyId(),
		SecretAccessKey: response.GetSecretAccessKey(), SessionToken: response.GetSessionToken(),
		ExpiresAt: time.Unix(response.GetExpiresAtUnix(), 0),
	}, nil
}

func (p *ContentPlugin) Close() error { return p.conn.Close() }
