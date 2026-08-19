// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"strings"
)

// TemplateComponentVersionEntry is one replica's recorded component versions.
type TemplateComponentVersionEntry struct {
	NodeID            string `json:"node_id,omitempty"`
	NodeIP            string `json:"node_ip,omitempty"`
	Status            string `json:"status,omitempty"`
	GuestImageVersion string `json:"guest_image_version,omitempty"`
	AgentVersion      string `json:"agent_version,omitempty"`
	KernelVersion     string `json:"kernel_version,omitempty"`
	ShimVersion       string `json:"shim_version,omitempty"`
}

// TemplateComponentVersions is the per-replica component version list for a template.
type TemplateComponentVersions struct {
	TemplateID string                          `json:"template_id"`
	Replicas   []TemplateComponentVersionEntry `json:"replicas"`
}

// GetTemplateComponentVersions returns component versions recorded on template replicas.
func GetTemplateComponentVersions(ctx context.Context, templateID string) (*TemplateComponentVersions, error) {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return nil, ErrTemplateNotFound
	}
	if _, err := GetDefinition(ctx, templateID); err != nil {
		return nil, err
	}
	replicas, err := ListReplicas(ctx, templateID)
	if err != nil {
		return nil, err
	}
	out := &TemplateComponentVersions{
		TemplateID: templateID,
		Replicas:   make([]TemplateComponentVersionEntry, 0, len(replicas)),
	}
	for _, replica := range replicas {
		status := replicaModelToStatus(replica)
		out.Replicas = append(out.Replicas, TemplateComponentVersionEntry{
			NodeID:            status.NodeID,
			NodeIP:            status.NodeIP,
			Status:            status.Status,
			GuestImageVersion: status.GuestImageVersion,
			AgentVersion:      status.AgentVersion,
			KernelVersion:     status.KernelVersion,
			ShimVersion:       status.ShimVersion,
		})
	}
	return out, nil
}
