// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package networkagentclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	networkagentv1 "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/networkagentclient/pb"
	"google.golang.org/grpc"
)

type grpcHealthClient struct {
	conn *grpc.ClientConn
	na   networkagentv1.NetworkAgentClient
}

func newGRPCClient(endpoint string) (Client, error) {
	target, err := grpcTargetFromEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, target, grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		return nil, err
	}
	return &grpcHealthClient{
		conn: conn,
		na:   networkagentv1.NewNetworkAgentClient(conn),
	}, nil
}

func grpcTargetFromEndpoint(endpoint string) (string, error) {
	switch {
	case strings.HasPrefix(endpoint, "grpc+unix://"):
		socketPath := strings.TrimPrefix(endpoint, "grpc+unix://")
		if strings.TrimSpace(socketPath) == "" {
			return "", ErrInvalidEndpoint
		}
		return "unix://" + socketPath, nil
	case strings.HasPrefix(endpoint, "grpc://"):
		addr := strings.TrimPrefix(endpoint, "grpc://")
		if strings.TrimSpace(addr) == "" {
			return "", ErrInvalidEndpoint
		}
		return addr, nil
	default:
		return "", ErrUnsupportedEndpoint
	}
}

func (c *grpcHealthClient) EnsureNetwork(ctx context.Context, req *EnsureNetworkRequest) (*EnsureNetworkResponse, error) {
	resp, err := c.na.EnsureNetwork(ctx, &networkagentv1.EnsureNetworkRequest{
		SandboxId:         req.SandboxID,
		IdempotencyKey:    req.IdempotencyKey,
		Interfaces:        mapInterfacesToProto(req.Interfaces),
		Routes:            mapRoutesToProto(req.Routes),
		ArpNeighbors:      mapARPNeighborsToProto(req.ARPNeighbors),
		PortMappings:      mapPortMappingsToProto(req.PortMappings),
		CubeNetworkConfig: mapCubeNetworkConfigToProto(req.CubeNetworkConfig),
		PersistMetadata:   req.PersistMetadata,
	})
	if err != nil {
		return nil, err
	}
	return &EnsureNetworkResponse{
		SandboxID:       resp.GetSandboxId(),
		NetworkHandle:   resp.GetNetworkHandle(),
		Interfaces:      mapInterfacesFromProto(resp.GetInterfaces()),
		Routes:          mapRoutesFromProto(resp.GetRoutes()),
		ARPNeighbors:    mapARPNeighborsFromProto(resp.GetArpNeighbors()),
		PortMappings:    mapPortMappingsFromProto(resp.GetPortMappings()),
		PersistMetadata: resp.GetPersistMetadata(),
	}, nil
}

func (c *grpcHealthClient) ReleaseNetwork(ctx context.Context, req *ReleaseNetworkRequest) error {
	_, err := c.na.ReleaseNetwork(ctx, &networkagentv1.ReleaseNetworkRequest{
		SandboxId:       req.SandboxID,
		NetworkHandle:   req.NetworkHandle,
		IdempotencyKey:  req.IdempotencyKey,
		PersistMetadata: req.PersistMetadata,
	})
	return err
}

func (c *grpcHealthClient) ReconcileNetwork(ctx context.Context, req *ReconcileNetworkRequest) (*ReconcileNetworkResponse, error) {
	resp, err := c.na.ReconcileNetwork(ctx, &networkagentv1.ReconcileNetworkRequest{
		SandboxId:         req.SandboxID,
		NetworkHandle:     req.NetworkHandle,
		IdempotencyKey:    req.IdempotencyKey,
		Interfaces:        mapInterfacesToProto(req.Interfaces),
		Routes:            mapRoutesToProto(req.Routes),
		ArpNeighbors:      mapARPNeighborsToProto(req.ARPNeighbors),
		PortMappings:      mapPortMappingsToProto(req.PortMappings),
		CubeNetworkConfig: mapCubeNetworkConfigToProto(req.CubeNetworkConfig),
		PersistMetadata:   req.PersistMetadata,
	})
	if err != nil {
		return nil, err
	}
	return &ReconcileNetworkResponse{
		SandboxID:       resp.GetSandboxId(),
		NetworkHandle:   resp.GetNetworkHandle(),
		Converged:       resp.GetConverged(),
		Interfaces:      mapInterfacesFromProto(resp.GetInterfaces()),
		Routes:          mapRoutesFromProto(resp.GetRoutes()),
		ARPNeighbors:    mapARPNeighborsFromProto(resp.GetArpNeighbors()),
		PortMappings:    mapPortMappingsFromProto(resp.GetPortMappings()),
		PersistMetadata: resp.GetPersistMetadata(),
	}, nil
}

