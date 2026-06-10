// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package networkagentclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	networkagentv1 "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/networkagentclient/pb"
	"google.golang.org/grpc"
)

func TestNewNoopClient(t *testing.T) {
	c := NewNoopClient()
	if c == nil {
		t.Fatal("expected noop client, got nil")
	}
}

func TestNoopClientReturnsErrNotConfigured(t *testing.T) {
	c := NewNoopClient()
	ctx := context.Background()

	if _, err := c.EnsureNetwork(ctx, &EnsureNetworkRequest{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("EnsureNetwork error = %v, want %v", err, ErrNotConfigured)
	}

	if err := c.ReleaseNetwork(ctx, &ReleaseNetworkRequest{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("ReleaseNetwork error = %v, want %v", err, ErrNotConfigured)
	}

	if _, err := c.ReconcileNetwork(ctx, &ReconcileNetworkRequest{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("ReconcileNetwork error = %v, want %v", err, ErrNotConfigured)
	}

	if _, err := c.GetNetwork(ctx, &GetNetworkRequest{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("GetNetwork error = %v, want %v", err, ErrNotConfigured)
	}

	if _, err := c.ListNetworks(ctx, &ListNetworksRequest{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("ListNetworks error = %v, want %v", err, ErrNotConfigured)
	}

	if err := c.Health(ctx, &HealthRequest{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Health error = %v, want %v", err, ErrNotConfigured)
	}
}

func TestNewClientEndpoint(t *testing.T) {
	t.Setenv(enableHTTPClientEnv, "")
	c, err := NewClient("")
	if err != nil {
		t.Fatalf("NewClient empty endpoint error=%v", err)
	}
	if c == nil {
		t.Fatal("expected client, got nil")
	}

	c, err = NewClient("unix:///run/cube/network-agent.sock")
	if !errors.Is(err, ErrUnsupportedEndpoint) {
		t.Fatalf("NewClient unix endpoint err=%v, want=%v when HTTP gate is off", err, ErrUnsupportedEndpoint)
	}
	if c == nil {
		t.Fatal("expected fallback client, got nil")
	}

	t.Setenv(enableHTTPClientEnv, "1")
	c, err = NewClient("unix:///run/cube/network-agent.sock")
	if err != nil {
		t.Fatalf("NewClient unix endpoint error=%v", err)
	}
	if c == nil {
		t.Fatal("expected unix client, got nil")
	}

	c, err = NewClient("unix://")
	if !errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("NewClient invalid endpoint err=%v, want=%v", err, ErrInvalidEndpoint)
	}
	if c == nil {
		t.Fatal("expected fallback client, got nil")
	}

	c, err = NewClient("grpc+unix://")
	if !errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("NewClient invalid grpc endpoint err=%v, want=%v", err, ErrInvalidEndpoint)
	}
	if c == nil {
		t.Fatal("expected fallback client, got nil")
	}
}

func TestHTTPClientHealth(t *testing.T) {
	t.Setenv(enableHTTPClientEnv, "1")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	c, err := NewClient(ts.URL)
	if err != nil {
		t.Fatalf("NewClient http endpoint error=%v", err)
	}
	if err := c.Health(context.Background(), &HealthRequest{}); err != nil {
		t.Fatalf("Health error=%v", err)
	}
}

func TestUnixClientHealthAndEnsure(t *testing.T) {
	t.Setenv(enableHTTPClientEnv, "1")
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "network-agent.sock")

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix error=%v", err)
	}
	defer ln.Close()
	defer os.Remove(socketPath)

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/healthz":
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			case "/v1/network/ensure":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"sandboxID":"sb-1","networkHandle":"nh-1"}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}),
	}
	go func() {
		_ = srv.Serve(ln)
	}()
	defer srv.Close()

	c, err := NewClient("unix://" + socketPath)
	if err != nil {
		t.Fatalf("NewClient unix endpoint error=%v", err)
	}
	if err := c.Health(context.Background(), &HealthRequest{}); err != nil {
		t.Fatalf("Health error=%v", err)
	}
	resp, err := c.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sb-1"})
	if err != nil {
		t.Fatalf("EnsureNetwork error=%v", err)
	}
	if resp.NetworkHandle != "nh-1" {
		t.Fatalf("EnsureNetwork networkHandle=%q, want=%q", resp.NetworkHandle, "nh-1")
	}
}

