// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package webhook

// ProtocolEventData is a background mapping result, not a Capture input.
// Callers must not mutate it during Encode; Encode retains no references to it.
type ProtocolEventData struct {
	SandboxMetadata map[string]string
	Execution       *ExecutionData
	SetTimeout      *string
	NetworkEgress   *NetworkEgressData
	PauseMode       string
	ResumeMode      string
	KillReason      string
}

type ExecutionData struct {
	StartedAt       string `json:"started_at"`
	VCPUCount       *int64 `json:"vcpu_count,omitempty"`
	MemoryMB        *int64 `json:"memory_mb,omitempty"`
	ExecutionTimeMS *int64 `json:"execution_time,omitempty"`
}

type NetworkEgressData struct {
	AllowedCIDRs   []string `json:"allowed_cidrs"`
	DeniedCIDRs    []string `json:"denied_cidrs"`
	AllowedDomains []string `json:"allowed_domains"`
}

// MapToProtocol runs in the Writer, including resource parsing.
// Encode still rejects missing required modes or update data.
func MapToProtocol(c *CapturedEvent) (*ProtocolEventData, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	return mapLifecycle(c), nil
}
