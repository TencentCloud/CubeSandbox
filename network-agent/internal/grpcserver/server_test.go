// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package grpcserver

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	networkagentv1 "github.com/tencentcloud/CubeSandbox/network-agent/api/v1"
	"github.com/tencentcloud/CubeSandbox/network-agent/internal/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestUnixEndpointHealth(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "network-agent-grpc.sock")
	srv, err := New("unix://"+socketPath, service.NewNoopService())
	if err != nil {
		t.Fatalf("New error=%v", err)
	}

	done := make(chan struct{})
	go func() {
		_ = srv.Start()
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)

	conn, err := grpc.DialContext(
		context.Background(),
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatalf("grpc dial error=%v", err)
	}
	defer conn.Close()

	client := healthpb.NewHealthClient(conn)
	resp, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health check error=%v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health status=%v, want=%v", resp.GetStatus(), healthpb.HealthCheckResponse_SERVING)
	}

	naClient := networkagentv1.NewNetworkAgentClient(conn)
	ensureResp, err := naClient.EnsureNetwork(context.Background(), &networkagentv1.EnsureNetworkRequest{
		SandboxId: "sb-1",
	})
	if err != nil {
		t.Fatalf("ensure network error=%v", err)
	}
	if ensureResp.GetNetworkHandle() != "sb-1" {
		t.Fatalf("ensure network handle=%q, want=%q", ensureResp.GetNetworkHandle(), "sb-1")
	}
	listResp, err := naClient.ListNetworks(context.Background(), &networkagentv1.ListNetworksRequest{})
	if err != nil {
		t.Fatalf("list networks error=%v", err)
	}
	if len(listResp.GetNetworks()) != 0 {
		t.Fatalf("list networks len=%d, want 0", len(listResp.GetNetworks()))
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Stop(stopCtx); err != nil {
		t.Fatalf("Stop error=%v", err)
	}
	<-done
}

func TestMapCubeNetworkConfigFromProtoDNSEnforceQuery(t *testing.T) {
	t.Run("enforce flows through", func(t *testing.T) {
		enforce := true
		out := mapCubeNetworkConfigFromProto(&networkagentv1.CubeNetworkConfig{
			AllowOut:        []string{"api.example.com"},
			DnsEnforceQuery: &enforce,
		})
		if out == nil || out.DnsEnforceQuery == nil || !*out.DnsEnforceQuery {
			t.Fatalf("DnsEnforceQuery=%v, want true", out)
		}
	})
	t.Run("nil stays nil", func(t *testing.T) {
		out := mapCubeNetworkConfigFromProto(&networkagentv1.CubeNetworkConfig{
			AllowOut: []string{"api.example.com"},
		})
		if out == nil || out.DnsEnforceQuery != nil {
			t.Fatalf("DnsEnforceQuery=%v, want nil", out)
		}
	})
}
