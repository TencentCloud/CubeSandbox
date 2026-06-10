// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

var (
	ErrSnapshotNotFound          = errors.New("snapshot not found")
	ErrSnapshotOperationNotFound = errors.New("snapshot operation not found")
)

type SnapshotInfo struct {
	SnapshotID                string                             `json:"snapshot_id,omitempty"`
	InstanceType              string                             `json:"instance_type,omitempty"`
	Version                   string                             `json:"version,omitempty"`
	Status                    string                             `json:"status,omitempty"`
	DisplayName               string                             `json:"display_name,omitempty"`
	OriginSandboxID           string                             `json:"origin_sandbox_id,omitempty"`
	OriginNodeID              string                             `json:"origin_node_id,omitempty"`
	StorageBackend            string                             `json:"storage_backend,omitempty"`
	Retain                    bool                               `json:"retain,omitempty"`
	RootfsSizeBytesAtSnapshot uint64                             `json:"rootfs_size_bytes_at_snapshot,omitempty"`
	LastError                 string                             `json:"last_error,omitempty"`
	CreatedAt                 string                             `json:"created_at,omitempty"`
	RuntimeRefCount           int64                              `json:"runtime_ref_count,omitempty"`
	RuntimeRefSandboxes       []string                           `json:"runtime_ref_sandboxes,omitempty"`
	Replicas                  []ReplicaStatus                    `json:"replicas,omitempty"`
	CreateRequest             *sandboxtypes.CreateCubeSandboxReq `json:"create_request,omitempty"`
}

type SnapshotOperationInfo struct {
	OperationID  string `json:"operation_id,omitempty"`
	SnapshotID   string `json:"snapshot_id,omitempty"`
	SandboxID    string `json:"sandbox_id,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
	Operation    string `json:"operation,omitempty"`
	Status       string `json:"status,omitempty"`
	Phase        string `json:"phase,omitempty"`
	Progress     int32  `json:"progress,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	AttemptNo    int32  `json:"attempt_no,omitempty"`
	RetryOfJobID string `json:"retry_of_job_id,omitempty"`
	ResourceType string `json:"resource_type,omitempty"`
	ResourceID   string `json:"resource_id,omitempty"`
}

type ListSnapshotsOptions struct {
	SnapshotID string
	SandboxID  string
	Name       string
	Status     string
	Limit      int
	NextToken  string
}

func ListSnapshots(ctx context.Context, opts *ListSnapshotsOptions) ([]SnapshotInfo, string, error) {
	infos, err := ListTemplates(ctx)
	if err != nil {
		return nil, "", err
	}
	normalized := normalizeListSnapshotsOptions(opts)
	filtered := make([]SnapshotInfo, 0, len(infos))
	for _, info := range infos {
		if !strings.EqualFold(strings.TrimSpace(info.Kind), TemplateKindSnapshot) {
			continue
		}
		if !matchesSnapshotListOptions(&info, normalized) {
			continue
		}
		item := snapshotInfoFromTemplateInfo(&info, nil)
		if err := populateSnapshotRuntimeRefSummary(ctx, &item); err != nil {
			return nil, "", err
		}
		filtered = append(filtered, item)
	}
	start, err := decodeSnapshotNextToken(normalized.NextToken)
	if err != nil {
		return nil, "", err
	}
	if start >= len(filtered) {
		return []SnapshotInfo{}, "", nil
	}
	end := start + normalized.Limit
	if end > len(filtered) {
		end = len(filtered)
	}
	nextToken := ""
	if end < len(filtered) {
		nextToken = encodeSnapshotNextToken(end)
	}
	return filtered[start:end], nextToken, nil
}