func TestGRPCUnixClientHealth(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "network-agent-grpc.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix error=%v", err)
	}
	defer ln.Close()
	defer os.Remove(socketPath)

	srv := &testNetworkAgentServer{}
	grpcServer := grpc.NewServer()
	networkagentv1.RegisterNetworkAgentServer(grpcServer, srv)
	go func() {
		_ = grpcServer.Serve(ln)
	}()
	defer grpcServer.Stop()

	c, err := NewClient("grpc+unix://" + socketPath)
	if err != nil {
		t.Fatalf("NewClient grpc+unix endpoint error=%v", err)
	}
	if err := c.Health(context.Background(), &HealthRequest{}); err != nil {
		t.Fatalf("Health error=%v", err)
	}
	ensureResp, err := c.EnsureNetwork(context.Background(), &EnsureNetworkRequest{
		SandboxID: "sb-grpc",
		CubeNetworkConfig: &CubeNetworkConfig{
			AllowInternetAccess: boolPtr(true),
			AllowOut:            []string{"1.1.1.1/32"},
			DenyOut:             []string{"10.0.0.0/8"},
		},
	})
	if err != nil {
		t.Fatalf("EnsureNetwork error=%v", err)
	}
	if ensureResp.NetworkHandle != "nh-grpc" {
		t.Fatalf("EnsureNetwork networkHandle=%q, want=%q", ensureResp.NetworkHandle, "nh-grpc")
	}
	if len(ensureResp.Interfaces) != 1 || ensureResp.Interfaces[0].Name != "ztap-grpc" {
		t.Fatalf("EnsureNetwork interfaces=%+v, want tap metadata", ensureResp.Interfaces)
	}
	if err := c.ReleaseNetwork(context.Background(), &ReleaseNetworkRequest{SandboxID: "sb-grpc", NetworkHandle: "nh-grpc"}); err != nil {
		t.Fatalf("ReleaseNetwork error=%v", err)
	}
	gotResp, err := c.GetNetwork(context.Background(), &GetNetworkRequest{SandboxID: "sb-grpc", NetworkHandle: "nh-grpc"})
	if err != nil {
		t.Fatalf("GetNetwork error=%v", err)
	}
	if gotResp.NetworkHandle != "nh-grpc" {
		t.Fatalf("GetNetwork networkHandle=%q, want=%q", gotResp.NetworkHandle, "nh-grpc")
	}
	listResp, err := c.ListNetworks(context.Background(), &ListNetworksRequest{})
	if err != nil {
		t.Fatalf("ListNetworks error=%v", err)
	}
	if len(listResp.Networks) != 1 {
		t.Fatalf("ListNetworks len=%d, want 1", len(listResp.Networks))
	}
	if listResp.Networks[0].TapName != "ztap-grpc" || listResp.Networks[0].TapIfIndex != 23 {
		t.Fatalf("ListNetworks[0]=%+v, want tap metadata", listResp.Networks[0])
	}
	if srv.lastEnsureReq == nil ||
		!srv.lastEnsureReq.GetCubeNetworkConfig().GetAllowInternetAccess() ||
		len(srv.lastEnsureReq.GetCubeNetworkConfig().GetAllowOut()) != 1 ||
		srv.lastEnsureReq.GetCubeNetworkConfig().GetAllowOut()[0] != "1.1.1.1/32" ||
		len(srv.lastEnsureReq.GetCubeNetworkConfig().GetDenyOut()) != 1 ||
		srv.lastEnsureReq.GetCubeNetworkConfig().GetDenyOut()[0] != "10.0.0.0/8" {
		t.Fatalf("server lastEnsureReq=%+v, want cube network config", srv.lastEnsureReq)
	}
}

func TestReconnectingClientReconnectsAfterFailure(t *testing.T) {
	oldFactory := concreteClientFactory
	t.Cleanup(func() {
		concreteClientFactory = oldFactory
	})

	var builds int32
	concreteClientFactory = func(string) (Client, error) {
		n := atomic.AddInt32(&builds, 1)
		if n == 1 {
			return &testReconnectClient{ensureErr: ErrNotConfigured}, nil
		}
		return &testReconnectClient{
			ensureResp: &EnsureNetworkResponse{
				SandboxID:     "sb-reconnect",
				NetworkHandle: "nh-reconnect",
			},
		}, nil
	}

	rc := &reconnectingClient{endpoint: "grpc+unix:///tmp/network-agent-grpc.sock"}
	resp, err := rc.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sb-reconnect"})
	if err != nil {
		t.Fatalf("EnsureNetwork error=%v", err)
	}
	if resp.NetworkHandle != "nh-reconnect" {
		t.Fatalf("EnsureNetwork networkHandle=%q, want=%q", resp.NetworkHandle, "nh-reconnect")
	}
	if got := atomic.LoadInt32(&builds); got != 2 {
		t.Fatalf("concreteClientFactory called %d times, want 2", got)
	}
}

