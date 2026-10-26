// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

var ErrNotFound = errors.New("not found")

type NodeStore interface {
	UpsertRegistration(ctx context.Context, reg *NodeRegistration) error
	GetRegistration(ctx context.Context, nodeID string) (*NodeRegistration, error)
	ListRegistrations(ctx context.Context) ([]NodeRegistration, error)
	UpdateLabels(ctx context.Context, nodeID string, labels map[string]string) error
	UpdateHostFacts(ctx context.Context, nodeID string, factsJSON, cpuidHash, kernelRelease string) error
	// DeleteRegistration removes the registration row; ErrNotFound if absent.
	DeleteRegistration(ctx context.Context, nodeID string) error

	UpsertStatus(ctx context.Context, status *NodeStatus) error
	GetStatus(ctx context.Context, nodeID string) (*NodeStatus, error)
	ListStatuses(ctx context.Context) ([]NodeStatus, error)

	WriteComponentVersions(ctx context.Context, nodeID string, versions []model.ComponentVersion, inventoryIncomplete bool) error
	ListComponentVersions(ctx context.Context) ([]NodeComponentVersion, error)
	ListComponentVersionsByNode(ctx context.Context, nodeID string) ([]NodeComponentVersion, error)

	CreateOperation(ctx context.Context, op *NodeOperation) error
	ListOperations(ctx context.Context, nodeID string, limit int) ([]NodeOperation, error)

	// DeleteNode removes the registration/status/version rows in one
	// transaction, keeping operations rows as an audit trail.
	DeleteNode(ctx context.Context, nodeID string) error
}

type gormNodeStore struct {
	db *gorm.DB
}

func NewNodeStore(db *gorm.DB) NodeStore {
	return &gormNodeStore{db: db}
}

func (s *gormNodeStore) UpsertRegistration(ctx context.Context, reg *NodeRegistration) error {
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "node_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"host_ip", "grpc_port", "capacity_json", "allocatable_json",
			"instance_type", "cluster_label", "quota_cpu", "quota_mem_mb",
			"create_concurrent_num", "max_mvm_num", "host_facts_json",
			"cpuid_hash", "host_kernel_release", "updated_at",
		}),
	}).Create(reg).Error
}

func (s *gormNodeStore) GetRegistration(ctx context.Context, nodeID string) (*NodeRegistration, error) {
	var reg NodeRegistration
	if err := s.db.WithContext(ctx).Where("node_id = ?", nodeID).First(&reg).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &reg, nil
}

func (s *gormNodeStore) ListRegistrations(ctx context.Context) ([]NodeRegistration, error) {
	var regs []NodeRegistration
	if err := s.db.WithContext(ctx).Find(&regs).Error; err != nil {
		return nil, err
	}
	return regs, nil
}

func (s *gormNodeStore) UpdateLabels(ctx context.Context, nodeID string, labels map[string]string) error {
	res := s.db.WithContext(ctx).Table(model.NodeMetaRegistrationTable).
		Where("node_id = ?", nodeID).
		Updates(map[string]any{
			"labels_json": model.MustJSON(labels),
			"updated_at":  time.Now(),
		})
	return res.Error
}

func (s *gormNodeStore) UpdateHostFacts(ctx context.Context, nodeID string, factsJSON, cpuidHash, kernelRelease string) error {
	res := s.db.WithContext(ctx).Table(model.NodeMetaRegistrationTable).
		Where("node_id = ?", nodeID).
		Updates(map[string]any{
			"host_facts_json":     factsJSON,
			"cpuid_hash":          cpuidHash,
			"host_kernel_release": kernelRelease,
			"updated_at":          time.Now(),
		})
	return res.Error
}

func (s *gormNodeStore) UpsertStatus(ctx context.Context, status *NodeStatus) error {
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "node_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"conditions_json", "images_json", "local_templates_json",
			"heartbeat_unix", "healthy", "updated_at",
		}),
	}).Create(status).Error
}

func (s *gormNodeStore) GetStatus(ctx context.Context, nodeID string) (*NodeStatus, error) {
	var st NodeStatus
	if err := s.db.WithContext(ctx).Where("node_id = ?", nodeID).First(&st).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &st, nil
}

func (s *gormNodeStore) ListStatuses(ctx context.Context) ([]NodeStatus, error) {
	var sts []NodeStatus
	if err := s.db.WithContext(ctx).Find(&sts).Error; err != nil {
		return nil, err
	}
	return sts, nil
}

func (s *gormNodeStore) WriteComponentVersions(ctx context.Context, nodeID string, versions []model.ComponentVersion, inventoryIncomplete bool) error {
	now := time.Now().Unix()
	rows := make([]*NodeComponentVersion, 0, len(versions))
	keep := make([]string, 0, len(versions))
	for _, v := range versions {
		if v.Component == "" {
			continue
		}
		rows = append(rows, &NodeComponentVersion{
			NodeID:       nodeID,
			Component:    v.Component,
			Version:      v.Version,
			Commit:       v.Commit,
			BuildTime:    v.BuildTime,
			Source:       v.Source,
			ReportedUnix: now,
		})
		keep = append(keep, v.Component)
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(rows) > 0 {
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "node_id"}, {Name: "component"}},
				DoUpdates: clause.AssignmentColumns([]string{
					"version", "commit", "build_time", "source", "reported_unix", "updated_at",
				}),
			}).Create(&rows).Error; err != nil {
				return err
			}
		}
		if inventoryIncomplete {
			return nil
		}
		if len(keep) == 0 {
			return nil
		}
		return tx.Where("node_id = ? AND component NOT IN ?", nodeID, keep).
			Delete(&NodeComponentVersion{}).Error
	})
}

