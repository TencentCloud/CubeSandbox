// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package types

type SandboxProxyMap struct {
	HostIP      string `json:"HostIP"`
	SandboxID   string `json:"SandboxID"`
	SandboxIP   string `json:"SandboxIP,omitempty"`
	SandboxPort string `json:"SandboxPort,omitempty"`

	CreatedAt string `json:"CreatedAt,omitempty"`
	// EndAt is the absolute deadline (Unix nanoseconds, as decimal string)
	// after which the TTL reaper will issue a DestroySandbox.
	// Empty / "0" means the sandbox has no TTL and will live until killed
	// explicitly via DELETE /sandboxes/{id}.
	EndAt                string            `json:"EndAt,omitempty"`
	ContainerToHostPorts map[string]string `json:"ContainerToHostPorts,omitempty"`
}
