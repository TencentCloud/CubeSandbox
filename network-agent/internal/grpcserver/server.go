// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package grpcserver

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	networkagentv1 "github.com/tencentcloud/CubeSandbox/network-agent/api/v1"
	"github.com/tencentcloud/CubeSandbox/network-agent/internal/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Server is a minimal gRPC server exposing health check service.
type Server struct {
	server         *grpc.Server
	listener       net.Listener
	unixSocketPath string
}

// New creates a gRPC server from endpoint.
// Supported endpoint forms: unix://, tcp://, and raw host:port.
func New(endpoint string, svc service.Service) (*Server, error) {
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		return nil, fmt.Errorf("endpoint is empty")
	}

	var (
		ln             net.Listener
		unixSocketPath string
		err            error
	)
	switch {
	case strings.HasPrefix(ep, "unix://"):
		socketPath := strings.TrimPrefix(ep, "unix://")
		if socketPath == "" {
			return nil, fmt.Errorf("unix endpoint is invalid: %q", endpoint)
		}
		if err = os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
			return nil, err
		}
		if err = os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		ln, err = net.Listen("unix", socketPath)
		if err != nil {
			return nil, err
		}
		unixSocketPath = socketPath
	case strings.HasPrefix(ep, "tcp://"):
		addr := strings.TrimPrefix(ep, "tcp://")
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
	default:
		ln, err = net.Listen("tcp", ep)
		if err != nil {
			return nil, err
		}
	}

	grpcServer := grpc.NewServer()
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	networkagentv1.RegisterNetworkAgentServer(grpcServer, &networkAgentServer{svc: svc})

	return &Server{
		server:         grpcServer,
		listener:       ln,
		unixSocketPath: unixSocketPath,
	}, nil
}

type networkAgentServer struct {
	networkagentv1.UnimplementedNetworkAgentServer
	svc service.Service
}

func (s *networkAgentServer) EnsureNetwork(ctx context.Context, req *networkagentv1.EnsureNetworkRequest) (*networkagentv1.EnsureNetworkResponse, error) {
	resp, err := s.svc.EnsureNetwork(ctx, &service.EnsureNetworkRequest{
		SandboxID:         req.GetSandboxId(),
		IdempotencyKey:    req.GetIdempotencyKey(),
		Interfaces:        mapInterfacesFromProto(req.GetInterfaces()),
		Routes:            mapRoutesFromProto(req.GetRoutes()),
		ARPNeighbors:      mapARPNeighborsFromProto(req.GetArpNeighbors()),
		PortMappings:      mapPortMappingsFromProto(req.GetPortMappings()),
		CubeNetworkConfig: mapCubeNetworkConfigFromProto(req.GetCubeNetworkConfig()),
		PersistMetadata:   req.GetPersistMetadata(),
	})
	if err != nil {
		return nil, err
	}
	return &networkagentv1.EnsureNetworkResponse{
		SandboxId:       resp.SandboxID,
		NetworkHandle:   resp.NetworkHandle,
		Interfaces:      mapInterfacesToProto(resp.Interfaces),
		Routes:          mapRoutesToProto(resp.Routes),
		ArpNeighbors:    mapARPNeighborsToProto(resp.ARPNeighbors),
		PortMappings:    mapPortMappingsToProto(resp.PortMappings),
		PersistMetadata: resp.PersistMetadata,
	}, nil
}

func (s *networkAgentServer) ReleaseNetwork(ctx context.Context, req *networkagentv1.ReleaseNetworkRequest) (*networkagentv1.ReleaseNetworkResponse, error) {
	resp, err := s.svc.ReleaseNetwork(ctx, &service.ReleaseNetworkRequest{
		SandboxID:       req.GetSandboxId(),
		NetworkHandle:   req.GetNetworkHandle(),
		IdempotencyKey:  req.GetIdempotencyKey(),
		PersistMetadata: req.GetPersistMetadata(),
	})
	if err != nil {
		return nil, err
	}
	return &networkagentv1.ReleaseNetworkResponse{
		Released:        resp.Released,
		PersistMetadata: resp.PersistMetadata,
	}, nil
}

