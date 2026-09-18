// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package model

import "time"

// QuotaSpec is the desired quota; zero/empty values mean unset (cubelet defaults).
type QuotaSpec struct {
	MCpuLimit             int64  `json:"mcpu_limit"`
	MemLimit              string `json:"mem_limit"`
	MvmLimit              int64  `json:"mvm_limit"`
	CreationConcurrentNum int64  `json:"creation_concurrent_num"`

	// PausedReleaseRatio: nil inherits the cluster default; any value is an explicit override.
	PausedReleaseRatio *float64 `json:"paused_resource_release_ratio,omitempty"`

	// ExpectedRevision enables CAS when non-zero; a mismatch rejects with ErrQuotaRevisionConflict.
	ExpectedRevision int64 `json:"expected_revision,omitempty"`

	// NodeManaged marks the row origin: true = all fields authoritative, false = ratio only.
	NodeManaged bool `json:"node_managed,omitempty"`

	Revision  int64     `json:"revision"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Set reports whether any desired field is set (non-zero / non-nil).
func (s *QuotaSpec) Set() bool {
	return s != nil && (s.MCpuLimit > 0 || s.MemLimit != "" || s.MvmLimit > 0 ||
		s.CreationConcurrentNum > 0 || s.PausedReleaseRatio != nil)
}

// QuotaActual is the effective quota as reported by cubelet on heartbeat.
type QuotaActual struct {
	MilliCPU            int64 `json:"milli_cpu"`
	MemMB               int64 `json:"mem_mb"`
	MaxMvmNum           int64 `json:"max_mvm_num"`
	CreateConcurrentNum int64 `json:"create_concurrent_num"`
	// PausedReleaseRatio is the effective ratio; nil = pre-reporting cubelet.
	PausedReleaseRatio *float64 `json:"paused_release_ratio,omitempty"`
}

// DriftState describes the relation between the desired spec and the actual.
type DriftState string

const (
	// DriftNone: every field set in the spec matches the actual report.
	DriftNone DriftState = "none"
	// DriftDetected: at least one set field differs from the actual report.
	DriftDetected DriftState = "detected"
	// DriftNoSpec: no spec row exists; the node runs on cubelet defaults.
	DriftNoSpec DriftState = "no_spec"
)

// QuotaView is the response of the node quota API: desired vs actual vs drift.
type QuotaView struct {
	NodeID string      `json:"node_id"`
	Spec   *QuotaSpec  `json:"spec,omitempty"`
	Actual QuotaActual `json:"actual"`
	Drift  DriftState  `json:"drift"`
	// PausedRatioInherited marks an inherited (nil) spec ratio.
	PausedRatioInherited bool    `json:"paused_ratio_inherited,omitempty"`
	EffectivePausedRatio float64 `json:"effective_paused_ratio"`
	Message              string  `json:"message,omitempty"`
}

// PushResult reports the outcome of forwarding the spec to ops-agent.
type PushResult struct {
	// Applied is true when ops-agent confirmed the file write.
	Applied bool `json:"applied"`
	// SkipReason explains a non-push (disabled, unreachable, rejected); empty on success.
	SkipReason string `json:"skip_reason,omitempty"`
}

// ClusterQuotaDefaults is the cluster-wide quota policy configuration.
type ClusterQuotaDefaults struct {
	// PausedReleaseRatio: nil = unset.
	PausedReleaseRatio *float64 `json:"paused_release_ratio"`
}

// QuotaPropagation reports the fan-out after a cluster default change.
type QuotaPropagation struct {
	Bumped int64 `json:"bumped"`
	Pushed int   `json:"pushed"`
	Failed int   `json:"push_failed"`
}

// QuotaHistoryEntry is one audit row of the quota spec lifecycle.
type QuotaHistoryEntry struct {
	Operator  string    `json:"operator"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

// PhysicalTotals carries the node's physical totals for guard checks.
type PhysicalTotals struct {
	CpuTotal   int64 `json:"cpu_total"`
	MemMBTotal int64 `json:"mem_mb_total"`
}

// OpsAgentPushRequest is the push payload.
type OpsAgentPushRequest struct {
	RequestID string         `json:"request_id"`
	Revision  int64          `json:"revision"`
	Spec      *QuotaSpec     `json:"spec"`
	Physical  PhysicalTotals `json:"physical"`
	// OnlyPausedRatio restricts the push to the ratio field (cluster-managed rows).
	OnlyPausedRatio bool `json:"only_paused_ratio,omitempty"`
}

// OpsAgentPushResponse is the synchronous agent answer.
type OpsAgentPushResponse struct {
	RequestID string `json:"request_id"`
	Applied   bool   `json:"applied"`
	Noop      bool   `json:"noop,omitempty"` // file already matches the spec
	Error     string `json:"error,omitempty"`
}

// OpsAgentSpecResponse serves the pull endpoint.
type OpsAgentSpecResponse struct {
	NodeID   string         `json:"node_id"`
	Managed  bool           `json:"managed"`
	Revision int64          `json:"revision,omitempty"`
	Spec     *QuotaSpec     `json:"spec,omitempty"`
	Physical PhysicalTotals `json:"physical"`
	// OnlyPausedRatio restricts the reconcile to the ratio field.
	OnlyPausedRatio bool `json:"only_paused_ratio,omitempty"`
}
