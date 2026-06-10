// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
)

type TemplateCompatSummary struct {
	StaleTemplates  int `json:"stale_templates"`
	StaleReplicas   int `json:"stale_replicas"`
	AffectedNodes   int `json:"affected_nodes"`
	MissingReplicas int `json:"missing_replicas"`
	UnknownReplicas int `json:"unknown_replicas"`
}

const (
	compatComponentGuestImage = "guest-image"
	compatComponentAgent      = "cube-agent"
	compatComponentKernel     = "kernel"
)

type TemplateNodeCompat struct {
	NodeID                   string `json:"node_id"`
	NodeIP                   string `json:"node_ip,omitempty"`
	CompatStatus             string `json:"compat_status"`
	BoundGuestImageVersion   string `json:"bound_guest_image_version,omitempty"`
	CurrentGuestImageVersion string `json:"current_guest_image_version,omitempty"`
	BoundAgentVersion        string `json:"bound_agent_version,omitempty"`
	CurrentAgentVersion      string `json:"current_agent_version,omitempty"`
	BoundKernelVersion       string `json:"bound_kernel_version,omitempty"`
	CurrentKernelVersion     string `json:"current_kernel_version,omitempty"`
}

type TemplateCompatRow struct {
	TemplateID   string               `json:"template_id"`
	InstanceType string               `json:"instance_type,omitempty"`
	Overall      string               `json:"overall"`
	Nodes        []TemplateNodeCompat `json:"nodes"`
}

type TemplateCompatMatrix struct {
	Summary   TemplateCompatSummary `json:"summary"`
	Templates []TemplateCompatRow   `json:"templates"`
}

func configureCompatHooks() {
	nodemeta.OnGuestAgentVersionChanged = func(nodeID string) {
		ScheduleCompatScanForNode(nodeID)
	}
}

func scheduleInitialCompatScan(ctx context.Context) {
	nodes, err := nodemeta.ListNodes(ctx)
	if err != nil {
		log.G(ctx).Warnf("template compat initial scan: list nodes failed: %v", err)
		return
	}
	for _, node := range nodes {
		if node == nil || !node.Healthy {
			continue
		}
		ScheduleCompatScanForNode(node.NodeID)
	}
}

func ScheduleCompatScanForNode(nodeID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := ScanNodeCompat(ctx, nodeID); err != nil {
			log.G(ctx).Warnf("template compat scan failed node=%s err=%v", nodeID, err)
		}
	}()
}

func ScanNodeCompat(ctx context.Context, nodeID string) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	current, ok := nodemeta.GetNodeComponentVersions(ctx, nodeID)
	if !ok {
		return markNodeReadyReplicasCompat(ctx, nodeID, CompatStatusUnknown)
	}

	replicas := make([]models.TemplateReplica, 0)
	if err := store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("node_id = ? AND status = ?", nodeID, ReplicaStatusReady).
		Find(&replicas).Error; err != nil {
		return err
	}
	for _, row := range replicas {
		replica := replicaModelToStatus(row)
		next := evaluateCompat(replica, current[compatComponentGuestImage], current[compatComponentAgent], current[compatComponentKernel])
		if next == normalizeCompatStatus(replica.CompatStatus) {
			continue
		}
		if err := updateReplicaCompat(ctx, row.TemplateID, nodeID, next); err != nil {
			return err
		}
		if next == CompatStatusStale {
			evictReplicaFromSchedulingCaches(row.TemplateID, nodeID)
		}
	}
	return nil
}

func markNodeReadyReplicasCompat(ctx context.Context, nodeID, status string) error {
	replicas := make([]models.TemplateReplica, 0)
	if err := store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("node_id = ? AND status = ?", nodeID, ReplicaStatusReady).
		Find(&replicas).Error; err != nil {
		return err
	}
	for _, row := range replicas {
		current := normalizeCompatStatus(row.CompatStatus)
		if current == CompatStatusStale && status == CompatStatusUnknown {
			continue
		}
		if current == status {
			continue
		}
		if err := updateReplicaCompat(ctx, row.TemplateID, nodeID, status); err != nil {
			return err
		}
		if status == CompatStatusStale {
			evictReplicaFromSchedulingCaches(row.TemplateID, nodeID)
		}
	}
	return nil
}

func updateReplicaCompat(ctx context.Context, templateID, nodeID, status string) error {
	now := time.Now()
	return store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id = ? AND node_id = ? AND status = ?", templateID, nodeID, ReplicaStatusReady).
		Updates(map[string]any{
			"compat_status":       normalizeCompatStatus(status),
			"compat_checked_unix": now.Unix(),
			"updated_at":          now,
		}).Error
}

func updateReplicaCompatBaseline(ctx context.Context, templateID, nodeID string, current map[string]string) error {
	now := time.Now()
	return store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id = ? AND node_id = ? AND status = ? AND compat_status = ?", templateID, nodeID, ReplicaStatusReady, CompatStatusUnknown).
		Updates(map[string]any{
			"guest_image_version": current[compatComponentGuestImage],
			"agent_version":       current[compatComponentAgent],
			"kernel_version":      current[compatComponentKernel],
			"compat_status":       CompatStatusOK,
			"compat_checked_unix": now.Unix(),
			"updated_at":          now,
		}).Error
}

