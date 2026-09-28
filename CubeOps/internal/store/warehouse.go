// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	ImportPending   = "pending"
	ImportRunning   = "running"
	ImportSucceeded = "succeeded"
	ImportFailed    = "failed"

	PreinstallPending   = "pending"
	PreinstallRunning   = "running"
	PreinstallSucceeded = "succeeded"
	PreinstallFailed    = "failed"
	PreinstallCancelled = "cancelled"
)

// WarehouseItem is one archived component version.
type WarehouseItem struct {
	Arch      string    `json:"arch"`
	Component string    `json:"component"`
	Version   string    `json:"version"`
	Source    string    `json:"source"`
	SourceRef string    `json:"sourceRef"`
	ObjectKey string    `json:"objectKey"`
	SizeBytes int64     `json:"sizeBytes"`
	Checksum  string    `json:"checksum"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ImportJob is an asynchronous one-click import.
type ImportJob struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	SourceRef  string    `json:"sourceRef"`
	Tag        string    `json:"tag"`
	Arch       string    `json:"arch"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	BytesTotal int64     `json:"bytesTotal"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// PreinstallJob is a node-side install request.
type PreinstallJob struct {
	ID        string    `json:"id"`
	NodeID    string    `json:"nodeId"`
	Arch      string    `json:"arch"`
	Component string    `json:"component"`
	Version   string    `json:"version"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// NodeInstall is one locally inventoried version on a node.
type NodeInstall struct {
	NodeID    string    `json:"nodeId"`
	Arch      string    `json:"arch"`
	Component string    `json:"component"`
	Version   string    `json:"version"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (s *Store) InsertWarehouseItem(ctx context.Context, item WarehouseItem) (inserted bool, err error) {
	q := insertIgnorePrefix() +
		` INTO t_component_warehouse (arch, component, version, source, source_ref, object_key, size_bytes, checksum)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)` + onConflictDoNothing()
	res := s.db.WithContext(ctx).Exec(q,
		item.Arch, item.Component, item.Version, item.Source, item.SourceRef,
		item.ObjectKey, item.SizeBytes, item.Checksum,
	)
	if res.Error != nil {
		return false, fmt.Errorf("insert warehouse item: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

func (s *Store) GetWarehouseItem(ctx context.Context, arch, component, version string) (*WarehouseItem, error) {
	var item WarehouseItem
	err := s.db.WithContext(ctx).Raw(
		`SELECT arch, component, version, source, source_ref, object_key, size_bytes, checksum, created_at, updated_at
		 FROM t_component_warehouse WHERE arch = ? AND component = ? AND version = ? LIMIT 1`,
		arch, component, version,
	).Row().Scan(
		&item.Arch, &item.Component, &item.Version, &item.Source, &item.SourceRef,
		&item.ObjectKey, &item.SizeBytes, &item.Checksum, &item.CreatedAt, &item.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get warehouse item: %w", err)
	}
	return &item, nil
}

func (s *Store) ListWarehouseItems(ctx context.Context) ([]WarehouseItem, error) {
	rows, err := s.db.WithContext(ctx).Raw(
		`SELECT arch, component, version, source, source_ref, object_key, size_bytes, checksum, created_at, updated_at
		 FROM t_component_warehouse ORDER BY component, version, arch`,
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("list warehouse: %w", err)
	}
	defer rows.Close()
	return scanWarehouseItems(rows)
}

func (s *Store) ListWarehouseItemsByComponent(ctx context.Context, component string) ([]WarehouseItem, error) {
	rows, err := s.db.WithContext(ctx).Raw(
		`SELECT arch, component, version, source, source_ref, object_key, size_bytes, checksum, created_at, updated_at
		 FROM t_component_warehouse WHERE component = ? ORDER BY version, arch`,
		component,
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("list warehouse by component: %w", err)
	}
	defer rows.Close()
	return scanWarehouseItems(rows)
}

func scanWarehouseItems(rows *sql.Rows) ([]WarehouseItem, error) {
	var out []WarehouseItem
	for rows.Next() {
		var item WarehouseItem
		if err := rows.Scan(
			&item.Arch, &item.Component, &item.Version, &item.Source, &item.SourceRef,
			&item.ObjectKey, &item.SizeBytes, &item.Checksum, &item.CreatedAt, &item.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) DeleteWarehouseItem(ctx context.Context, arch, component, version string) error {
	res := s.db.WithContext(ctx).Exec(
		`DELETE FROM t_component_warehouse WHERE arch = ? AND component = ? AND version = ?`,
		arch, component, version,
	)
	if res.Error != nil {
		return fmt.Errorf("delete warehouse item: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) CreateImportJob(ctx context.Context, job ImportJob) error {
	res := s.db.WithContext(ctx).Exec(
		`INSERT INTO t_component_import_job (id, source, source_ref, tag, arch, status, error, bytes_total)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Source, job.SourceRef, job.Tag, job.Arch, job.Status, nullIfEmpty(job.Error), job.BytesTotal,
	)
	if res.Error != nil {
		return fmt.Errorf("create import job: %w", res.Error)
	}
	return nil
}