func (c *grpcHealthClient) GetNetwork(ctx context.Context, req *GetNetworkRequest) (*GetNetworkResponse, error) {
	resp, err := c.na.GetNetwork(ctx, &networkagentv1.GetNetworkRequest{
		SandboxId:     req.SandboxID,
		NetworkHandle: req.NetworkHandle,
	})
	if err != nil {
		return nil, err
	}
	return &GetNetworkResponse{
		SandboxID:       resp.GetSandboxId(),
		NetworkHandle:   resp.GetNetworkHandle(),
		Interfaces:      mapInterfacesFromProto(resp.GetInterfaces()),
		Routes:          mapRoutesFromProto(resp.GetRoutes()),
		ARPNeighbors:    mapARPNeighborsFromProto(resp.GetArpNeighbors()),
		PortMappings:    mapPortMappingsFromProto(resp.GetPortMappings()),
		PersistMetadata: resp.GetPersistMetadata(),
	}, nil
}

func (c *grpcHealthClient) ListNetworks(ctx context.Context, req *ListNetworksRequest) (*ListNetworksResponse, error) {
	_ = req
	resp, err := c.na.ListNetworks(ctx, &networkagentv1.ListNetworksRequest{})
	if err != nil {
		return nil, err
	}
	return &ListNetworksResponse{Networks: mapNetworkStatesFromProto(resp.GetNetworks())}, nil
}

func (c *grpcHealthClient) Health(ctx context.Context, _ *HealthRequest) error {
	resp, err := c.na.Health(ctx, &networkagentv1.HealthRequest{})
	if err != nil {
		return err
	}
	if !resp.GetOk() {
		return fmt.Errorf("grpc health not ok: %s", resp.GetStatus())
	}
	return nil
}