func (s *networkAgentServer) ReconcileNetwork(ctx context.Context, req *networkagentv1.ReconcileNetworkRequest) (*networkagentv1.ReconcileNetworkResponse, error) {
	resp, err := s.svc.ReconcileNetwork(ctx, &service.ReconcileNetworkRequest{
		SandboxID:         req.GetSandboxId(),
		NetworkHandle:     req.GetNetworkHandle(),
		IdempotencyKey:    req.GetIdempotencyKey(),
		Interfaces:        mapInterfacesFromProto(req.GetInterfaces()),
		Routes:            mapRoutesFromProto(req.GetRoutes()),
		ARPNeighbors:      mapARPNeighborsFromProto(req.GetArpNeighbors()),
		PortMappings:      mapPortMappingsFromProto(req.GetPortMappings()),
		CubeNetworkConfig: mapCubeNetworkConfigFromProto(req.GetCubeNetworkConfig()),
		PersistMetadata:   req.GetPersistMetadata(),
	})
	if err != nil {
		return nil, err
	}
	return &networkagentv1.ReconcileNetworkResponse{
		SandboxId:       resp.SandboxID,
		NetworkHandle:   resp.NetworkHandle,
		Converged:       resp.Converged,
		Interfaces:      mapInterfacesToProto(resp.Interfaces),
		Routes:          mapRoutesToProto(resp.Routes),
		ArpNeighbors:    mapARPNeighborsToProto(resp.ARPNeighbors),
		PortMappings:    mapPortMappingsToProto(resp.PortMappings),
		PersistMetadata: resp.PersistMetadata,
	}, nil
}

func (s *networkAgentServer) GetNetwork(ctx context.Context, req *networkagentv1.GetNetworkRequest) (*networkagentv1.GetNetworkResponse, error) {
	resp, err := s.svc.GetNetwork(ctx, &service.GetNetworkRequest{
		SandboxID:     req.GetSandboxId(),
		NetworkHandle: req.GetNetworkHandle(),
	})
	if err != nil {
		return nil, err
	}
	return &networkagentv1.GetNetworkResponse{
		SandboxId:       resp.SandboxID,
		NetworkHandle:   resp.NetworkHandle,
		Interfaces:      mapInterfacesToProto(resp.Interfaces),
		Routes:          mapRoutesToProto(resp.Routes),
		ArpNeighbors:    mapARPNeighborsToProto(resp.ARPNeighbors),
		PortMappings:    mapPortMappingsToProto(resp.PortMappings),
		PersistMetadata: resp.PersistMetadata,
	}, nil
}

func (s *networkAgentServer) Health(ctx context.Context, req *networkagentv1.HealthRequest) (*networkagentv1.HealthResponse, error) {
	if err := s.svc.Health(ctx); err != nil {
		return &networkagentv1.HealthResponse{
			Ok:     false,
			Status: err.Error(),
		}, err
	}
	return &networkagentv1.HealthResponse{
		Ok:     true,
		Status: "ok",
	}, nil
}

func (s *networkAgentServer) ListNetworks(ctx context.Context, req *networkagentv1.ListNetworksRequest) (*networkagentv1.ListNetworksResponse, error) {
	resp, err := s.svc.ListNetworks(ctx, &service.ListNetworksRequest{})
	if err != nil {
		return nil, err
	}
	return &networkagentv1.ListNetworksResponse{
		Networks: mapNetworkStatesToProto(resp.Networks),
	}, nil
}

func mapInterfacesFromProto(items []*networkagentv1.Interface) []service.Interface {
	out := make([]service.Interface, 0, len(items))
	for _, item := range items {
		out = append(out, service.Interface{
			Name:    item.GetName(),
			MAC:     item.GetMacAddress(),
			IPs:     item.GetIpCidrs(),
			Gateway: item.GetGateway(),
			MTU:     item.GetMtu(),
		})
	}
	return out
}

func mapInterfacesToProto(items []service.Interface) []*networkagentv1.Interface {
	out := make([]*networkagentv1.Interface, 0, len(items))
	for _, item := range items {
		out = append(out, &networkagentv1.Interface{
			Name:       item.Name,
			MacAddress: item.MAC,
			IpCidrs:    item.IPs,
			Gateway:    item.Gateway,
			Mtu:        item.MTU,
		})
	}
	return out
}

func mapRoutesFromProto(items []*networkagentv1.Route) []service.Route {
	out := make([]service.Route, 0, len(items))
	for _, item := range items {
		out = append(out, service.Route{
			Destination: item.GetDestinationCidr(),
			Gateway:     item.GetGateway(),
			Device:      item.GetDevice(),
		})
	}
	return out
}

func mapRoutesToProto(items []service.Route) []*networkagentv1.Route {
	out := make([]*networkagentv1.Route, 0, len(items))
	for _, item := range items {
		out = append(out, &networkagentv1.Route{
			DestinationCidr: item.Destination,
			Gateway:         item.Gateway,
			Device:          item.Device,
		})
	}
	return out
}

func mapARPNeighborsFromProto(items []*networkagentv1.ARPNeighbor) []service.ARPNeighbor {
	out := make([]service.ARPNeighbor, 0, len(items))
	for _, item := range items {
		out = append(out, service.ARPNeighbor{
			IP:     item.GetIp(),
			MAC:    item.GetMacAddress(),
			Device: item.GetDevice(),
		})
	}
	return out
}

