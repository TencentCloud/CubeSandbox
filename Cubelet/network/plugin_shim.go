// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"encoding/base64"
	"fmt"
	"net"

	networkruntime "github.com/tencentcloud/CubeSandbox/Cubelet/network/runtime"
	. "github.com/tencentcloud/CubeSandbox/Cubelet/network/types"
)

// buildEnsureNetworkRequestFromIntent converts the old workflow/shim intent into
// the declarative runtime request. It keeps guest-side defaults such as eth0,
// gateway ARP and loopback host-port bindings in one place.
func (l *local) buildEnsureNetworkRequestFromIntent(sandboxID, requestID string, exposedPorts []int64, shimReq *NetRequest, cubeNetworkConfig *networkruntime.CubeNetworkConfig, dnsAllowOutCIDRs []string) *networkruntime.EnsureNetworkRequest {
	desired := &networkruntime.EnsureNetworkRequest{
		SandboxID:      sandboxID,
		IdempotencyKey: requestID,
		Interfaces: []networkruntime.Interface{
			{
				MAC:     l.Config.MVMMacAddr,
				MTU:     int32(l.Config.MvmMtu),
				IPs:     []string{fmt.Sprintf("%s/%d", l.Config.MVMInnerIP, l.Config.MvmMask)},
				Gateway: l.Config.MvmGwDestIP,
			},
		},
		Routes: []networkruntime.Route{
			{
				Gateway: l.Config.MvmGwDestIP,
				Device:  eth0,
			},
		},
		ARPNeighbors: []networkruntime.ARPNeighbor{
			{
				IP:     l.Config.MvmGwDestIP,
				MAC:    l.Config.MvmGwMacAddr,
				Device: eth0,
			},
		},
	}
	desired.CubeNetworkConfig = cubeNetworkConfig
	// Recorded, not re-derived: a later policy update carries only user-authored
	// targets and must fold these same resolvers back in.
	desired.DNSAllowOutCIDRs = dnsAllowOutCIDRs
	portReq := make(map[uint16]struct{})
	for _, port := range exposedPorts {
		portReq[uint16(port)] = struct{}{}
	}
	for reqPort := range portReq {
		desired.PortMappings = append(desired.PortMappings, networkruntime.PortMapping{
			Protocol:      "tcp",
			HostIP:        "127.0.0.1",
			ContainerPort: int32(reqPort),
		})
	}
	desired.PersistMetadata = buildPersistMetadataMap(nil, nil)
	desired.PersistMetadata["gateway_ip"] = l.Config.MvmGwDestIP
	desired.PersistMetadata["mvm_inner_ip"] = l.Config.MVMInnerIP
	if shimReq != nil && shimReq.Qos != nil {
		desired.PersistMetadata["qos_enabled"] = "true"
	}
	return desired
}

// buildShimNetReqFromEnsureResponse adapts runtime output back to the legacy shim
// request format. The shim still consumes routes, ARP neighbors and port mappings
// from ShimNetReq, even though their source of truth is now the runtime response.
func (l *local) buildShimNetReqFromEnsureResponse(resp *networkruntime.EnsureNetworkResponse) (*ShimNetReq, error) {
	if resp == nil {
		return nil, fmt.Errorf("network runtime response is nil")
	}
	if len(resp.Interfaces) == 0 {
		return nil, fmt.Errorf("network runtime returned no interfaces")
	}
	sandboxIP := net.ParseIP(resp.PersistMetadata["sandbox_ip"]).To4()
	if sandboxIP == nil {
		return nil, fmt.Errorf("network runtime response missing sandbox_ip")
	}
	intf, legacyIP, err := l.buildShimInterface(resp.Interfaces[0], sandboxIP)
	if err != nil {
		return nil, err
	}
	shimReq := &ShimNetReq{
		Interfaces:   []*Interface{intf},
		Routes:       l.buildShimRoutes(resp.Routes, legacyIP),
		ARPs:         l.buildShimARPs(resp.ARPNeighbors),
		PortMappings: buildShimPortMappings(resp.PortMappings),
	}
	return shimReq, nil
}

func (l *local) buildShimInterface(intf networkruntime.Interface, sandboxIP net.IP) (*Interface, string, error) {
	mvmIPs := make([]MVMIp, 0, len(intf.IPs))
	legacyIP := l.Config.MVMInnerIP
	legacyMask := l.Config.MvmMask
	for _, cidr := range intf.IPs {
		ip, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, "", fmt.Errorf("parse interface cidr %q: %w", cidr, err)
		}
		mask, _ := network.Mask.Size()
		mvmIPs = append(mvmIPs, MVMIp{
			IP:     ip.String(),
			Mask:   mask,
			Family: 0,
		})
		if len(mvmIPs) == 1 {
			legacyIP = ip.String()
			legacyMask = mask
		}
	}
	return &Interface{
		Name:      intf.Name,
		IPAddr:    sandboxIP,
		GuestName: eth0,
		Mac:       intf.MAC,
		Mtu:       int(intf.MTU),
		IP:        legacyIP,
		Family:    0,
		Mask:      legacyMask,
		IPs:       mvmIPs,
	}, legacyIP, nil
}

func (l *local) buildShimRoutes(routes []networkruntime.Route, sourceIP string) []Route {
	if len(routes) == 0 {
		return []Route{{
			Family:  0,
			Gateway: l.Config.MvmGwDestIP,
			Source:  sourceIP,
			Device:  eth0,
			Scope:   0,
		}}
	}
	out := make([]Route, 0, len(routes))
	for _, route := range routes {
		out = append(out, Route{
			Family:  0,
			Dest:    route.Destination,
			Gateway: route.Gateway,
			Source:  sourceIP,
			Device:  route.Device,
			Scope:   0,
		})
	}
	return out
}

func (l *local) buildShimARPs(neighbors []networkruntime.ARPNeighbor) []ARP {
	if len(neighbors) == 0 {
		return []ARP{{
			DestIP: l.Config.MvmGwDestIP,
			Device: eth0,
			LlAddr: l.Config.MvmGwMacAddr,
		}}
	}
	out := make([]ARP, 0, len(neighbors))
	for _, arp := range neighbors {
		out = append(out, ARP{
			DestIP: arp.IP,
			Device: arp.Device,
			LlAddr: arp.MAC,
		})
	}
	return out
}

func buildShimPortMappings(mappings []networkruntime.PortMapping) []PortMapping {
	out := make([]PortMapping, 0, len(mappings))
	for _, pm := range mappings {
		out = append(out, PortMapping{
			HostPort:      uint16(pm.HostPort),
			ContainerPort: uint16(pm.ContainerPort),
		})
	}
	return out
}

func buildPersistMetadataMap(raw []byte, shimReq *ShimNetReq) map[string]string {
	meta := map[string]string{
		"shim_req_metadata_b64": base64.StdEncoding.EncodeToString(raw),
	}
	if shimReq != nil {
		if ip := shimReq.SandboxIP(); ip != "" {
			meta["sandbox_ip"] = ip
		}
		if gw := shimReq.GatewayIP(); gw != "" {
			meta["gateway_ip"] = gw
		}
	}
	return meta
}