func GetSnapshotInfo(ctx context.Context, snapshotID string, includeRequest bool) (*SnapshotInfo, error) {
	info, err := GetTemplateInfo(ctx, strings.TrimSpace(snapshotID))
	if err != nil {
		if errors.Is(err, ErrTemplateNotFound) {
			return nil, ErrSnapshotNotFound
		}
		return nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(info.Kind), TemplateKindSnapshot) {
		return nil, ErrSnapshotNotFound
	}
	var createReq *sandboxtypes.CreateCubeSandboxReq
	if includeRequest {
		createReq, err = GetTemplateRequest(ctx, snapshotID)
		if err != nil {
			if errors.Is(err, ErrTemplateNotFound) {
				return nil, ErrSnapshotNotFound
			}
			return nil, err
		}
	}
	result := snapshotInfoFromTemplateInfo(info, createReq)
	if err := populateSnapshotRuntimeRefSummary(ctx, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func GetSnapshotOperation(ctx context.Context, operationID string) (*SnapshotOperationInfo, error) {
	record, err := getTemplateImageJobRecordByID(ctx, strings.TrimSpace(operationID))
	if err != nil {
		return nil, ErrSnapshotOperationNotFound
	}
	if !isSnapshotOperationRecord(record) {
		return nil, ErrSnapshotOperationNotFound
	}
	info, err := jobModelToInfo(ctx, record)
	if err != nil {
		return nil, err
	}
	result := snapshotOperationFromJobInfo(info)
	return &result, nil
}

func GetTemplateKind(ctx context.Context, templateID string) (string, error) {
	id := strings.TrimSpace(templateID)
	if id == "" {
		return "", ErrTemplateNotFound
	}
	if kind, ok := getCachedTemplateKind(id); ok {
		return kind, nil
	}
	def, err := GetDefinition(ctx, id)
	if err != nil {
		return "", err
	}
	kind := strings.TrimSpace(def.Kind)
	setTemplateKindCache(id, kind)
	return kind, nil
}

func ResolveSnapshotReadyNodeScope(ctx context.Context, snapshotID string) ([]string, error) {
	info, err := GetSnapshotInfo(ctx, snapshotID, false)
	if err != nil {
		return nil, err
	}
	scope := make([]string, 0, len(info.Replicas))
	for _, replica := range info.Replicas {
		if !isReplicaSchedulable(replica) {
			continue
		}
		if replica.NodeID != "" {
			scope = append(scope, replica.NodeID)
		}
	}
	if len(scope) == 0 {
		return nil, ErrTemplateHasNoReadyReplica
	}
	return scope, nil
}

func ResolveSnapshotReadyReplica(ctx context.Context, snapshotID, preferredNodeID string) (ReplicaStatus, error) {
	return getSnapshotReadyReplica(ctx, strings.TrimSpace(snapshotID), strings.TrimSpace(preferredNodeID))
}

func snapshotInfoFromTemplateInfo(info *TemplateInfo, createReq *sandboxtypes.CreateCubeSandboxReq) SnapshotInfo {
	if info == nil {
		return SnapshotInfo{}
	}
	return SnapshotInfo{
		SnapshotID:                info.TemplateID,
		InstanceType:              info.InstanceType,
		Version:                   info.Version,
		Status:                    info.Status,
		DisplayName:               info.DisplayName,
		OriginSandboxID:           info.OriginSandboxID,
		OriginNodeID:              info.OriginNodeID,
		StorageBackend:            info.StorageBackend,
		Retain:                    info.Retain,
		RootfsSizeBytesAtSnapshot: info.RootfsSizeBytesAtSnapshot,
		LastError:                 info.LastError,
		CreatedAt:                 info.CreatedAt,
		Replicas:                  append([]ReplicaStatus(nil), info.Replicas...),
		CreateRequest:             createReq,
	}
}

func populateSnapshotRuntimeRefSummary(ctx context.Context, info *SnapshotInfo) error {
	if info == nil || strings.TrimSpace(info.SnapshotID) == "" {
		return nil
	}
	refs, sandboxes, err := listSnapshotRuntimeRefSummary(ctx, info.SnapshotID)
	if err != nil {
		return err
	}
	info.RuntimeRefCount = int64(len(refs))
	if len(sandboxes) == 0 {
		info.RuntimeRefSandboxes = nil
		return nil
	}
	info.RuntimeRefSandboxes = sandboxes
	return nil
}

func listSnapshotRuntimeRefSummary(ctx context.Context, snapshotID string) ([]SnapshotRuntimeRefInfo, []string, error) {
	refs, err := ListActiveSnapshotRuntimeRefs(ctx, snapshotID)
	if err != nil {
		return nil, nil, err
	}
	sandboxes := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.SandboxID == "" {
			continue
		}
		sandboxes = append(sandboxes, ref.SandboxID)
	}
	return refs, sandboxes, nil
}

func snapshotOperationFromJobInfo(info *sandboxtypes.TemplateImageJobInfo) SnapshotOperationInfo {
	if info == nil {
		return SnapshotOperationInfo{}
	}
	snapshotID := strings.TrimSpace(info.ResourceID)
	if snapshotID == "" {
		snapshotID = strings.TrimSpace(info.TemplateID)
	}
	return SnapshotOperationInfo{
		OperationID:  info.JobID,
		SnapshotID:   snapshotID,
		SandboxID:    info.SandboxID,
		RequestID:    info.RequestID,
		Operation:    normalizeSnapshotOperationName(info.Operation),
		Status:       normalizeSnapshotOperationStatus(info.Status),
		Phase:        strings.ToUpper(strings.TrimSpace(info.Phase)),
		Progress:     info.Progress,
		ErrorMessage: info.ErrorMessage,
		AttemptNo:    info.AttemptNo,
		RetryOfJobID: info.RetryOfJobID,
		ResourceType: info.ResourceType,
		ResourceID:   info.ResourceID,
	}
}

func isSnapshotOperationRecord(record *models.TemplateImageJob) bool {
	if record == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(record.ResourceType), JobResourceTypeSnapshot) {
		return true
	}
	switch strings.ToUpper(strings.TrimSpace(record.Operation)) {
	case JobOperationSnapshotCreate, JobOperationSnapshotRollback, JobOperationSnapshotDelete:
		return true
	default:
		return false
	}
}

