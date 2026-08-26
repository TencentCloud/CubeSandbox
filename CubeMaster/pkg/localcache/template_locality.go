// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"strings"

	fwk "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/framework"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

func SyncNodeTemplates(ctx context.Context, nodeID string, templateIDs []string) {
	if nodeID == "" {
		return
	}

	current := normalizeTemplateIDSet(templateIDs)
	previous, ok := getCachedNodeTemplateSet(nodeID)
	if !ok {
		previous = discoverNodeTemplateSet(nodeID)
	}

	log.G(ctx).Infof("SyncNodeTemplates nodeID=%s current=%v previous=%v previousCached=%v", nodeID, current, previous, ok)

	for templateID := range previous {
		if _, exists := current[templateID]; exists {
			continue
		}
		log.G(ctx).Infof("SyncNodeTemplates deregister nodeID=%s templateID=%s", nodeID, templateID)
		deregisterTemplateReplica(templateID, nodeID, false)
	}
	for templateID := range current {
		if _, exists := previous[templateID]; exists && GetImageStateByNode(templateID, nodeID) != nil {
			log.G(ctx).Infof("SyncNodeTemplates skip nodeID=%s templateID=%s (already registered)", nodeID, templateID)
			continue
		}
		log.G(ctx).Infof("SyncNodeTemplates register nodeID=%s templateID=%s", nodeID, templateID)
		registerTemplateReplica(templateID, nodeID, 1, false)
	}
	setCachedNodeTemplateSet(nodeID, current)
}

func normalizeTemplateIDSet(templateIDs []string) map[string]struct{} {
	out := make(map[string]struct{}, len(templateIDs))
	for _, templateID := range templateIDs {
		templateID = strings.TrimSpace(templateID)
		if templateID == "" {
			continue
		}
		out[templateID] = struct{}{}
	}
	return out
}

func discoverNodeTemplateSet(nodeID string) map[string]struct{} {
	out := make(map[string]struct{})
	if nodeID == "" || l.imageCache == nil {
		return out
	}
	for templateID, item := range l.imageCache.Items() {
		state, ok := item.Object.(*fwk.ImageStateSummary)
		if !ok || state == nil || !state.HasNode(nodeID) {
			continue
		}
		out[templateID] = struct{}{}
	}
	return out
}

func getCachedNodeTemplateSet(nodeID string) (map[string]struct{}, bool) {
	if nodeID == "" || l.templateNodeCache == nil {
		return nil, false
	}
	value, ok := l.templateNodeCache.Get(nodeID)
	if !ok {
		return nil, false
	}
	templates, ok := value.(map[string]struct{})
	if !ok {
		return nil, false
	}
	return cloneTemplateIDSet(templates), true
}

func setCachedNodeTemplateSet(nodeID string, templateSet map[string]struct{}) {
	if nodeID == "" || l.templateNodeCache == nil {
		return
	}
	l.templateNodeCache.SetDefault(nodeID, cloneTemplateIDSet(templateSet))
}

func recordNodeTemplateMembership(nodeID, templateID string) {
	if nodeID == "" || templateID == "" || l.templateNodeCache == nil {
		return
	}
	templates, _ := getCachedNodeTemplateSet(nodeID)
	if templates == nil {
		templates = make(map[string]struct{})
	}
	templates[templateID] = struct{}{}
	setCachedNodeTemplateSet(nodeID, templates)
}

func removeNodeTemplateMembership(nodeID, templateID string) {
	if nodeID == "" || templateID == "" || l.templateNodeCache == nil {
		return
	}
	templates, ok := getCachedNodeTemplateSet(nodeID)
	if !ok {
		return
	}
	delete(templates, templateID)
	setCachedNodeTemplateSet(nodeID, templates)
}

func removeTemplateMembershipFromAllNodes(templateID string) {
	if templateID == "" || l.templateNodeCache == nil {
		return
	}
	for nodeID, item := range l.templateNodeCache.Items() {
		templates, ok := item.Object.(map[string]struct{})
		if !ok {
			continue
		}
		cloned := cloneTemplateIDSet(templates)
		if _, exists := cloned[templateID]; !exists {
			continue
		}
		delete(cloned, templateID)
		setCachedNodeTemplateSet(nodeID, cloned)
	}
}

func cloneTemplateIDSet(in map[string]struct{}) map[string]struct{} {
	if len(in) == 0 {
		return map[string]struct{}{}
	}
	out := make(map[string]struct{}, len(in))
	for templateID := range in {
		out[templateID] = struct{}{}
	}
	return out
}
