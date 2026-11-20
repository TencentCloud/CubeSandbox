// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package rpc

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	vpb "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/volumeplugin/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type contentCredentialsServer struct {
	vpb.UnimplementedVolumeContentServiceServer
	request *vpb.IssueVolumeCredentialsRequest
}

func (s *contentCredentialsServer) IssueCredentials(_ context.Context, req *vpb.IssueVolumeCredentialsRequest) (*vpb.IssueVolumeCredentialsResponse, error) {
	s.request = req
	return &vpb.IssueVolumeCredentialsResponse{
		Provider: "cos", Endpoint: "https://bucket.cos.ap-test.myqcloud.com", Bucket: "bucket-1234567890",
		Region: "ap-test", Prefix: "volumes/vol-1/", AddressingStyle: "path", AccessKeyId: "tmp-id",
		SecretAccessKey: "tmp-secret", SessionToken: "tmp-token", ExpiresAtUnix: 2_000_000_000,
	}, nil
}

func TestIssueDirectAccessCredentialsRPC(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := &contentCredentialsServer{}
	grpcServer := grpc.NewServer()
	vpb.RegisterVolumeContentServiceServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})

	conn, err := grpc.NewClient("passthrough:///volume-content-test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
	)
	require.NoError(t, err)
	p := &ContentPlugin{name: "test-content", conn: conn, client: vpb.NewVolumeContentServiceClient(conn)}
	t.Cleanup(func() { require.NoError(t, conn.Close()) })

	issued, err := p.IssueCredentials(context.Background(), "vol-1", "backend-state", "user-1")
	require.NoError(t, err)
	assert.Equal(t, "cos", issued.Provider)
	assert.Equal(t, "https://bucket.cos.ap-test.myqcloud.com", issued.Endpoint)
	assert.Equal(t, "bucket-1234567890", issued.Bucket)
	assert.Equal(t, "ap-test", issued.Region)
	assert.Equal(t, "volumes/vol-1/", issued.Prefix)
	assert.Equal(t, "path", issued.AddressingStyle)
	assert.Equal(t, "tmp-id", issued.AccessKeyID)
	assert.Equal(t, "tmp-secret", issued.SecretAccessKey)
	assert.Equal(t, "tmp-token", issued.SessionToken)
	assert.Equal(t, time.Unix(2_000_000_000, 0), issued.ExpiresAt)
	require.NotNil(t, server.request)
	assert.Equal(t, "vol-1", server.request.GetVolumeId())
	assert.Equal(t, "backend-state", server.request.GetPrivateData())
	assert.Equal(t, "user-1", server.request.GetSubject())
}