func normalizeSnapshotOperationStatus(status string) string {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case JobStatusReady:
		return JobStatusReady
	case JobStatusFailed:
		return JobStatusFailed
	case JobStatusRunning:
		return JobStatusRunning
	default:
		return JobStatusPending
	}
}

func normalizeSnapshotOperationName(operation string) string {
	switch strings.ToUpper(strings.TrimSpace(operation)) {
	case JobOperationSnapshotCreate:
		return "CREATE"
	case JobOperationSnapshotRollback:
		return "ROLLBACK"
	case JobOperationSnapshotDelete:
		return "DELETE"
	default:
		return strings.ToUpper(strings.TrimSpace(operation))
	}
}

func normalizeListSnapshotsOptions(opts *ListSnapshotsOptions) ListSnapshotsOptions {
	normalized := ListSnapshotsOptions{
		Limit: 20,
	}
	if opts == nil {
		return normalized
	}
	normalized.SnapshotID = strings.TrimSpace(opts.SnapshotID)
	normalized.SandboxID = strings.TrimSpace(opts.SandboxID)
	normalized.Name = strings.TrimSpace(opts.Name)
	normalized.Status = strings.TrimSpace(opts.Status)
	normalized.NextToken = strings.TrimSpace(opts.NextToken)
	if opts.Limit > 0 {
		normalized.Limit = opts.Limit
	}
	if normalized.Limit > 100 {
		normalized.Limit = 100
	}
	return normalized
}

func matchesSnapshotListOptions(info *TemplateInfo, opts ListSnapshotsOptions) bool {
	if info == nil {
		return false
	}
	if opts.SnapshotID != "" && !strings.EqualFold(strings.TrimSpace(info.TemplateID), opts.SnapshotID) {
		return false
	}
	if opts.SandboxID != "" && !strings.EqualFold(strings.TrimSpace(info.OriginSandboxID), opts.SandboxID) {
		return false
	}
	if opts.Name != "" && !strings.EqualFold(strings.TrimSpace(info.DisplayName), opts.Name) {
		return false
	}
	if opts.Status != "" {
		if !strings.EqualFold(strings.TrimSpace(info.Status), opts.Status) {
			return false
		}
	} else if strings.EqualFold(strings.TrimSpace(info.Status), StatusDeleting) {
		return false
	}
	return true
}

func decodeSnapshotNextToken(token string) (int, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(token)
	if err != nil || value < 0 {
		return 0, errors.New("invalid next_token")
	}
	return value, nil
}

func encodeSnapshotNextToken(offset int) string {
	if offset <= 0 {
		return ""
	}
	return strconv.Itoa(offset)
}

func ensureSnapshotTemplateRequestAnnotation(req *sandboxtypes.CreateCubeSandboxReq, snapshotID string) {
	if req == nil {
		return
	}
	if req.Annotations == nil {
		req.Annotations = map[string]string{}
	}
	req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = snapshotID
}
