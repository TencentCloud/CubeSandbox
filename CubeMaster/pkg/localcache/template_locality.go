// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"sort"
	"strconv"
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

	deregistered := make([]string, 0)
	registered := make([]string, 0)
	for templateID := range previous {
		if _, exists := current[templateID]; exists {
			continue
		}
		deregisterTemplateReplica(templateID, nodeID, false)
		deregistered = append(deregistered, templateID)
	}
	for templateID := range current {
		if _, exists := previous[templateID]; exists && GetImageStateByNode(templateID, nodeID) != nil {
			continue
		}
		registerTemplateReplica(templateID, nodeID, 1, false)
		registered = append(registered, templateID)
	}
	setCachedNodeTemplateSet(nodeID, current)

	if len(deregistered) == 0 && len(registered) == 0 {
		log.G(ctx).Debugf("SyncNodeTemplates nodeID=%s unchanged current=%d previousCached=%v", nodeID, len(current), ok)
		return
	}
	log.G(ctx).Infof("SyncNodeTemplates nodeID=%s registered=%d deregistered=%d current=%d previous=%d previousCached=%v registered_templates=%s deregistered_templates=%s",
		nodeID, len(registered), len(deregistered), len(current), len(previous), ok,
		summarizeTemplateIDChanges(registered), summarizeTemplateIDChanges(deregistered))
	if log.IsDebug() {
		log.G(ctx).Debugf("SyncNodeTemplates detail nodeID=%s current=%v previous=%v", nodeID, current, previous)
	}
}

func summarizeTemplateIDChanges(templateIDs []string) string {
	if len(templateIDs) == 0 {
		return "[]"
	}
	sorted := append([]string(nil), templateIDs...)
	sort.Strings(sorted)
	const maxItems = 8
	if len(sorted) <= maxItems {
		return "[" + strings.Join(sorted, " ") + "]"
	}
	return "[" + strings.Join(sorted[:maxItems], " ") + " ... +" + strconv.Itoa(len(sorted)-maxItems) + " more]"
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
