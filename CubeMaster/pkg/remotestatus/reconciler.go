// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package remotestatus polls Cubelet Snapshot.Status for S3 rows that are
// still uploading (remote_status=inprogress) and writes the terminal
// result. Multiple CubeMaster replicas may run this loop; updates use
// WHERE remote_status=inprogress so a replica that already finished the
// row is not overwritten.
package remotestatus

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	snapshotv1 "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/snapshot/v1"
	"gorm.io/gorm"
)

const reconcileInterval = 5 * time.Second

type workItem struct {
	kind       string
	table      string
	snapshotID string
	hostIP     string
	backend    string
}

const (
	kindSnapshot = "snapshot"
	kindPause    = "pause"
)

var (
	startOnce    sync.Once
	queryStatus  = cubelet.SnapshotStatus
	cubeletAddr  = cubelet.GetCubeletAddr
	nowTimestamp = func() any { return gorm.Expr("CURRENT_TIMESTAMP") }
)

// Start runs the inprogress poller until ctx is cancelled. Safe to call
// once per process; extra calls are ignored.
func Start(ctx context.Context, db *gorm.DB) {
	if db == nil {
		return
	}
	startOnce.Do(func() {
		go func() {
			for {
				ReconcileOnce(ctx, db)
				select {
				case <-ctx.Done():
					return
				case <-time.After(reconcileInterval):
				}
			}
		}()
	})
}

// ReconcileOnce lists inprogress S3 rows on both snapshot tables, queries
// Cubelet Status, and writes ready／failed only while the row is still
// inprogress.
func ReconcileOnce(ctx context.Context, db *gorm.DB) {
	if db == nil {
		return
	}
	items, err := listInProgress(ctx, db)
	if err != nil {
		log.G(ctx).Warnf("remote_status list inprogress failed: %v", err)
		return
	}
	for _, item := range items {
		if ctx.Err() != nil {
			return
		}
		reconcileItem(ctx, db, item)
	}
}

func listInProgress(ctx context.Context, db *gorm.DB) ([]workItem, error) {
	var items []workItem
	var snaps []models.SnapshotRecord
	if err := db.WithContext(ctx).Table(constants.SnapshotTableName).
		Where("backend = ? AND remote_status = ?", constants.SnapshotBackendS3, constants.RemoteStatusInProgress).
		Find(&snaps).Error; err != nil {
		return nil, err
	}
	for _, rec := range snaps {
		items = append(items, workItem{
			kind:       kindSnapshot,
			table:      constants.SnapshotTableName,
			snapshotID: rec.SnapshotID,
			hostIP:     rec.OriginNodeIP,
			backend:    rec.Backend,
		})
	}
	var pauses []models.PauseSnapshotRecord
	if err := db.WithContext(ctx).Table(constants.PauseSnapshotTableName).
		Where("backend = ? AND remote_status = ?", constants.SnapshotBackendS3, constants.RemoteStatusInProgress).
		Find(&pauses).Error; err != nil {
		return nil, err
	}
	for _, rec := range pauses {
		items = append(items, workItem{
			kind:       kindPause,
			table:      constants.PauseSnapshotTableName,
			snapshotID: rec.SnapshotID,
			hostIP:     rec.NodeIP,
			backend:    rec.Backend,
		})
	}
	return items, nil
}

func reconcileItem(ctx context.Context, db *gorm.DB, item workItem) {
	hostIP := strings.TrimSpace(item.hostIP)
	snapshotID := strings.TrimSpace(item.snapshotID)
	if hostIP == "" || snapshotID == "" {
		return
	}
	st, err := queryStatus(ctx, cubeletAddr(hostIP), &snapshotv1.StatusRequest{
		SnapshotId: snapshotID,
		Backend:    item.backend,
	})
	if err != nil || st == nil {
		log.G(ctx).Warnf("remote_status query %s %s on %s failed: %v", item.kind, snapshotID, hostIP, err)
		return
	}
	next, ok := terminalRemoteStatus(st.GetState(), st.GetRemoteReady())
	if !ok {
		return
	}
	res := db.WithContext(ctx).Table(item.table).
		Where("snapshot_id = ? AND remote_status = ?", snapshotID, constants.RemoteStatusInProgress).
		Updates(map[string]any{
			"remote_status": next,
			"updated_at":    nowTimestamp(),
		})
	if res.Error != nil {
		log.G(ctx).Warnf("remote_status update %s %s failed: %v", item.kind, snapshotID, res.Error)
		return
	}
	if res.RowsAffected == 0 {
		return
	}
	log.G(ctx).Infof("remote_status %s %s -> %s", item.kind, snapshotID, next)
}

func terminalRemoteStatus(state string, remoteReady bool) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case constants.RemoteStatusReady:
		return constants.RemoteStatusReady, true
	case constants.RemoteStatusFailed:
		return constants.RemoteStatusFailed, true
	case constants.RemoteStatusRunning, constants.RemoteStatusInProgress, constants.RemoteStatusPending:
		return "", false
	}
	if remoteReady {
		return constants.RemoteStatusReady, true
	}
	return "", false
}
