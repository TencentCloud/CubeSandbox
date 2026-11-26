// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandboxspec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const mysqlRuntimeVolumeMountsMaxBytes = 65_535

var ErrRuntimeVolumeMountsTooLarge = errors.New("runtime volume mounts exceed the MySQL TEXT capacity")

// RuntimeVolumeMount identifies a confirmed runtime binding. VolumeRecordID
// identifies the original volume row even if VolumeID is later reused.
type RuntimeVolumeMount struct {
	VolumeID       string `json:"volume_id"`
	VolumeRecordID uint   `json:"volume_record_id"`
	ContainerPath  string `json:"container_path"`
	Readonly       bool   `json:"readonly"`
}

// SpecWithRuntimeVolumeMounts contains the base request and confirmed additions
// from one spec row. RowID identifies the row to update after Attach returns.
type SpecWithRuntimeVolumeMounts struct {
	SandboxID string
	RowID     uint
	Request   *sandboxtypes.CreateCubeSandboxReq
	Mounts    []RuntimeVolumeMount
}

// GetWithRuntimeVolumeMounts reads the active spec and its confirmed bindings.
func GetWithRuntimeVolumeMounts(ctx context.Context, sandboxID string) (*SpecWithRuntimeVolumeMounts, error) {
	rec, err := getSpecRow(ctx, sandboxID)
	return specWithRuntimeVolumeMounts(rec, err)
}

// GetRuntimeVolumeMountsMany reads confirmed mounts for active specs without
// decoding the base request. A failed batch returns the results already read.
func GetRuntimeVolumeMountsMany(ctx context.Context, sandboxIDs []string) (map[string][]RuntimeVolumeMount, error) {
	ids := uniqueTrimmedIDs(sandboxIDs)
	out := make(map[string][]RuntimeVolumeMount)
	if len(ids) == 0 {
		return out, nil
	}
	client := getDB()
	if client == nil {
		return nil, ErrSandboxSpecStoreNotReady
	}
	batchSize := max(1, getManyBatchSize)
	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		var records []models.SandboxSpec
		if err := client.WithContext(ctx).Select("sandbox_id", "runtime_volume_mounts").
			Where("sandbox_id IN ? AND runtime_volume_mounts IS NOT NULL", ids[start:end]).Find(&records).Error; err != nil {
			return out, err
		}
		for i := range records {
			mounts, err := decodeRuntimeVolumeMounts(&records[i])
			if err == nil && len(mounts) > 0 {
				out[records[i].SandboxID] = mounts
			}
		}
	}
	return out, nil
}

// GetWithRuntimeVolumeMountsInTx reads the active spec through a caller-owned transaction.
func GetWithRuntimeVolumeMountsInTx(tx *gorm.DB, sandboxID string) (*SpecWithRuntimeVolumeMounts, error) {
	rec, err := getSpecRowWithDB(tx, sandboxID)
	return specWithRuntimeVolumeMounts(rec, err)
}

func specWithRuntimeVolumeMounts(rec *models.SandboxSpec, err error) (*SpecWithRuntimeVolumeMounts, error) {
	if err != nil {
		return nil, err
	}
	request, err := decodeSpec(rec)
	if err != nil {
		return nil, err
	}
	mounts, err := decodeRuntimeVolumeMounts(rec)
	if err != nil {
		return nil, err
	}
	return &SpecWithRuntimeVolumeMounts{SandboxID: rec.SandboxID, RowID: rec.ID, Request: request, Mounts: mounts}, nil
}

// RecordRuntimeVolumeMount appends a confirmed binding to the active row read
// before Attach was sent. The caller owns ordering with sandbox lifecycle RPCs.
func RecordRuntimeVolumeMount(ctx context.Context, sandboxID string, rowID uint, mount RuntimeVolumeMount) error {
	client := getDB()
	if client == nil {
		return ErrSandboxSpecStoreNotReady
	}
	return client.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return RecordRuntimeVolumeMountInTx(tx, sandboxID, rowID, mount)
	})
}

