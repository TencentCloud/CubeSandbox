// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package sandboxstatus stores the control-plane restart status of a sandbox.
// Schema lives in pkgs/cubedb/migrate. A missing or stale row never blocks
// the node from restarting.
package sandboxstatus

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/pkgs/sandboxrestart"
	"gorm.io/gorm"
)

var (
	ErrNotFound = errors.New("sandbox status not found")
	ErrNotReady = errors.New("sandbox status store is not initialized")
)

var (
	dbMu sync.RWMutex
	db   *gorm.DB
)

// Init caches the shared gorm handle. The table is created by cubedb migrations.
func Init(client *gorm.DB) error {
	if client == nil {
		return errors.New("sandboxstatus.Init: nil db")
	}
	dbMu.Lock()
	db = client
	dbMu.Unlock()
	return nil
}

func getDB() *gorm.DB {
	dbMu.RLock()
	defer dbMu.RUnlock()
	return db
}

// DisplayPolicy rewrites the on-wire RESTART_POLICY_* value to the k8s name
// users send and read. Unknown text is left as-is.
func DisplayPolicy(raw string) string {
	return sandboxrestart.Display(raw)
}

// Upsert writes rec when its status_seq is strictly newer than the stored one.
// An older or equal seq is ignored. A soft-deleted row stays deleted so a
// late report after destroy cannot bring the sandbox back. The bool is true
// only when a row was created or updated.
func Upsert(ctx context.Context, rec *models.SandboxStatus) (bool, string, error) {
	client := getDB()
	if client == nil {
		return false, "", ErrNotReady
	}
	if rec == nil || strings.TrimSpace(rec.SandboxID) == "" {
		return false, "", errors.New("sandboxstatus.Upsert: sandbox_id is required")
	}
	rec.SandboxID = strings.TrimSpace(rec.SandboxID)
	if rec.ReportedAt.IsZero() {
		rec.ReportedAt = time.Now()
	}
	rec.RestartPolicy = DisplayPolicy(rec.RestartPolicy)

	var existing models.SandboxStatus
	err := client.WithContext(ctx).Unscoped().
		Where("sandbox_id = ?", rec.SandboxID).
		First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = client.WithContext(ctx).Create(rec).Error
		if err == nil {
			return true, "", nil
		}
		if !isDuplicateKey(err) {
			return false, "", err
		}
		err = client.WithContext(ctx).Unscoped().
			Where("sandbox_id = ?", rec.SandboxID).
			First(&existing).Error
	}
	if err != nil {
		return false, "", err
	}
	if existing.DeletedAt.Valid || existing.StatusSeq >= rec.StatusSeq {
		return false, existing.RestartState, nil
	}
	// The WHERE keeps a destroy that lands between the read and the write
	// from being overwritten. RowsAffected 0 means the row was deleted or
	// a newer seq won.
	tx := client.WithContext(ctx).Model(&models.SandboxStatus{}).
		Where("id = ? AND deleted_at IS NULL AND status_seq < ?", existing.ID, rec.StatusSeq).
		Updates(statusUpdates(rec))
	if tx.Error != nil {
		return false, existing.RestartState, tx.Error
	}
	return tx.RowsAffected > 0, existing.RestartState, nil
}

func statusUpdates(rec *models.SandboxStatus) map[string]any {
	return map[string]any{
		"host_id":                    rec.HostID,
		"phase":                      rec.Phase,
		"restart_policy":             rec.RestartPolicy,
		"restart_state":              rec.RestartState,
		"restart_count":              rec.RestartCount,
		"last_exit_code":             rec.LastExitCode,
		"last_exit_reason":           rec.LastExitReason,
		"last_finished_at":           rec.LastFinishedAt,
		"last_restart_at":            rec.LastRestartAt,
		"last_successful_restart_at": rec.LastSuccessfulRestartAt,
		"last_failed_restart_at":     rec.LastFailedRestartAt,
		"next_restart_at":            rec.NextRestartAt,
		"status_seq":                 rec.StatusSeq,
		"reported_at":                rec.ReportedAt,
	}
}

// Get returns the live status row. Soft-deleted rows are not found.
func Get(ctx context.Context, sandboxID string) (*models.SandboxStatus, error) {
	client := getDB()
	if client == nil {
		return nil, ErrNotReady
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, errors.New("sandboxstatus.Get: sandbox_id is required")
	}
	var rec models.SandboxStatus
	err := client.WithContext(ctx).
		Where("sandbox_id = ?", sandboxID).
		First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// BatchGet returns live rows for the given ids. Missing ids are omitted.
func BatchGet(ctx context.Context, sandboxIDs []string) (map[string]*models.SandboxStatus, error) {
	client := getDB()
	if client == nil {
		return nil, ErrNotReady
	}
	ids := make([]string, 0, len(sandboxIDs))
	for _, id := range sandboxIDs {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	out := map[string]*models.SandboxStatus{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []models.SandboxStatus
	if err := client.WithContext(ctx).Where("sandbox_id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for i := range rows {
		out[rows[i].SandboxID] = &rows[i]
	}
	return out, nil
}

// SoftDelete marks the row deleted. A missing row becomes a tombstone so a
// later report cannot create a live row for a sandbox that is already gone.
func SoftDelete(ctx context.Context, sandboxID string) error {
	client := getDB()
	if client == nil {
		return nil
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return errors.New("sandboxstatus.SoftDelete: sandbox_id is required")
	}
	res := client.WithContext(ctx).
		Where("sandbox_id = ?", sandboxID).
		Delete(&models.SandboxStatus{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	var existing models.SandboxStatus
	err := client.WithContext(ctx).Unscoped().
		Where("sandbox_id = ?", sandboxID).
		First(&existing).Error
	if err == nil || !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	now := time.Now()
	tomb := &models.SandboxStatus{
		SandboxID:     sandboxID,
		RestartPolicy: "Never",
		StatusSeq:     math.MaxInt64,
		ReportedAt:    now,
	}
	tomb.DeletedAt = gorm.DeletedAt{Time: now, Valid: true}
	err = client.WithContext(ctx).Create(tomb).Error
	if isDuplicateKey(err) {
		return nil
	}
	return err
}

func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "1062") ||
		strings.Contains(msg, "duplicate entry") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "duplicate key")
}

// TableName is the physical table, exported for purge registration checks.
func TableName() string {
	return constants.SandboxStatusTableName
}
