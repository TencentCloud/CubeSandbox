// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

func getSnapshotRecord(ctx context.Context, snapshotID string) (*models.SnapshotRecord, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return nil, ErrSnapshotNotFound
	}
	rec := &models.SnapshotRecord{}
	err := store.db.WithContext(ctx).Table(constants.SnapshotTableName).
		Where("snapshot_id = ?", snapshotID).First(rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSnapshotNotFound
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func listSnapshotRecords(ctx context.Context) ([]models.SnapshotRecord, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	var rows []models.SnapshotRecord
	err := store.db.WithContext(ctx).Table(constants.SnapshotTableName).
		Order("created_at desc, id desc").Find(&rows).Error
	return rows, err
}

func updateSnapshotFields(ctx context.Context, snapshotID string, values map[string]any) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	return updateSnapshotFieldsTx(store.db.WithContext(ctx), snapshotID, values)
}

func updateSnapshotFieldsTx(tx *gorm.DB, snapshotID string, values map[string]any) error {
	values["updated_at"] = gorm.Expr("CURRENT_TIMESTAMP")
	return tx.Table(constants.SnapshotTableName).
		Where("snapshot_id = ?", snapshotID).Updates(values).Error
}

// updateSnapshotFieldsIfStatusIn writes only when the row is still in one of
// statuses. Used by replica-presence so a concurrent tombstone / physical
// delete is not overwritten with FAILED.
func updateSnapshotFieldsIfStatusIn(ctx context.Context, snapshotID string, values map[string]any, statuses ...string) (bool, error) {
	if !isReady() {
		return false, ErrTemplateStoreNotInitialized
	}
	if len(statuses) == 0 {
		return false, nil
	}
	values["updated_at"] = gorm.Expr("CURRENT_TIMESTAMP")
	result := store.db.WithContext(ctx).Table(constants.SnapshotTableName).
		Where("snapshot_id = ? AND status IN ?", snapshotID, statuses).
		Updates(values)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

func createSnapshotTx(ctx context.Context, tx *gorm.DB, snapshotID string, storedReq *sandboxtypes.CreateCubeSandboxReq, instanceType, version string, rec *models.SnapshotRecord) error {
	payload, err := json.Marshal(storedReq)
	if err != nil {
		return err
	}
	if rec == nil {
		rec = &models.SnapshotRecord{}
	}
	rec.SnapshotID = snapshotID
	rec.InstanceType = instanceType
	rec.Version = version
	rec.RequestJSON = string(payload)
	if rec.Status == "" {
		rec.Status = StatusCreating
	}
	if rec.Backend == "" {
		rec.Backend = constants.SnapshotBackendXFS
	}
	if err := tx.Table(constants.SnapshotTableName).Create(rec).Error; err != nil {
		if isDuplicateKeyError(err) {
			return ErrDuplicateTemplate
		}
		return err
	}
	return nil
}

func snapshotInfoFromRecord(rec *models.SnapshotRecord, replicas []ReplicaStatus, createReq *sandboxtypes.CreateCubeSandboxReq) SnapshotInfo {
	if rec == nil {
		return SnapshotInfo{}
	}
	return SnapshotInfo{
		SnapshotID:                rec.SnapshotID,
		InstanceType:              rec.InstanceType,
		Version:                   rec.Version,
		Status:                    rec.Status,
		DisplayName:               rec.DisplayName,
		OriginSandboxID:           rec.OriginSandboxID,
		OriginNodeID:              rec.OriginNodeID,
		OriginNodeIP:              rec.OriginNodeIP,
		OriginHostFactsJSON:       rec.OriginHostFactsJSON,
		StorageBackend:            rec.Backend,
		Backend:                   rec.Backend,
		RemoteStatus:              rec.RemoteStatus,
		RootfsSizeBytesAtSnapshot: rec.RootfsSizeBytesAtSnapshot,
		LastError:                 rec.LastError,
		CreatedAt:                 formatUTCRFC3339(rec.CreatedAt),
		Replicas:                  replicas,
		CreateRequest:             createReq,
	}
}

func snapshotReplicasFromRecord(ctx context.Context, rec *models.SnapshotRecord) ([]ReplicaStatus, error) {
	if rec == nil {
		return nil, nil
	}
	modelsReplicas, err := ListReplicas(ctx, rec.SnapshotID)
	if err != nil {
		return nil, err
	}
	out := make([]ReplicaStatus, 0, len(modelsReplicas)+1)
	for _, item := range modelsReplicas {
		out = append(out, replicaModelToStatus(item))
	}
	if len(out) == 0 {
		if rec.OriginNodeID != "" || rec.OriginNodeIP != "" {
			out = append(out, ReplicaStatus{
				NodeID:       rec.OriginNodeID,
				NodeIP:       rec.OriginNodeIP,
				InstanceType: rec.InstanceType,
				Status:       ReplicaStatusReady,
				Phase:        ReplicaPhaseReady,
			})
		}
	}
	return out, nil
}

func requestFromSnapshotJSON(raw string) (*sandboxtypes.CreateCubeSandboxReq, error) {
	req := &sandboxtypes.CreateCubeSandboxReq{}
	if strings.TrimSpace(raw) == "" {
		return req, nil
	}
	if err := json.Unmarshal([]byte(raw), req); err != nil {
		return nil, err
	}
	if req.Annotations == nil {
		req.Annotations = make(map[string]string)
	}
	constants.NormalizeAppSnapshotAnnotations(req.Annotations)
	return req, nil
}

// RestoreSource is the subset of a snapshot row needed by restoreplace.Decide.
type RestoreSource struct {
	SnapshotID          string
	Backend             string
	RemoteStatus        string
	OriginNodeID        string
	OriginNodeIP        string
	OriginHostFactsJSON string
	InstanceType        string
	ExportUUIDs         string
}

// GetSnapshotRestoreSource loads backend, remote_status, origin node and
// frozen host facts for §3.1 placement. Does not consult the template table.
func GetSnapshotRestoreSource(ctx context.Context, snapshotID string) (*RestoreSource, error) {
	rec, err := getSnapshotRecord(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	if snapshotRejectsNewUse(rec.Status) {
		return nil, wrapSnapshotNotFound(snapshotID)
	}
	return &RestoreSource{
		SnapshotID:          rec.SnapshotID,
		Backend:             rec.Backend,
		RemoteStatus:        rec.RemoteStatus,
		OriginNodeID:        rec.OriginNodeID,
		OriginNodeIP:        rec.OriginNodeIP,
		OriginHostFactsJSON: rec.OriginHostFactsJSON,
		InstanceType:        rec.InstanceType,
		ExportUUIDs:         rec.ExportUUIDs,
	}, nil
}
