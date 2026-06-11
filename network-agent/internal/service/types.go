// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

type EnsureNetworkRequest struct {
	SandboxID         string             `json:"sandboxID,omitempty"`
	IdempotencyKey    string             `json:"idempotencyKey,omitempty"`
	Interfaces        []Interface        `json:"interfaces,omitempty"`
	Routes            []Route            `json:"routes,omitempty"`
	ARPNeighbors      []ARPNeighbor      `json:"arpNeighbors,omitempty"`
	PortMappings      []PortMapping      `json:"portMappings,omitempty"`
	CubeNetworkConfig *CubeNetworkConfig `json:"cubeNetworkConfig,omitempty"`
	PersistMetadata   map[string]string  `json:"persistMetadata,omitempty"`
}

type EnsureNetworkResponse struct {
	SandboxID       string            `json:"sandboxID,omitempty"`
	NetworkHandle   string            `json:"networkHandle,omitempty"`
	Interfaces      []Interface       `json:"interfaces,omitempty"`
	Routes          []Route           `json:"routes,omitempty"`
	ARPNeighbors    []ARPNeighbor     `json:"arpNeighbors,omitempty"`
	PortMappings    []PortMapping     `json:"portMappings,omitempty"`
	PersistMetadata map[string]string `json:"persistMetadata,omitempty"`
}

type ReleaseNetworkRequest struct {
	SandboxID       string            `json:"sandboxID,omitempty"`
	NetworkHandle   string            `json:"networkHandle,omitempty"`
	IdempotencyKey  string            `json:"idempotencyKey,omitempty"`
	PersistMetadata map[string]string `json:"persistMetadata,omitempty"`
}

type ReleaseNetworkResponse struct {
	Released        bool              `json:"released,omitempty"`
	PersistMetadata map[string]string `json:"persistMetadata,omitempty"`
}

type ReconcileNetworkRequest struct {
	SandboxID         string             `json:"sandboxID,omitempty"`
	NetworkHandle     string             `json:"networkHandle,omitempty"`
	IdempotencyKey    string             `json:"idempotencyKey,omitempty"`
	Interfaces        []Interface        `json:"interfaces,omitempty"`
	Routes            []Route            `json:"routes,omitempty"`
	ARPNeighbors      []ARPNeighbor      `json:"arpNeighbors,omitempty"`
	PortMappings      []PortMapping      `json:"portMappings,omitempty"`
	CubeNetworkConfig *CubeNetworkConfig `json:"cubeNetworkConfig,omitempty"`
	PersistMetadata   map[string]string  `json:"persistMetadata,omitempty"`
}

type ReconcileNetworkResponse struct {
	SandboxID       string            `json:"sandboxID,omitempty"`
	NetworkHandle   string            `json:"networkHandle,omitempty"`
	Converged       bool              `json:"converged,omitempty"`
	Interfaces      []Interface       `json:"interfaces,omitempty"`
	Routes          []Route           `json:"routes,omitempty"`
	ARPNeighbors    []ARPNeighbor     `json:"arpNeighbors,omitempty"`
	PortMappings    []PortMapping     `json:"portMappings,omitempty"`
	PersistMetadata map[string]string `json:"persistMetadata,omitempty"`
}

type GetNetworkRequest struct {
	SandboxID     string `json:"sandboxID,omitempty"`
	NetworkHandle string `json:"networkHandle,omitempty"`
}

type GetNetworkResponse struct {
	SandboxID       string            `json:"sandboxID,omitempty"`
	NetworkHandle   string            `json:"networkHandle,omitempty"`
	Interfaces      []Interface       `json:"interfaces,omitempty"`
	Routes          []Route           `json:"routes,omitempty"`
	ARPNeighbors    []ARPNeighbor     `json:"arpNeighbors,omitempty"`
	PortMappings    []PortMapping     `json:"portMappings,omitempty"`
	PersistMetadata map[string]string `json:"persistMetadata,omitempty"`
}

type ListNetworksRequest struct{}

type ListNetworksResponse struct {
	Networks []NetworkState `json:"networks,omitempty"`
}

type NetworkState struct {
	SandboxID     string        `json:"sandboxID,omitempty"`
	NetworkHandle string        `json:"networkHandle,omitempty"`
	TapName       string        `json:"tapName,omitempty"`
	TapIfIndex    int32         `json:"tapIfIndex,omitempty"`
	SandboxIP     string        `json:"sandboxIP,omitempty"`
	PortMappings  []PortMapping `json:"portMappings,omitempty"`
}

type Interface struct {
	Name    string   `json:"name,omitempty"`
	MAC     string   `json:"mac,omitempty"`
	MTU     int32    `json:"mtu,omitempty"`
	IPs     []string `json:"ips,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
}

type Route struct {
	Destination string `json:"destination,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	Device      string `json:"device,omitempty"`
}

type ARPNeighbor struct {
	IP     string `json:"ip,omitempty"`
	MAC    string `json:"mac,omitempty"`
	Device string `json:"device,omitempty"`
}

type PortMapping struct {
	Protocol      string `json:"protocol,omitempty"`
	HostIP        string `json:"hostIP,omitempty"`
	HostPort      int32  `json:"hostPort,omitempty"`
	ContainerPort int32  `json:"containerPort,omitempty"`
}

type CubeNetworkConfig struct {
	AllowInternetAccess *bool         `json:"allowInternetAccess,omitempty"`
	AllowOut            []string      `json:"allowOut,omitempty"`
	DenyOut             []string      `json:"denyOut,omitempty"`
	Rules               []*EgressRule `json:"rules,omitempty"`
}

// EgressRule is an L7 egress rule, evaluated first-match-wins.
//
// network-agent does not enforce these rules itself. Full rules are pushed to
// CubeEgress, while their network targets are also extracted into cubevs as L7
// allow targets so the eBPF datapath can permit the underlying IP/domain flow.
type EgressRule struct {
	Name   string            `json:"name"`
	Match  *EgressRuleMatch  `json:"match,omitempty"`
	Action *EgressRuleAction `json:"action,omitempty"`
}

// EgressRuleMatch holds the per-request match conditions for an EgressRule.
// All fields are optional; an empty match matches any request.
type EgressRuleMatch struct {
	SNI    *string  `json:"sni,omitempty"`
	Host   *string  `json:"host,omitempty"`
	Method []string `json:"method,omitempty"`
	Path   *string  `json:"path,omitempty"`
	Scheme *string  `json:"scheme,omitempty"`
}

// EgressRuleAction holds the action taken when an EgressRule matches.
type EgressRuleAction struct {
	Allow  bool                `json:"allow"`
	Audit  *string             `json:"audit,omitempty"`
	Inject []*EgressRuleInject `json:"inject,omitempty"`
}

// EgressRuleInject is a credential injection.
type EgressRuleInject struct {
	Header string  `json:"header"`
	Secret string  `json:"secret"`
	Format *string `json:"format,omitempty"`
}
