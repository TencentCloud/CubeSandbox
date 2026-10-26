// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package models

import "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"

// ArtifactNodePlacement records that a given ext4 rootfs artifact is (or was)
// physically distributed to a node. Unlike t_cube_template_replica it is not
// tied to a template's lifecycle: rows persist until the artifact itself is
// fully cleaned up, so last-owner-cleanup / GC can enumerate every node that
// ever received the artifact even after all referencing replicas are deleted.
type ArtifactNodePlacement struct {
	ArtifactID string `json:"artifact_id" gorm:"column:artifact_id;primaryKey"`
	NodeID     string `json:"node_id" gorm:"column:node_id;primaryKey"`
	NodeIP     string `json:"node_ip" gorm:"column:node_ip"`
	CreatedAt  int64  `json:"created_at" gorm:"column:created_at"`
}

func (ArtifactNodePlacement) TableName() string {
	return constants.ArtifactNodePlacementTableName
}