func mapARPNeighborsToProto(items []service.ARPNeighbor) []*networkagentv1.ARPNeighbor {
	out := make([]*networkagentv1.ARPNeighbor, 0, len(items))
	for _, item := range items {
		out = append(out, &networkagentv1.ARPNeighbor{
			Ip:         item.IP,
			MacAddress: item.MAC,
			Device:     item.Device,
		})
	}
	return out
}

func mapPortMappingsFromProto(items []*networkagentv1.PortMapping) []service.PortMapping {
	out := make([]service.PortMapping, 0, len(items))
	for _, item := range items {
		out = append(out, service.PortMapping{
			Protocol:      item.GetProtocol(),
			HostIP:        item.GetHostIp(),
			HostPort:      int32(item.GetHostPort()),
			ContainerPort: int32(item.GetContainerPort()),
		})
	}
	return out
}

func mapPortMappingsToProto(items []service.PortMapping) []*networkagentv1.PortMapping {
	out := make([]*networkagentv1.PortMapping, 0, len(items))
	for _, item := range items {
		out = append(out, &networkagentv1.PortMapping{
			Protocol:      item.Protocol,
			HostIp:        item.HostIP,
			HostPort:      uint32(item.HostPort),
			ContainerPort: uint32(item.ContainerPort),
		})
	}
	return out
}

func mapCubeNetworkConfigFromProto(item *networkagentv1.CubeNetworkConfig) *service.CubeNetworkConfig {
	if item == nil {
		return nil
	}
	out := &service.CubeNetworkConfig{
		AllowOut: item.GetAllowOut(),
		DenyOut:  item.GetDenyOut(),
		Rules:    mapEgressRulesFromProto(item.GetRules()),
	}
	if item.AllowInternetAccess != nil {
		v := item.GetAllowInternetAccess()
		out.AllowInternetAccess = &v
	}
	return out
}

func mapEgressRulesFromProto(items []*networkagentv1.EgressRule) []*service.EgressRule {
	if len(items) == 0 {
		return nil
	}
	out := make([]*service.EgressRule, 0, len(items))
	for _, r := range items {
		if r == nil {
			continue
		}
		out = append(out, &service.EgressRule{
			Name:   r.GetName(),
			Match:  mapEgressRuleMatchFromProto(r.GetMatch()),
			Action: mapEgressRuleActionFromProto(r.GetAction()),
		})
	}
	return out
}

func mapEgressRuleMatchFromProto(m *networkagentv1.EgressRuleMatch) *service.EgressRuleMatch {
	if m == nil {
		return nil
	}
	return &service.EgressRuleMatch{
		SNI:    m.Sni,
		Host:   m.Host,
		Method: append([]string(nil), m.GetMethod()...),
		Path:   m.Path,
		Scheme: m.Scheme,
	}
}

func mapEgressRuleActionFromProto(a *networkagentv1.EgressRuleAction) *service.EgressRuleAction {
	if a == nil {
		return nil
	}
	out := &service.EgressRuleAction{
		Allow: a.GetAllow(),
		Audit: a.Audit,
	}
	if len(a.GetInject()) > 0 {
		out.Inject = make([]*service.EgressRuleInject, 0, len(a.GetInject()))
		for _, inj := range a.GetInject() {
			if inj == nil {
				continue
			}
			out.Inject = append(out.Inject, &service.EgressRuleInject{
				Header: inj.GetHeader(),
				Secret: inj.GetSecret(),
				Format: inj.Format,
			})
		}
	}
	return out
}

func mapNetworkStatesToProto(items []service.NetworkState) []*networkagentv1.NetworkState {
	out := make([]*networkagentv1.NetworkState, 0, len(items))
	for _, item := range items {
		out = append(out, &networkagentv1.NetworkState{
			SandboxId:     item.SandboxID,
			NetworkHandle: item.NetworkHandle,
			TapName:       item.TapName,
			TapIfindex:    item.TapIfIndex,
			SandboxIp:     item.SandboxIP,
			PortMappings:  mapPortMappingsToProto(item.PortMappings),
		})
	}
	return out
}

// Start starts serving gRPC requests.
func (s *Server) Start() error {
	return s.server.Serve(s.listener)
}

// Stop gracefully stops the server and cleans unix socket.
func (s *Server) Stop(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.server.GracefulStop()
		close(done)
	}()
	select {
	case <-ctx.Done():
		s.server.Stop()
	case <-done:
	}
	if s.unixSocketPath != "" {
		_ = os.Remove(s.unixSocketPath)
	}
	return nil
}
