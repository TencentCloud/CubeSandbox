// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

// EnsureNetworkRequest is Cubelet's desired network shape for one sandbox.
// SandboxID is also used as the network handle in the current embedded runtime.
type EnsureNetworkRequest struct {
	SandboxID         string             `json:"sandboxID,omitempty"`
	IdempotencyKey    string             `json:"idempotencyKey,omitempty"`
	Interfaces        []Interface        `json:"interfaces,omitempty"`
	Routes            []Route            `json:"routes,omitempty"`
	ARPNeighbors      []ARPNeighbor      `json:"arpNeighbors,omitempty"`
	PortMappings      []PortMapping      `json:"portMappings,omitempty"`
	CubeNetworkConfig *CubeNetworkConfig `json:"cubeNetworkConfig,omitempty"`
	// DNSAllowOutCIDRs are the resolver /32s the caller already folded into
	// CubeNetworkConfig.AllowOut. Recorded so a later policy update, which only
	// carries user-authored targets, can fold the same resolvers back in.
	DNSAllowOutCIDRs []string          `json:"dnsAllowOutCIDRs,omitempty"`
	PersistMetadata  map[string]string `json:"persistMetadata,omitempty"`
}

// EnsureNetworkResponse is the concrete network shape assigned by the runtime.
// It may include generated host ports and normalized interface defaults.
type EnsureNetworkResponse struct {
	SandboxID       string            `json:"sandboxID,omitempty"`
	NetworkHandle   string            `json:"networkHandle,omitempty"`
	Interfaces      []Interface       `json:"interfaces,omitempty"`
	Routes          []Route           `json:"routes,omitempty"`
	ARPNeighbors    []ARPNeighbor     `json:"arpNeighbors,omitempty"`
	PortMappings    []PortMapping     `json:"portMappings,omitempty"`
	PersistMetadata map[string]string `json:"persistMetadata,omitempty"`
}

// ReleaseNetworkRequest identifies a network by SandboxID or NetworkHandle and
// carries caller metadata through the idempotent release path.
type ReleaseNetworkRequest struct {
	SandboxID       string            `json:"sandboxID,omitempty"`
	NetworkHandle   string            `json:"networkHandle,omitempty"`
	IdempotencyKey  string            `json:"idempotencyKey,omitempty"`
	PersistMetadata map[string]string `json:"persistMetadata,omitempty"`
}

// UpdateNetworkPolicyRequest replaces the egress policy of a running sandbox.
// CubeNetworkConfig is the complete desired state, not a patch: an omitted or
// empty field clears whatever is currently installed.
type UpdateNetworkPolicyRequest struct {
	SandboxID         string             `json:"sandboxID,omitempty"`
	CubeNetworkConfig *CubeNetworkConfig `json:"cubeNetworkConfig,omitempty"`
	// DNSAllowOutCIDRs is a fallback resolver list, used only for sandboxes
	// created before the runtime started recording its own. See
	// NetworkController.UpdateNetworkPolicy.
	DNSAllowOutCIDRs []string `json:"dnsAllowOutCIDRs,omitempty"`
}

// ReleaseNetworkResponse confirms the release handoff and returns the metadata
// persisted at creation time when the network existed.
type ReleaseNetworkResponse struct {
	Released        bool              `json:"released,omitempty"`
	PersistMetadata map[string]string `json:"persistMetadata,omitempty"`
}

// ListTapsRequest asks the embedded network runtime for a diagnostic snapshot
// of every TAP known by TapPool, including non-Active states.
type ListTapsRequest struct{}

type ListTapsResponse struct {
	Taps        []TapState     `json:"taps,omitempty"`
	StateCounts map[string]int `json:"stateCounts,omitempty"`
}

// TapState exposes TapPool state for diagnostics and recovery visibility.
type TapState struct {
	SandboxID      string        `json:"sandboxID,omitempty"`
	NetworkHandle  string        `json:"networkHandle,omitempty"`
	TapName        string        `json:"tapName,omitempty"`
	TapIfIndex     int32         `json:"tapIfIndex,omitempty"`
	SandboxIP      string        `json:"sandboxIP,omitempty"`
	State          string        `json:"state,omitempty"`
	OwnerSandboxID string        `json:"ownerSandboxID,omitempty"`
	RetryCount     int           `json:"retryCount,omitempty"`
	LastError      string        `json:"lastError,omitempty"`
	PortMappings   []PortMapping `json:"portMappings,omitempty"`
}

// Interface describes the sandbox-facing interface returned to Cubelet.
type Interface struct {
	Name    string   `json:"name,omitempty"`
	MAC     string   `json:"mac,omitempty"`
	MTU     int32    `json:"mtu,omitempty"`
	IPs     []string `json:"ips,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
}

// Route describes a sandbox route entry returned to Cubelet.
type Route struct {
	Destination string `json:"destination,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	Device      string `json:"device,omitempty"`
}

// ARPNeighbor describes a static neighbor entry visible to the sandbox.
type ARPNeighbor struct {
	IP     string `json:"ip,omitempty"`
	MAC    string `json:"mac,omitempty"`
	Device string `json:"device,omitempty"`
}

// PortMapping maps a sandbox container port to a managed host port.
type PortMapping struct {
	Protocol      string `json:"protocol,omitempty"`
	HostIP        string `json:"hostIP,omitempty"`
	HostPort      int32  `json:"hostPort,omitempty"`
	ContainerPort int32  `json:"containerPort,omitempty"`
}

// CubeNetworkConfig is the combined L3/L4/L7 egress policy carried by sandbox
// creation. cubevs enforces the network-level pieces; CubeEgress receives the
// full L7 rules.
type CubeNetworkConfig struct {
	AllowInternetAccess *bool         `json:"allowInternetAccess,omitempty"`
	AllowOut            []string      `json:"allowOut,omitempty"`
	DenyOut             []string      `json:"denyOut,omitempty"`
	Rules               []*EgressRule `json:"rules,omitempty"`
}

// EgressRule is an L7 egress rule, evaluated first-match-wins.
//
// The embedded network runtime does not enforce these rules itself. Full rules are pushed to
// CubeEgress, while their network targets are also extracted into cubevs as L7
// allow targets so the eBPF datapath can permit the underlying IP/domain flow.
type EgressRule struct {
	Name   string            `json:"name"`
	Match  *EgressRuleMatch  `json:"match,omitempty"`
	Action *EgressRuleAction `json:"action,omitempty"`
}

// EgressRuleMatch holds the per-request match conditions for an EgressRule.
// All fields are optional; an empty match matches any request.
//
// Port and Scheme together control which TCP port CubeEgress intercepts on the
// sandbox side. When both are omitted the rule applies to the default set
// {80/http, 443/https}. When Port is set, Scheme MUST also be set to "http" or
// "https" — every rule for the same (host, port) tuple must agree on the
// scheme, because iptables can only steer a single tuple to one TPROXY
// listener.
type EgressRuleMatch struct {
	SNI    *string  `json:"sni,omitempty"`
	Host   *string  `json:"host,omitempty"`
	Method []string `json:"method,omitempty"`
	Path   *string  `json:"path,omitempty"`
	Scheme *string  `json:"scheme,omitempty"`
	Port   *int     `json:"port,omitempty"`
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