func (s *Store) GetImportJob(ctx context.Context, id string) (*ImportJob, error) {
	var job ImportJob
	var errMsg sql.NullString
	err := s.db.WithContext(ctx).Raw(
		`SELECT id, source, source_ref, tag, arch, status, error, bytes_total, created_at, updated_at
		 FROM t_component_import_job WHERE id = ? LIMIT 1`, id,
	).Row().Scan(
		&job.ID, &job.Source, &job.SourceRef, &job.Tag, &job.Arch, &job.Status,
		&errMsg, &job.BytesTotal, &job.CreatedAt, &job.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get import job: %w", err)
	}
	job.Error = errMsg.String
	return &job, nil
}

func staleAfterSeconds(d time.Duration) int64 {
	sec := int64(d / time.Second)
	if d > 0 && sec < 1 {
		sec = 1
	}
	return sec
}

// ListImportWork returns pending jobs plus running jobs whose updated_at is
// older than staleAfter, so another replica can reclaim a dead worker.
func (s *Store) ListImportWork(ctx context.Context, staleAfter time.Duration) ([]ImportJob, error) {
	rows, err := s.db.WithContext(ctx).Raw(
		`SELECT id, source, source_ref, tag, arch, status, error, bytes_total, created_at, updated_at
		 FROM t_component_import_job
		 WHERE status = ? OR (status = ? AND `+olderThanDurationSQL("updated_at")+`)
		 ORDER BY created_at ASC`,
		ImportPending, ImportRunning, staleAfterSeconds(staleAfter),
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("list import work: %w", err)
	}
	defer rows.Close()
	return scanImportJobs(rows)
}

