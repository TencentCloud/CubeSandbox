// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package types

// EvictionEvent carries the information extracted from an AdmissionReview
// when a sandbox Pod eviction is intercepted.
type EvictionEvent struct {
	// EventID equals AdmissionReview.request.uid — globally unique per eviction request.
	EventID string `json:"requestID"`
	// PodName is the K8s Pod name. It is NOT the CubeMaster SandboxID —
	// sandbox IDs are 32-char hex UUIDs, discovered via ListSandboxesByNode.
	PodName      string `json:"podName"`
	Namespace    string `json:"namespace"`
	NodeName     string `json:"nodeName"`
	InstanceType string `json:"instanceType"`
	// InterceptedAt is the RFC3339 timestamp when the webhook intercepted the request.
	InterceptedAt string `json:"interceptedAt"`
}

// ReportResponse is the expected CubeMaster response envelope for POST /event/eviction.
type ReportResponse struct {
	RequestID string      `json:"requestID"`
	Ret       *ReportRet  `json:"ret"`
}

type ReportRet struct {
	RetCode int    `json:"ret_code"`
	RetMsg  string `json:"ret_msg"`
}