// RecordRuntimeVolumeMountInTx appends the binding in the caller's transaction.
// The caller owns commit, rollback and ordering with sandbox lifecycle RPCs.
func RecordRuntimeVolumeMountInTx(tx *gorm.DB, sandboxID string, rowID uint, mount RuntimeVolumeMount) error {
	payload, err := runtimeVolumeMountAppendPayload(tx, sandboxID, rowID, mount)
	if err != nil || payload == nil {
		return err
	}
	return tx.Model(&models.SandboxSpec{}).Where("id = ?", rowID).
		UpdateColumn("runtime_volume_mounts", string(payload)).Error
}

// CheckRuntimeVolumeMountCapacityInTx checks whether MySQL can store the
// appended bindings while the reservation transaction still owns the spec row.
func CheckRuntimeVolumeMountCapacityInTx(tx *gorm.DB, sandboxID string, rowID uint, mount RuntimeVolumeMount) error {
	if tx.Dialector.Name() != "mysql" {
		return nil
	}
	_, err := runtimeVolumeMountAppendPayload(tx, sandboxID, rowID, mount)
	return err
}

func runtimeVolumeMountAppendPayload(tx *gorm.DB, sandboxID string, rowID uint, mount RuntimeVolumeMount) ([]byte, error) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" || rowID == 0 {
		return nil, errors.New("sandboxspec.RecordRuntimeVolumeMount: sandbox_id and spec row ID are required")
	}
	mount.VolumeID = strings.TrimSpace(mount.VolumeID)
	mount.ContainerPath = path.Clean(mount.ContainerPath)
	if err := mount.validate(); err != nil {
		return nil, err
	}

	var rec models.SandboxSpec
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id", "runtime_volume_mounts").
		Where("sandbox_id = ? AND id = ?", sandboxID, rowID).Take(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSandboxSpecNotFound
	}
	if err != nil {
		return nil, err
	}
	mounts, err := decodeRuntimeVolumeMounts(&rec)
	if err != nil {
		return nil, err
	}
	for _, existing := range mounts {
		if existing == mount {
			return nil, nil
		}
		if existing.VolumeID == mount.VolumeID || existing.ContainerPath == mount.ContainerPath {
			return nil, fmt.Errorf("runtime volume %q conflicts with an existing binding", mount.VolumeID)
		}
	}
	payload, err := json.Marshal(append(mounts, mount))
	if err != nil {
		return nil, err
	}
	if tx.Dialector.Name() == "mysql" && len(payload) > mysqlRuntimeVolumeMountsMaxBytes {
		return nil, ErrRuntimeVolumeMountsTooLarge
	}
	return payload, nil
}

func decodeRuntimeVolumeMounts(rec *models.SandboxSpec) ([]RuntimeVolumeMount, error) {
	if rec.RuntimeVolumeMounts == nil {
		return nil, nil
	}
	var records []struct {
		RuntimeVolumeMount
		Readonly *bool `json:"readonly"`
	}
	if err := json.Unmarshal([]byte(*rec.RuntimeVolumeMounts), &records); err != nil {
		return nil, fmt.Errorf("decode runtime volume mounts: %w", err)
	}
	if records == nil {
		return nil, errors.New("decode runtime volume mounts: expected an array")
	}
	mounts := make([]RuntimeVolumeMount, 0, len(records))
	for _, record := range records {
		if record.Readonly == nil {
			return nil, errors.New("decode runtime volume mounts: readonly is required")
		}
		mount := record.RuntimeVolumeMount
		mount.Readonly = *record.Readonly
		if err := mount.validate(); err != nil {
			return nil, err
		}
		mounts = append(mounts, mount)
	}
	return mounts, nil
}

func (mount RuntimeVolumeMount) validate() error {
	if mount.VolumeID == "" || mount.VolumeID != strings.TrimSpace(mount.VolumeID) || mount.VolumeRecordID == 0 {
		return errors.New("runtime volume mount requires a volume ID and volume record ID")
	}
	if !path.IsAbs(mount.ContainerPath) || mount.ContainerPath == "/" || mount.ContainerPath != path.Clean(mount.ContainerPath) {
		return errors.New("runtime volume mount container path must be a canonical, absolute non-root path")
	}
	return nil
}