func (c *grpcHealthClient) close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func mapInterfacesToProto(in []Interface) []*networkagentv1.Interface {
	out := make([]*networkagentv1.Interface, 0, len(in))
	for _, item := range in {
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

func mapInterfacesFromProto(in []*networkagentv1.Interface) []Interface {
	out := make([]Interface, 0, len(in))
	for _, item := range in {
		out = append(out, Interface{
			Name:    item.GetName(),
			MAC:     item.GetMacAddress(),
			IPs:     item.GetIpCidrs(),
			Gateway: item.GetGateway(),
			MTU:     item.GetMtu(),
		})
	}
	return out
}

func mapRoutesToProto(in []Route) []*networkagentv1.Route {
	out := make([]*networkagentv1.Route, 0, len(in))
	for _, item := range in {
		out = append(out, &networkagentv1.Route{
			DestinationCidr: item.Destination,
			Gateway:         item.Gateway,
			Device:          item.Device,
		})
	}
	return out
}

func mapRoutesFromProto(in []*networkagentv1.Route) []Route {
	out := make([]Route, 0, len(in))
	for _, item := range in {
		out = append(out, Route{
			Destination: item.GetDestinationCidr(),
			Gateway:     item.GetGateway(),
			Device:      item.GetDevice(),
		})
	}
	return out
}

func mapARPNeighborsToProto(in []ARPNeighbor) []*networkagentv1.ARPNeighbor {
	out := make([]*networkagentv1.ARPNeighbor, 0, len(in))
	for _, item := range in {
		out = append(out, &networkagentv1.ARPNeighbor{
			Ip:         item.IP,
			MacAddress: item.MAC,
			Device:     item.Device,
		})
	}
	return out
}

func mapARPNeighborsFromProto(in []*networkagentv1.ARPNeighbor) []ARPNeighbor {
	out := make([]ARPNeighbor, 0, len(in))
	for _, item := range in {
		out = append(out, ARPNeighbor{
			IP:     item.GetIp(),
			MAC:    item.GetMacAddress(),
			Device: item.GetDevice(),
		})
	}
	return out
}

func mapPortMappingsToProto(in []PortMapping) []*networkagentv1.PortMapping {
	out := make([]*networkagentv1.PortMapping, 0, len(in))
	for _, item := range in {
		out = append(out, &networkagentv1.PortMapping{
			Protocol:      item.Protocol,
			ContainerPort: uint32(item.ContainerPort),
			HostPort:      uint32(item.HostPort),
			HostIp:        item.HostIP,
		})
	}
	return out
}

func mapPortMappingsFromProto(in []*networkagentv1.PortMapping) []PortMapping {
	out := make([]PortMapping, 0, len(in))
	for _, item := range in {
		out = append(out, PortMapping{
			Protocol:      item.GetProtocol(),
			ContainerPort: int32(item.GetContainerPort()),
			HostPort:      int32(item.GetHostPort()),
			HostIP:        item.GetHostIp(),
		})
	}
	return out
}

func mapCubeNetworkConfigToProto(in *CubeNetworkConfig) *networkagentv1.CubeNetworkConfig {
	if in == nil {
		return nil
	}
	out := &networkagentv1.CubeNetworkConfig{
		AllowInternetAccess: in.AllowInternetAccess,
		AllowOut:            in.AllowOut,
		DenyOut:             in.DenyOut,
		Rules:               mapEgressRulesToProto(in.Rules),
	}
	return out
}

func mapEgressRulesToProto(in []*EgressRule) []*networkagentv1.EgressRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]*networkagentv1.EgressRule, 0, len(in))
	for _, r := range in {
		if r == nil {
			continue
		}
		out = append(out, &networkagentv1.EgressRule{
			Name:   r.Name,
			Match:  mapEgressRuleMatchToProto(r.Match),
			Action: mapEgressRuleActionToProto(r.Action),
		})
	}
	return out
}

func mapEgressRuleMatchToProto(in *EgressRuleMatch) *networkagentv1.EgressRuleMatch {
	if in == nil {
		return nil
	}
	return &networkagentv1.EgressRuleMatch{
		Sni:    in.SNI,
		Host:   in.Host,
		Method: append([]string(nil), in.Method...),
		Path:   in.Path,
		Scheme: in.Scheme,
	}
}

func mapEgressRuleActionToProto(in *EgressRuleAction) *networkagentv1.EgressRuleAction {
	if in == nil {
		return nil
	}
	out := &networkagentv1.EgressRuleAction{
		Allow: in.Allow,
		Audit: in.Audit,
	}
	if len(in.Inject) > 0 {
		out.Inject = make([]*networkagentv1.EgressRuleInject, 0, len(in.Inject))
		for _, inj := range in.Inject {
			if inj == nil {
				continue
			}
			out.Inject = append(out.Inject, &networkagentv1.EgressRuleInject{
				Header: inj.Header,
				Secret: inj.Secret,
				Format: inj.Format,
			})
		}
	}
	return out
}

func mapNetworkStatesFromProto(in []*networkagentv1.NetworkState) []NetworkState {
	out := make([]NetworkState, 0, len(in))
	for _, item := range in {
		out = append(out, NetworkState{
			SandboxID:     item.GetSandboxId(),
			NetworkHandle: item.GetNetworkHandle(),
			TapName:       item.GetTapName(),
			TapIfIndex:    item.GetTapIfindex(),
			SandboxIP:     item.GetSandboxIp(),
			PortMappings:  mapPortMappingsFromProto(item.GetPortMappings()),
		})
	}
	return out
}