// ClaimImportJob atomically takes a pending job, or a running job that has
// been stale longer than staleAfter. Returns true when this caller owns it.
func (s *Store) ClaimImportJob(ctx context.Context, id string, staleAfter time.Duration) (bool, error) {
	res := s.db.WithContext(ctx).Exec(
		`UPDATE t_component_import_job
		 SET status = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND (
		   status = ?
		   OR (status = ? AND `+olderThanDurationSQL("updated_at")+`)
		 )`,
		ImportRunning, id, ImportPending, ImportRunning, staleAfterSeconds(staleAfter),
	)
	if res.Error != nil {
		return false, fmt.Errorf("claim import job: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

func clampListLimit(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func (s *Store) ListImportJobs(ctx context.Context, limit, offset int) ([]ImportJob, int, error) {
	limit, offset = clampListLimit(limit, offset)
	var total int64
	if err := s.db.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM t_component_import_job`,
	).Scan(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count import jobs: %w", err)
	}
	rows, err := s.db.WithContext(ctx).Raw(
		`SELECT id, source, source_ref, tag, arch, status, error, bytes_total, created_at, updated_at
		 FROM t_component_import_job ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		limit, offset,
	).Rows()
	if err != nil {
		return nil, 0, fmt.Errorf("list import jobs: %w", err)
	}
	defer rows.Close()
	jobs, err := scanImportJobs(rows)
	if err != nil {
		return nil, 0, err
	}
	return jobs, int(total), nil
}

func (s *Store) UpdateImportJob(ctx context.Context, id, status, errMsg string, bytesTotal int64) error {
	res := s.db.WithContext(ctx).Exec(
		`UPDATE t_component_import_job SET status = ?, error = ?, bytes_total = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		status, nullIfEmpty(errMsg), bytesTotal, id,
	)
	if res.Error != nil {
		return fmt.Errorf("update import job: %w", res.Error)
	}
	return nil
}

// TouchImportJob refreshes updated_at for a still-running import. Returns
// false when this replica no longer owns the job (status is not running).
func (s *Store) TouchImportJob(ctx context.Context, id string) (bool, error) {
	res := s.db.WithContext(ctx).Exec(
		`UPDATE t_component_import_job SET updated_at = CURRENT_TIMESTAMP WHERE id = ? AND status = ?`,
		id, ImportRunning,
	)
	if res.Error != nil {
		return false, fmt.Errorf("touch import job: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// RequeueImportJob returns a running job to pending so another replica can
// pick it up immediately (used on graceful shutdown).
func (s *Store) RequeueImportJob(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).Exec(
		`UPDATE t_component_import_job SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND status = ?`,
		ImportPending, id, ImportRunning,
	)
	if res.Error != nil {
		return fmt.Errorf("requeue import job: %w", res.Error)
	}
	return nil
}

func (s *Store) CountLiveImportJobsBySourceRef(ctx context.Context, sourceRef string) (int, error) {
	var n int64
	if err := s.db.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM t_component_import_job
		 WHERE source_ref = ? AND status IN (?, ?)`,
		sourceRef, ImportPending, ImportRunning,
	).Scan(&n).Error; err != nil {
		return 0, fmt.Errorf("count live import jobs: %w", err)
	}
	return int(n), nil
}

func (s *Store) CreatePreinstallJobs(ctx context.Context, jobs []PreinstallJob) error {
	if len(jobs) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, job := range jobs {
			if err := tx.Exec(
				`INSERT INTO t_component_preinstall_job (id, node_id, arch, component, version, status, error)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				job.ID, job.NodeID, job.Arch, job.Component, job.Version, job.Status, nullIfEmpty(job.Error),
			).Error; err != nil {
				return fmt.Errorf("create preinstall job: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) ListPreinstallJobs(ctx context.Context, nodeID, status string, limit, offset int) ([]PreinstallJob, int, error) {
	limit, offset = clampListLimit(limit, offset)
	where := ` WHERE 1=1`
	args := []any{}
	if nodeID != "" {
		where += ` AND node_id = ?`
		args = append(args, nodeID)
	}
	if status != "" {
		where += ` AND status = ?`
		args = append(args, status)
	}
	var total int64
	if err := s.db.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM t_component_preinstall_job`+where, args...,
	).Scan(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count preinstall jobs: %w", err)
	}
	rows, err := s.db.WithContext(ctx).Raw(
		`SELECT id, node_id, arch, component, version, status, error, created_at, updated_at
		 FROM t_component_preinstall_job`+where+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...,
	).Rows()
	if err != nil {
		return nil, 0, fmt.Errorf("list preinstall jobs: %w", err)
	}
	defer rows.Close()
	jobs, err := scanPreinstallJobs(rows)
	if err != nil {
		return nil, 0, err
	}
	return jobs, int(total), nil
}

func (s *Store) ListNodePreinstallWork(ctx context.Context, nodeID string, staleAfter time.Duration) ([]PreinstallJob, error) {
	rows, err := s.db.WithContext(ctx).Raw(
		`SELECT id, node_id, arch, component, version, status, error, created_at, updated_at
		 FROM t_component_preinstall_job
		 WHERE node_id = ? AND (status = ? OR (status = ? AND `+olderThanDurationSQL("updated_at")+`))
		 ORDER BY created_at ASC`,
		nodeID, PreinstallPending, PreinstallRunning, staleAfterSeconds(staleAfter),
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("list node preinstall work: %w", err)
	}
	defer rows.Close()
	return scanPreinstallJobs(rows)
}

func (s *Store) AckPreinstallJob(ctx context.Context, id, nodeID, status, errMsg string) error {
	res := s.db.WithContext(ctx).Exec(
		`UPDATE t_component_preinstall_job SET status = ?, error = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND node_id = ?`,
		status, nullIfEmpty(errMsg), id, nodeID,
	)
	if res.Error != nil {
		return fmt.Errorf("ack preinstall job: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) CancelPendingPreinstallForVersion(ctx context.Context, arch, component, version string) error {
	res := s.db.WithContext(ctx).Exec(
		`UPDATE t_component_preinstall_job SET status = ?, error = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE arch = ? AND component = ? AND version = ? AND status IN (?, ?, ?)`,
		PreinstallCancelled, "warehouse version deleted", arch, component, version,
		PreinstallPending, PreinstallFailed, PreinstallRunning,
	)
	if res.Error != nil {
		return fmt.Errorf("cancel preinstall jobs: %w", res.Error)
	}
	return nil
}

// ReplaceNodeInstalls atomically replaces the inventory snapshot for
// (nodeID, arch). If the incoming set matches what is already stored, the
// transaction commits without DELETE/INSERT.
func (s *Store) ReplaceNodeInstalls(ctx context.Context, nodeID, arch string, items []NodeInstall) error {
	items = uniqueNodeInstalls(items)
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		rows, err := tx.Raw(
			`SELECT component, version FROM t_component_node_install
			 WHERE node_id = ? AND arch = ? FOR UPDATE`,
			nodeID, arch,
		).Rows()
		if err != nil {
			return fmt.Errorf("lock node installs: %w", err)
		}
		existing := map[string]struct{}{}
		for rows.Next() {
			var component, version string
			if err := rows.Scan(&component, &version); err != nil {
				_ = rows.Close()
				return err
			}
			existing[nodeInstallKey(component, version)] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if sameNodeInstallSet(existing, items) {
			return nil
		}
		if err := tx.Exec(
			`DELETE FROM t_component_node_install WHERE node_id = ? AND arch = ?`,
			nodeID, arch,
		).Error; err != nil {
			return fmt.Errorf("delete node installs: %w", err)
		}
		for _, it := range items {
			if err := tx.Exec(
				`INSERT INTO t_component_node_install (node_id, arch, component, version) VALUES (?, ?, ?, ?)`,
				nodeID, arch, it.Component, it.Version,
			).Error; err != nil {
				return fmt.Errorf("insert node install: %w", err)
			}
		}
		return nil
	})
	if txErr != nil {
		return fmt.Errorf("replace node installs: %w", txErr)
	}
	return nil
}

func (s *Store) ListNodeInstalls(ctx context.Context) ([]NodeInstall, error) {
	rows, err := s.db.WithContext(ctx).Raw(
		`SELECT node_id, arch, component, version, updated_at FROM t_component_node_install`,
	).Rows()
	if err != nil {
		return nil, fmt.Errorf("list node installs: %w", err)
	}
	defer rows.Close()
	return scanNodeInstalls(rows)
}

func scanNodeInstalls(rows *sql.Rows) ([]NodeInstall, error) {
	var out []NodeInstall
	for rows.Next() {
		var item NodeInstall
		if err := rows.Scan(&item.NodeID, &item.Arch, &item.Component, &item.Version, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func nodeInstallKey(component, version string) string {
	return component + "\x00" + version
}

func uniqueNodeInstalls(items []NodeInstall) []NodeInstall {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]NodeInstall, 0, len(items))
	for _, it := range items {
		key := nodeInstallKey(it.Component, it.Version)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, it)
	}
	return out
}

func sameNodeInstallSet(existing map[string]struct{}, items []NodeInstall) bool {
	if len(existing) != len(items) {
		return false
	}
	for _, it := range items {
		if _, ok := existing[nodeInstallKey(it.Component, it.Version)]; !ok {
			return false
		}
	}
	return true
}

func scanImportJobs(rows *sql.Rows) ([]ImportJob, error) {
	var out []ImportJob
	for rows.Next() {
		var job ImportJob
		var errMsg sql.NullString
		if err := rows.Scan(
			&job.ID, &job.Source, &job.SourceRef, &job.Tag, &job.Arch, &job.Status,
			&errMsg, &job.BytesTotal, &job.CreatedAt, &job.UpdatedAt,
		); err != nil {
			return nil, err
		}
		job.Error = errMsg.String
		out = append(out, job)
	}
	return out, rows.Err()
}

func scanPreinstallJobs(rows *sql.Rows) ([]PreinstallJob, error) {
	var out []PreinstallJob
	for rows.Next() {
		var job PreinstallJob
		var errMsg sql.NullString
		if err := rows.Scan(
			&job.ID, &job.NodeID, &job.Arch, &job.Component, &job.Version, &job.Status,
			&errMsg, &job.CreatedAt, &job.UpdatedAt,
		); err != nil {
			return nil, err
		}
		job.Error = errMsg.String
		out = append(out, job)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
