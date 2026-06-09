// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func jobModelToInfo(ctx context.Context, record *models.TemplateImageJob) (*types.TemplateImageJobInfo, error) {
	info := &types.TemplateImageJobInfo{
		JobID:                   record.JobID,
		TemplateID:              record.TemplateID,
		RequestID:               record.RequestID,
		SandboxID:               record.SandboxID,
		ResourceType:            record.ResourceType,
		ResourceID:              record.ResourceID,
		AttemptNo:               record.AttemptNo,
		RetryOfJobID:            record.RetryOfJobID,
		Operation:               record.Operation,
		RedoMode:                record.RedoMode,
		RedoScope:               unmarshalRedoScope(record.RedoScopeJSON),
		ResumePhase:             record.ResumePhase,
		ArtifactID:              record.ArtifactID,
		TemplateSpecFingerprint: record.TemplateSpecFingerprint,
		Status:                  record.Status,
		Phase:                   record.Phase,
		Progress:                record.Progress,
		ErrorMessage:            record.ErrorMessage,
		ExpectedNodeCount:       record.ExpectedNodeCount,
		ReadyNodeCount:          record.ReadyNodeCount,
		FailedNodeCount:         record.FailedNodeCount,
		TemplateStatus:          record.TemplateStatus,
		ArtifactStatus:          record.ArtifactStatus,
	}
	if record.ArtifactID != "" {
		if artifact, err := getRootfsArtifactByID(ctx, record.ArtifactID); err == nil {
			info.Artifact = artifactModelToInfo(artifact)
		}
	}
	return info, nil
}

func artifactModelToInfo(record *models.RootfsArtifact) *types.RootfsArtifactInfo {
	return &types.RootfsArtifactInfo{
		ArtifactID:              record.ArtifactID,
		TemplateSpecFingerprint: record.TemplateSpecFingerprint,
		SourceImageRef:          record.SourceImageRef,
		SourceImageDigest:       record.SourceImageDigest,
		MasterNodeID:            record.MasterNodeID,
		MasterNodeIP:            record.MasterNodeIP,
		Ext4Path:                record.Ext4Path,
		Ext4SHA256:              record.Ext4SHA256,
		Ext4SizeBytes:           record.Ext4SizeBytes,
		WritableLayerSize:       record.WritableLayerSize,
		Status:                  record.Status,
		LastError:               record.LastError,
	}
}
