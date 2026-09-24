// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	fwk "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/framework"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

// nodeTemplateState tracks, per node, which template replicas we believe are
// present, plus the bookkeeping needed to tolerate stale or partial heartbeats.
//
//   - templates:           the effective replica set attributed to the node.
//   - pendingOmission:     templates missing from the most recent fresh
//     heartbeat. A template must be omitted by TWO distinct newer heartbeats
//     (recorded here on the first, deregistered on the second) before it is
//     removed, so a single stale/partial poll cannot erase it.
//   - registrationBarrier: templates registered out-of-band (e.g. a snapshot
//     just cloned onto the node via RegisterTemplateReplica). They survive one
//     extra heartbeat omission so a poll captured before the registration
//     cannot immediately erase a just-registered replica.
//   - lastHeartbeat:       newest heartbeat timestamp applied. Older heartbeats
//     are additive-only; an equal one is treated as a duplicate replay.
//   - staleObservation:    whether stale input has put reconciliation into
//     additive-only mode; this rate-limits warnings and resets pending removal.
type nodeTemplateState struct {
	templates           map[string]struct{}
	pendingOmission     map[string]struct{}
	registrationBarrier map[string]struct{}
	lastHeartbeat       time.Time
	staleObservation    bool
}

func SyncNodeTemplates(ctx context.Context, nodeID string, templateIDs []string, heartbeatAt time.Time) {
	if nodeID == "" {
		return
	}

	current := normalizeTemplateIDSet(templateIDs)
	l.lockTemplateLocality.Lock()
	defer l.lockTemplateLocality.Unlock()

	state, ok := getCachedNodeTemplateStateLocked(nodeID)
	if !ok {
		state = newNodeTemplateState(nodeID)
	}
	staleHeartbeat := !heartbeatAt.IsZero() && !state.lastHeartbeat.IsZero() && heartbeatAt.Before(state.lastHeartbeat)
	if staleHeartbeat && !state.staleObservation {
		log.G(ctx).Warnf("SyncNodeTemplates nodeID=%s entered additive-only mode for stale observations heartbeat=%s last_heartbeat=%s",
			nodeID, heartbeatAt.Format(time.RFC3339Nano), state.lastHeartbeat.Format(time.RFC3339Nano))
		state.pendingOmission = make(map[string]struct{})
		state.staleObservation = true
	} else if !staleHeartbeat && state.staleObservation {
		log.G(ctx).Infof("SyncNodeTemplates nodeID=%s resumed ordered reconciliation heartbeat=%s last_heartbeat=%s",
			nodeID, heartbeatAt.Format(time.RFC3339Nano), state.lastHeartbeat.Format(time.RFC3339Nano))
		state.staleObservation = false
	}
	duplicateHeartbeat := !heartbeatAt.IsZero() && heartbeatAt.Equal(state.lastHeartbeat)

	registered := make([]string, 0)
	for templateID := range current {
		delete(state.pendingOmission, templateID)
		delete(state.registrationBarrier, templateID)
		if _, exists := state.templates[templateID]; exists && GetImageStateByNode(templateID, nodeID) != nil {
			continue
		}
		registerTemplateReplicaLocked(templateID, nodeID, 1, false)
		state.templates[templateID] = struct{}{}
		registered = append(registered, templateID)
	}

	// A single heartbeat that omits a template does NOT deregister it: a stale
	// or partial poll can legitimately lack a freshly-registered replica.
	// Removal is confirmed only across successive newer heartbeats:
	//   1. a registrationBarrier entry gets one free pass (barrier cleared, kept);
	//   2. otherwise the first omission is recorded in pendingOmission (kept);
	//   3. a subsequent newer heartbeat that still omits it deregisters it.
	// Duplicate (equal timestamp) and stale (older) heartbeats never deregister,
	// so a DB reload with no timestamp can only add replicas, never remove them.
	deregistered := make([]string, 0)
	retained := make([]string, 0)
	if !heartbeatAt.IsZero() && !duplicateHeartbeat && !staleHeartbeat {
		for templateID := range state.templates {
			if _, exists := current[templateID]; exists {
				continue
			}
			if _, direct := state.registrationBarrier[templateID]; direct {
				delete(state.registrationBarrier, templateID)
				retained = append(retained, templateID)
				continue
			}
			if _, pending := state.pendingOmission[templateID]; !pending {
				state.pendingOmission[templateID] = struct{}{}
				retained = append(retained, templateID)
				continue
			}
			deregisterTemplateReplicaLocked(templateID, nodeID)
			delete(state.templates, templateID)
			delete(state.pendingOmission, templateID)
			delete(state.registrationBarrier, templateID)
			deregistered = append(deregistered, templateID)
		}
		state.lastHeartbeat = heartbeatAt
	}
	setCachedNodeTemplateStateLocked(nodeID, state)

	if len(deregistered) == 0 && len(registered) == 0 && len(retained) == 0 {
		log.G(ctx).Debugf("SyncNodeTemplates nodeID=%s unchanged current=%d previousCached=%v", nodeID, len(current), ok)
		return
	}
	log.G(ctx).Infof("SyncNodeTemplates nodeID=%s registered=%d deregistered=%d retained_pending=%d current=%d effective=%d previousCached=%v registered_templates=%s deregistered_templates=%s retained_templates=%s",
		nodeID, len(registered), len(deregistered), len(retained), len(current), len(state.templates), ok,
		summarizeTemplateIDChanges(registered), summarizeTemplateIDChanges(deregistered), summarizeTemplateIDChanges(retained))
}