func boolPtr(v bool) *bool {
	return &v
}

type testNetworkAgentServer struct {
	networkagentv1.UnimplementedNetworkAgentServer
	lastEnsureReq *networkagentv1.EnsureNetworkRequest
}

type testReconnectClient struct {
	ensureResp *EnsureNetworkResponse
	ensureErr  error
}

func (c *testReconnectClient) EnsureNetwork(context.Context, *EnsureNetworkRequest) (*EnsureNetworkResponse, error) {
	return c.ensureResp, c.ensureErr
}

func (c *testReconnectClient) ReleaseNetwork(context.Context, *ReleaseNetworkRequest) error {
	return nil
}

func (c *testReconnectClient) ReconcileNetwork(context.Context, *ReconcileNetworkRequest) (*ReconcileNetworkResponse, error) {
	return nil, nil
}

func (c *testReconnectClient) GetNetwork(context.Context, *GetNetworkRequest) (*GetNetworkResponse, error) {
	return nil, nil
}

func (c *testReconnectClient) ListNetworks(context.Context, *ListNetworksRequest) (*ListNetworksResponse, error) {
	return nil, nil
}

func (c *testReconnectClient) Health(context.Context, *HealthRequest) error {
	return c.ensureErr
}

func (s *testNetworkAgentServer) EnsureNetwork(ctx context.Context, req *networkagentv1.EnsureNetworkRequest) (*networkagentv1.EnsureNetworkResponse, error) {
	s.lastEnsureReq = req
	return &networkagentv1.EnsureNetworkResponse{
		SandboxId:     req.GetSandboxId(),
		NetworkHandle: "nh-grpc",
		Interfaces: []*networkagentv1.Interface{{
			Name:       "ztap-grpc",
			MacAddress: "20:90:6f:fc:fc:fc",
			IpCidrs:    []string{"169.254.68.6/30"},
			Gateway:    "169.254.68.5",
			Mtu:        1500,
		}},
		PortMappings: []*networkagentv1.PortMapping{{
			Protocol:      "tcp",
			HostIp:        "127.0.0.1",
			HostPort:      65000,
			ContainerPort: 80,
		}},
		PersistMetadata: map[string]string{"sandbox_ip": "192.168.0.10"},
	}, nil
}

func (s *testNetworkAgentServer) ReleaseNetwork(ctx context.Context, req *networkagentv1.ReleaseNetworkRequest) (*networkagentv1.ReleaseNetworkResponse, error) {
	return &networkagentv1.ReleaseNetworkResponse{
		Released: true,
	}, nil
}

func (s *testNetworkAgentServer) ReconcileNetwork(ctx context.Context, req *networkagentv1.ReconcileNetworkRequest) (*networkagentv1.ReconcileNetworkResponse, error) {
	return &networkagentv1.ReconcileNetworkResponse{
		SandboxId:     req.GetSandboxId(),
		NetworkHandle: req.GetNetworkHandle(),
		Converged:     true,
	}, nil
}

func (s *testNetworkAgentServer) GetNetwork(ctx context.Context, req *networkagentv1.GetNetworkRequest) (*networkagentv1.GetNetworkResponse, error) {
	return &networkagentv1.GetNetworkResponse{
		SandboxId:       req.GetSandboxId(),
		NetworkHandle:   "nh-grpc",
		Interfaces:      []*networkagentv1.Interface{{Name: "ztap-grpc"}},
		PersistMetadata: map[string]string{"sandbox_ip": "192.168.0.10"},
	}, nil
}

func (s *testNetworkAgentServer) Health(ctx context.Context, req *networkagentv1.HealthRequest) (*networkagentv1.HealthResponse, error) {
	return &networkagentv1.HealthResponse{
		Ok:     true,
		Status: "ok",
	}, nil
}

func (s *testNetworkAgentServer) ListNetworks(ctx context.Context, req *networkagentv1.ListNetworksRequest) (*networkagentv1.ListNetworksResponse, error) {
	return &networkagentv1.ListNetworksResponse{
		Networks: []*networkagentv1.NetworkState{{
			SandboxId:     "sb-grpc",
			NetworkHandle: "nh-grpc",
			TapName:       "ztap-grpc",
			TapIfindex:    23,
			SandboxIp:     "192.168.0.10",
			PortMappings: []*networkagentv1.PortMapping{{
				Protocol:      "tcp",
				HostIp:        "127.0.0.1",
				HostPort:      65000,
				ContainerPort: 80,
			}},
		}},
	}, nil
}
