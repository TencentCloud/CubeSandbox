// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"errors"
	"time"
)

const (
	ArtifactStatusPending  = "PENDING"
	ArtifactStatusBuilding = "BUILDING"
	ArtifactStatusReady    = "READY"
	ArtifactStatusFailed   = "FAILED"

	JobStatusPending = "PENDING"
	JobStatusRunning = "RUNNING"
	JobStatusReady   = "READY"
	JobStatusFailed  = "FAILED"

	JobOperationCreate           = "CREATE"
	JobOperationRedo             = "REDO"
	JobOperationCommit           = "COMMIT"
	JobOperationLegacy           = "LEGACY"
	JobOperationSnapshotCreate   = "SNAPSHOT_CREATE"
	JobOperationSnapshotRollback = "SNAPSHOT_ROLLBACK"
	JobOperationSnapshotDelete   = "SNAPSHOT_DELETE"

	JobResourceTypeSnapshot = "snapshot"
	JobResourceTypeTemplate = "template"

	RedoModeAll         = "ALL"
	RedoModeNodes       = "NODES"
	RedoModeFailedOnly  = "FAILED_ONLY"
	RedoModeFailedNodes = "FAILED_NODES"

	JobPhasePulling            = "PULLING"
	JobPhaseUnpacking          = "UNPACKING"
	JobPhaseBuildingExt4       = "BUILDING_EXT4"
	JobPhaseGeneratingJSON     = "GENERATING_JSON"
	JobPhaseDistributing       = "DISTRIBUTING"
	JobPhaseCreatingTemplate   = "CREATING_TEMPLATE"
	JobPhaseSnapshotting       = "SNAPSHOTTING"
	JobPhaseRegistering        = "REGISTERING"
	JobPhaseRollbackPreparing  = "ROLLBACK_PREPARING"
	JobPhaseRollbackDriving    = "ROLLBACK_DRIVING"
	JobPhaseRollbackRecovering = "ROLLBACK_RECOVERING"
	JobPhaseDeleting           = "DELETING"
	JobPhaseReady              = "READY"

	defaultTemplateCPU         = "2000m"
	defaultTemplateMemory      = "2000Mi"
	defaultTemplateArtifactTTL = 7 * 24 * time.Hour
	rootfsWritableVolumeName   = "cube_rootfs_rw"
	defaultDistributionWorkers = 4
)

var ErrNoFailedTemplateReplicas = errors.New("no failed template replicas matched redo request")
