// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package types

type SandboxProxyMap struct {
	HostIP      string `json:"HostIP"`
	SandboxID   string `json:"SandboxID"`
	SandboxIP   string `json:"SandboxIP,omitempty"`
	SandboxPort string `json:"SandboxPort,omitempty"`

	CreatedAt            string            `json:"CreatedAt,omitempty"`
	ContainerToHostPorts map[string]string `json:"ContainerToHostPorts,omitempty"`

	// AllowPublicTraffic gates the per-sandbox public URL: when false,
	// CubeProxy must reject any request that does not carry a matching
	// TrafficAccessToken header. Default true preserves the historical
	// "publicly reachable" behavior. Stored in Redis as the literal string
	// "true" / "false" so the Lua side can branch on it without typing
	// ambiguity.
	AllowPublicTraffic bool `json:"AllowPublicTraffic"`

	// TrafficAccessToken is the per-sandbox secret CubeProxy compares
	// against the e2b-traffic-access-token / cube-traffic-access-token
	// request headers. Generated only when AllowPublicTraffic=false; empty
	// otherwise. Lifecycle is bound to the sandbox — no rotation, cleared
	// when the proxy entry is deleted.
	TrafficAccessToken string `json:"TrafficAccessToken,omitempty"`
}