func evictReplicaFromSchedulingCaches(templateID, nodeID string) {
	localcache.DeregisterTemplateReplica(templateID, nodeID)
	evictReplicaFromLocalityCache(templateID, nodeID)
}

func GetCompatMatrix(ctx context.Context) (*TemplateCompatMatrix, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	defs := make([]models.TemplateDefinition, 0)
	if err := store.db.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
		Order("template_id asc").Find(&defs).Error; err != nil {
		return nil, err
	}
	replicas := make([]models.TemplateReplica, 0)
	if err := store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Find(&replicas).Error; err != nil {
		return nil, err
	}
	byTemplateNode := make(map[string]map[string]ReplicaStatus)
	for _, row := range replicas {
		if byTemplateNode[row.TemplateID] == nil {
			byTemplateNode[row.TemplateID] = make(map[string]ReplicaStatus)
		}
		byTemplateNode[row.TemplateID][row.NodeID] = replicaModelToStatus(row)
	}

	matrix := &TemplateCompatMatrix{Templates: make([]TemplateCompatRow, 0, len(defs))}
	affected := map[string]struct{}{}
	for _, def := range defs {
		nodes := localcache.GetHealthyNodesByInstanceType(-1, def.InstanceType)
		row := TemplateCompatRow{
			TemplateID:   def.TemplateID,
			InstanceType: def.InstanceType,
			Overall:      CompatStatusOK,
			Nodes:        make([]TemplateNodeCompat, 0, len(nodes)),
		}
		templateHasStale := false
		for i := range nodes {
			node := nodes[i]
			if node == nil {
				continue
			}
			current, ok := nodemeta.GetNodeComponentVersions(ctx, node.ID())
			if !ok {
				current = map[string]string{}
			}
			replica, hasReplica := byTemplateNode[def.TemplateID][node.ID()]
			cell := TemplateNodeCompat{
				NodeID:                   node.ID(),
				NodeIP:                   node.HostIP(),
				CompatStatus:             CompatStatusMissing,
				CurrentGuestImageVersion: current[compatComponentGuestImage],
				CurrentAgentVersion:      current[compatComponentAgent],
				CurrentKernelVersion:     current[compatComponentKernel],
			}
			if hasReplica && replica.Status == ReplicaStatusReady {
				cell.BoundGuestImageVersion = replica.GuestImageVersion
				cell.BoundAgentVersion = replica.AgentVersion
				cell.BoundKernelVersion = replica.KernelVersion
				cell.CompatStatus = normalizeCompatStatus(replica.CompatStatus)
				if ok {
					cell.CompatStatus = evaluateCompat(replica, current[compatComponentGuestImage], current[compatComponentAgent], current[compatComponentKernel])
				}
			}
			row.Nodes = append(row.Nodes, cell)
			switch cell.CompatStatus {
			case CompatStatusStale:
				templateHasStale = true
				matrix.Summary.StaleReplicas++
				affected[node.ID()] = struct{}{}
			case CompatStatusUnknown:
				matrix.Summary.UnknownReplicas++
				if row.Overall == CompatStatusOK {
					row.Overall = CompatStatusUnknown
				}
			case CompatStatusMissing:
				matrix.Summary.MissingReplicas++
				if row.Overall == CompatStatusOK {
					row.Overall = CompatStatusMissing
				}
			}
		}
		if templateHasStale {
			row.Overall = CompatStatusStale
			matrix.Summary.StaleTemplates++
		}
		sort.Slice(row.Nodes, func(i, j int) bool { return row.Nodes[i].NodeID < row.Nodes[j].NodeID })
		matrix.Templates = append(matrix.Templates, row)
	}
	matrix.Summary.AffectedNodes = len(affected)
	return matrix, nil
}

func RescanCompat(ctx context.Context, nodeIDs []string) error {
	if len(nodeIDs) == 0 {
		nodes, err := nodemeta.ListNodes(ctx)
		if err != nil {
			return err
		}
		for _, node := range nodes {
			if node == nil || !node.Healthy {
				continue
			}
			if err := ScanNodeCompat(ctx, node.NodeID); err != nil {
				return err
			}
		}
		return nil
	}
	for _, nodeID := range nodeIDs {
		if err := ScanNodeCompat(ctx, strings.TrimSpace(nodeID)); err != nil {
			return err
		}
	}
	return nil
}

func AdoptCompatBaseline(ctx context.Context, templateID string) (int, error) {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return 0, ErrTemplateNotFound
	}
	if _, err := GetDefinition(ctx, templateID); err != nil {
		return 0, err
	}
	replicas := make([]models.TemplateReplica, 0)
	if err := store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id = ? AND status = ? AND compat_status = ?", templateID, ReplicaStatusReady, CompatStatusUnknown).
		Find(&replicas).Error; err != nil {
		return 0, err
	}
	updated := 0
	for _, row := range replicas {
		current, ok := nodemeta.GetNodeComponentVersions(ctx, row.NodeID)
		if !ok {
			continue
		}
		guest := normalizeComponentVersion(current[compatComponentGuestImage])
		agent := normalizeComponentVersion(current[compatComponentAgent])
		kernel := normalizeComponentVersion(current[compatComponentKernel])
		if guest == "" || agent == "" {
			continue
		}
		if err := updateReplicaCompatBaseline(ctx, templateID, row.NodeID, map[string]string{
			compatComponentGuestImage: guest,
			compatComponentAgent:      agent,
			compatComponentKernel:     kernel,
		}); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}
