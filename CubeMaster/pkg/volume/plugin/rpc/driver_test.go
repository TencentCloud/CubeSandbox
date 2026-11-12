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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type testControllerServer struct {
	vpb.UnimplementedVolumeControllerServiceServer
	createRequests  chan *vpb.CreateRequest
	destroyRequests chan *vpb.DestroyRequest
	createResponse  *vpb.CreateResponse
	createErr       error
	destroyErr      error
}

func (s *testControllerServer) Create(_ context.Context, req *vpb.CreateRequest) (*vpb.CreateResponse, error) {
	s.createRequests <- req
	return s.createResponse, s.createErr
}

func (s *testControllerServer) Destroy(_ context.Context, req *vpb.DestroyRequest) (*vpb.DestroyResponse, error) {
	s.destroyRequests <- req
	if s.destroyErr != nil {
		return nil, s.destroyErr
	}
	return &vpb.DestroyResponse{}, nil
}

// Use a real gRPC client/server so the assertions cover protobuf serialization,
// rather than just assigning fields on an in-process mock client.
func newTestPlugin(t *testing.T, server *testControllerServer) *Plugin {
	t.Helper()
	server.createRequests = make(chan *vpb.CreateRequest, 1)
	server.destroyRequests = make(chan *vpb.DestroyRequest, 1)
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	vpb.RegisterVolumeControllerServiceServer(grpcServer, server)
	go func() {
		_ = grpcServer.Serve(listener)
	}()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})
	conn, err := grpc.NewClient("passthrough:///volume-plugin-test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
	)
	require.NoError(t, err)
	p := &Plugin{name: "test-rpc", conn: conn, client: vpb.NewVolumeControllerServiceClient(conn)}
	t.Cleanup(func() { require.NoError(t, p.Close()) })
	return p
}

func TestPrivateDataRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		input  string
		output string
	}{
		{name: "empty"},
		{
			name:   "distinct opaque input and output",
			input:  "  {\"bucket\":\"bucket-a\",\"prefix\":\"路径/'\\\\$\",\"cred_ref\":\"ref-a\"}  ",
			output: `{"bucket":"bucket-a","prefix":"normalized/","cred_ref":"ref-a"}`,
		},
		{name: "nonempty input and empty output", input: "caller-state-a"},
		{name: "empty input and nonempty output", output: "plugin-state-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &testControllerServer{createResponse: &vpb.CreateResponse{Token: "test-token", PrivateData: tc.output}}
			p := newTestPlugin(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			info, err := p.Create(ctx, "vol-1", "volume-name", tc.input)
			require.NoError(t, err)
			require.NotNil(t, info)
			select {
			case req := <-server.createRequests:
				assert.Equal(t, "vol-1", req.GetVolumeId())
				assert.Equal(t, "volume-name", req.GetName())
				assert.Equal(t, tc.input, req.GetPrivateData())
			case <-ctx.Done():
				t.Fatal("Create did not reach the gRPC server")
			}
			assert.Equal(t, "vol-1", info.VolumeID)
			assert.Equal(t, "volume-name", info.Name)
			assert.Equal(t, "test-rpc", info.PluginName)
			assert.Equal(t, "test-token", info.Token)
			assert.Equal(t, tc.output, info.PrivateData)

			require.NoError(t, p.Destroy(ctx, info.VolumeID, info.PrivateData))
			select {
			case req := <-server.destroyRequests:
				assert.Equal(t, "vol-1", req.GetVolumeId())
				assert.Equal(t, tc.output, req.GetPrivateData())
			case <-ctx.Done():
				t.Fatal("Destroy did not reach the gRPC server")
			}
		})
	}
}

func TestRPCErrorPropagation(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		server := &testControllerServer{createErr: status.Error(codes.PermissionDenied, "create denied")}
		p := newTestPlugin(t, server)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		info, err := p.Create(ctx, "vol-1", "volume-name", "caller-state-a")
		require.Error(t, err)
		assert.Nil(t, info)
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		assert.ErrorContains(t, err, `rpc volume plugin "test-rpc" create`)
		assert.ErrorContains(t, err, "create denied")
	})
	t.Run("destroy", func(t *testing.T) {
		server := &testControllerServer{destroyErr: status.Error(codes.Unavailable, "destroy unavailable")}
		p := newTestPlugin(t, server)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := p.Destroy(ctx, "vol-1", "plugin-state-b")
		require.Error(t, err)
		assert.Equal(t, codes.Unavailable, status.Code(err))
		assert.ErrorContains(t, err, `rpc volume plugin "test-rpc" destroy`)
		assert.ErrorContains(t, err, "destroy unavailable")
	})
}