func forceRemoveNodeTemplates(ctx context.Context, nodeID string) {
	if nodeID == "" {
		return
	}
	l.lockTemplateLocality.Lock()
	defer l.lockTemplateLocality.Unlock()

	state, ok := getCachedNodeTemplateStateLocked(nodeID)
	if !ok {
		state = newNodeTemplateState(nodeID)
	}
	for templateID := range state.templates {
		deregisterTemplateReplicaLocked(templateID, nodeID)
	}
	if l.templateNodeCache != nil {
		l.templateNodeCache.Delete(nodeID)
	}
	log.G(ctx).Debugf("forceRemoveNodeTemplates nodeID=%s removed=%d", nodeID, len(state.templates))
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

// newNodeTemplateState seeds a fresh state for a node, recovering any templates
// already attributed to it from the image cache.
func newNodeTemplateState(nodeID string) *nodeTemplateState {
	return &nodeTemplateState{
		templates:           discoverNodeTemplateSet(nodeID),
		pendingOmission:     make(map[string]struct{}),
		registrationBarrier: make(map[string]struct{}),
	}
}

func getCachedNodeTemplateStateLocked(nodeID string) (*nodeTemplateState, bool) {
	if nodeID == "" || l.templateNodeCache == nil {
		return nil, false
	}
	value, ok := l.templateNodeCache.Get(nodeID)
	if !ok {
		return nil, false
	}
	state, ok := value.(*nodeTemplateState)
	if !ok || state == nil {
		return nil, false
	}
	return cloneNodeTemplateState(state), true
}

func setCachedNodeTemplateStateLocked(nodeID string, state *nodeTemplateState) {
	if nodeID == "" || state == nil || l.templateNodeCache == nil {
		return
	}
	l.templateNodeCache.SetDefault(nodeID, cloneNodeTemplateState(state))
}

func getCachedNodeTemplateSet(nodeID string) (map[string]struct{}, bool) {
	l.lockTemplateLocality.Lock()
	defer l.lockTemplateLocality.Unlock()
	state, ok := getCachedNodeTemplateStateLocked(nodeID)
	if !ok {
		return nil, false
	}
	return cloneTemplateIDSet(state.templates), true
}

func recordNodeTemplateMembershipLocked(nodeID, templateID string) {
	if nodeID == "" || templateID == "" || l.templateNodeCache == nil {
		return
	}
	state, _ := getCachedNodeTemplateStateLocked(nodeID)
	if state == nil {
		state = newNodeTemplateState(nodeID)
	}
	_, known := state.templates[templateID]
	state.templates[templateID] = struct{}{}
	if !known {
		delete(state.pendingOmission, templateID)
		state.registrationBarrier[templateID] = struct{}{}
	}
	setCachedNodeTemplateStateLocked(nodeID, state)
}

func removeNodeTemplateMembershipLocked(nodeID, templateID string) {
	if nodeID == "" || templateID == "" || l.templateNodeCache == nil {
		return
	}
	state, ok := getCachedNodeTemplateStateLocked(nodeID)
	if !ok {
		return
	}
	delete(state.templates, templateID)
	delete(state.pendingOmission, templateID)
	delete(state.registrationBarrier, templateID)
	setCachedNodeTemplateStateLocked(nodeID, state)
}

func removeTemplateMembershipFromAllNodesLocked(templateID string) {
	if templateID == "" || l.templateNodeCache == nil {
		return
	}
	// Mutate the cached state in place under lockTemplateLocality and skip nodes
	// that never referenced the template, so invalidating one image does not
	// clone and rewrite every node's state.
	for _, item := range l.templateNodeCache.Items() {
		state, ok := item.Object.(*nodeTemplateState)
		if !ok || state == nil {
			continue
		}
		_, inTemplates := state.templates[templateID]
		_, inPending := state.pendingOmission[templateID]
		_, inBarrier := state.registrationBarrier[templateID]
		if !inTemplates && !inPending && !inBarrier {
			continue
		}
		delete(state.templates, templateID)
		delete(state.pendingOmission, templateID)
		delete(state.registrationBarrier, templateID)
	}
}

func cloneNodeTemplateState(in *nodeTemplateState) *nodeTemplateState {
	if in == nil {
		return nil
	}
	out := &nodeTemplateState{
		templates:           cloneTemplateIDSet(in.templates),
		pendingOmission:     make(map[string]struct{}, len(in.pendingOmission)),
		registrationBarrier: make(map[string]struct{}, len(in.registrationBarrier)),
		lastHeartbeat:       in.lastHeartbeat,
		staleObservation:    in.staleObservation,
	}
	for templateID := range in.pendingOmission {
		out.pendingOmission[templateID] = struct{}{}
	}
	for templateID := range in.registrationBarrier {
		out.registrationBarrier[templateID] = struct{}{}
	}
	return out
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