func (s *gormNodeStore) ListComponentVersions(ctx context.Context) ([]NodeComponentVersion, error) {
	var rows []NodeComponentVersion
	if err := s.db.WithContext(ctx).Model(&NodeComponentVersion{}).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *gormNodeStore) ListComponentVersionsByNode(ctx context.Context, nodeID string) ([]NodeComponentVersion, error) {
	var rows []NodeComponentVersion
	if err := s.db.WithContext(ctx).Where("node_id = ?", nodeID).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *gormNodeStore) CreateOperation(ctx context.Context, op *NodeOperation) error {
	return s.db.WithContext(ctx).Create(op).Error
}

func (s *gormNodeStore) ListOperations(ctx context.Context, nodeID string, limit int) ([]NodeOperation, error) {
	if limit <= 0 {
		limit = 50
	}
	var ops []NodeOperation
	if err := s.db.WithContext(ctx).
		Where("node_id = ?", nodeID).
		Order("created_at DESC, id DESC").
		Limit(limit).
		Find(&ops).Error; err != nil {
		return nil, err
	}
	return ops, nil
}

// DeleteRegistration removes only the registration row. Status and
// component-version rows for the node are left untouched; use
// DeleteNode for the transactional, multi-table delete.
func (s *gormNodeStore) DeleteRegistration(ctx context.Context, nodeID string) error {
	res := s.db.WithContext(ctx).
		Where("node_id = ?", nodeID).
		Delete(&NodeRegistration{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// HostMetaLoader reads the legacy host registry tables (read-only) to recover
// the scheduler fields the cubelet heartbeat does not carry.
type HostMetaLoader interface {
	// LoadHostMetas returns all host_info rows, keyed by InsID.
	LoadHostMetas(ctx context.Context) (map[string]*HostInfo, error)
	// LoadHostTypes returns the instance_type→cpu_type mapping.
	LoadHostTypes(ctx context.Context) (map[string]*HostTypeInfo, error)
	// LoadSubHostMetas returns all sub_host_info rows, keyed by InsID.
	LoadSubHostMetas(ctx context.Context) (map[string]*SubHostInfo, error)
}

type gormHostMetaLoader struct {
	db *gorm.DB
}

// NewHostMetaLoader returns a HostMetaLoader backed by the given *gorm.DB.
func NewHostMetaLoader(db *gorm.DB) HostMetaLoader {
	return &gormHostMetaLoader{db: db}
}

func (l *gormHostMetaLoader) LoadHostMetas(ctx context.Context) (map[string]*HostInfo, error) {
	rows := make([]HostInfo, 0)
	if err := l.db.WithContext(ctx).Table((&HostInfo{}).TableName()).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]*HostInfo, len(rows))
	for i := range rows {
		out[rows[i].InsID] = &rows[i]
	}
	return out, nil
}

func (l *gormHostMetaLoader) LoadHostTypes(ctx context.Context) (map[string]*HostTypeInfo, error) {
	rows := make([]HostTypeInfo, 0)
	if err := l.db.WithContext(ctx).Table((&HostTypeInfo{}).TableName()).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]*HostTypeInfo, len(rows))
	for i := range rows {
		out[rows[i].InstanceType] = &rows[i]
	}
	return out, nil
}

func (l *gormHostMetaLoader) LoadSubHostMetas(ctx context.Context) (map[string]*SubHostInfo, error) {
	rows := make([]SubHostInfo, 0)
	if err := l.db.WithContext(ctx).Table((&SubHostInfo{}).TableName()).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]*SubHostInfo, len(rows))
	for i := range rows {
		out[rows[i].InsID] = &rows[i]
	}
	return out, nil
}

// DeleteNode removes the node's registration, status, and component
// versions in a single transaction. Operations rows are preserved as
// an audit trail of past operator actions. A row-level UPDATE lock is
// taken on the registration to coordinate with concurrent registration
// upserts. Returns ErrNotFound when the registration row is absent.
func (s *gormNodeStore) DeleteNode(ctx context.Context, nodeID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var reg NodeRegistration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("node_id = ?", nodeID).Take(&reg).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		// Unscoped delete on status/registration because the rows use
		// soft-delete (gorm.Model.DeletedAt); we want the node fully gone
		// so a later registration with the same id starts fresh.
		if err := tx.Unscoped().Where("node_id = ?", nodeID).Delete(&NodeStatus{}).Error; err != nil {
			return err
		}
		if err := tx.Where("node_id = ?", nodeID).Delete(&NodeComponentVersion{}).Error; err != nil {
			return err
		}
		return tx.Unscoped().Where("node_id = ?", nodeID).Delete(&NodeRegistration{}).Error
	})
}

func ParseLabelsJSON(raw string) (map[string]string, error) {
	if raw == "" {
		return map[string]string{}, nil
	}
	m := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("parse labels_json: %w", err)
	}
	if m == nil {
		return map[string]string{}, nil
	}
	return m, nil
}
