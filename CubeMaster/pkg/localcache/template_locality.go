// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/patrickmn/go-cache"
	"k8s.io/apimachinery/pkg/util/sets"

	fwk "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/framework"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

// templateLocality tracks which nodes hold which templates for scheduling.
// Two writers feed it and each can only retract what it asserted:
//   - registered: the template center, driven by replica lifecycle events;
//   - reported:   node heartbeats, the latest inventory of each node.
//
// A template is local on a node when either writer asserts it, so a lagging
// heartbeat cannot drop a replica the template center just registered.
// images is the scheduler-facing projection of that rule. Membership changes
// happen under mu, one (template, node) pair at a time, so lock-free readers
// never observe a still-local pair disappear.
//
// Lock order: mu may be held while taking lockSortedNodes (read) and the node
// cache. Never acquire mu while holding lockSortedNodes or lockMetaData.
type templateLocality struct {
	mu         sync.Mutex
	registered map[string]sets.Set[string] // templateID -> nodeIDs
	reported   map[string]sets.Set[string] // nodeID -> templateIDs
	images     *cache.Cache                // templateID -> *fwk.ImageStateSummary
}

func newTemplateLocality() *templateLocality {
	return &templateLocality{
		registered: make(map[string]sets.Set[string]),
		reported:   make(map[string]sets.Set[string]),
		images:     cache.New(0, 0),
	}
}

// reportedDiff is the heartbeat inventory change applied by one replace.
// keptByRegistration lists templates this heartbeat dropped that stay local
// because the template center still asserts them.
type reportedDiff struct {
	added              []string
	removed            []string
	keptByRegistration []string
}

func (tl *templateLocality) image(templateID string) *fwk.ImageStateSummary {
	v, ok := tl.images.Get(templateID)
	if !ok {
		return nil
	}
	return v.(*fwk.ImageStateSummary)
}

func (tl *templateLocality) register(templateID, nodeID string, sizeBytes int64) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	// The first assertion creates the summary. Its size is whatever the caller
	// passed, including 0. Later calls only replace a known size with a positive one.
	created := tl.image(templateID) == nil
	addIndexMember(tl.registered, templateID, nodeID)
	tl.project(templateID, nodeID)
	state := tl.image(templateID)
	if created || sizeBytes > 0 {
		state.Size = sizeBytes
	}
	state.UpdateAt = time.Now()
	tl.refreshScores([]string{templateID})
}

func (tl *templateLocality) deregister(templateID, nodeID string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	removeIndexMember(tl.registered, templateID, nodeID)
	tl.project(templateID, nodeID)
}

func (tl *templateLocality) invalidateTemplate(templateID string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	nodes := tl.registered[templateID]
	delete(tl.registered, templateID)
	for nodeID := range nodes {
		tl.project(templateID, nodeID)
	}
	// Heartbeat assertions can keep the image alive, with fewer nodes than the
	// score was computed for. Refresh it here; the steady heartbeat path does not.
	tl.refreshScores([]string{templateID})
}

func (tl *templateLocality) replaceReported(nodeID string, current sets.Set[string]) reportedDiff {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	previous := tl.reported[nodeID]
	// Own the stored set. Callers may reuse or mutate the one they passed in.
	setOrDeleteIndex(tl.reported, nodeID, current.Clone())

	var diff reportedDiff
	var scoreRefresh []string
	for _, templateID := range sets.List(current.Difference(previous)) {
		// Already local via registration: the projection did not gain a node,
		// so the spread score must not be rewritten on the hot heartbeat path.
		alreadyLocal := tl.registered[templateID].Has(nodeID)
		tl.project(templateID, nodeID)
		diff.added = append(diff.added, templateID)
		if !alreadyLocal {
			scoreRefresh = append(scoreRefresh, templateID)
		}
	}
	for _, templateID := range sets.List(previous.Difference(current)) {
		if tl.registered[templateID].Has(nodeID) {
			diff.keptByRegistration = append(diff.keptByRegistration, templateID)
			continue
		}
		tl.project(templateID, nodeID)
		diff.removed = append(diff.removed, templateID)
	}
	tl.refreshScores(scoreRefresh)
	return diff
}

func (tl *templateLocality) removeNode(nodeID string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	delete(tl.reported, nodeID)
	for templateID := range tl.registered {
		removeIndexMember(tl.registered, templateID, nodeID)
	}
	for templateID, item := range tl.images.Items() {
		state, ok := item.Object.(*fwk.ImageStateSummary)
		if ok && state.HasNode(nodeID) {
			tl.project(templateID, nodeID)
		}
	}
}

// project makes images agree with whether any writer asserts (templateID, nodeID).
// It is the only code that changes node membership in images.
func (tl *templateLocality) project(templateID, nodeID string) {
	state := tl.image(templateID)
	if tl.registered[templateID].Has(nodeID) || tl.reported[nodeID].Has(templateID) {
		if state == nil {
			tl.images.SetDefault(templateID, fwk.NewImageStateSummary(1, nodeOssClusterLabel(nodeID), nodeID))
			return
		}
		state.AddNode(nodeID)
		return
	}
	if state == nil {
		return
	}
	state.RemoveNode(nodeID)
	if state.GetNumNodes() == 0 {
		tl.images.Delete(templateID)
	}
}

func scaledImageScore(imageState *fwk.ImageStateSummary, totalNumNodes int) int64 {
	if imageState == nil || totalNumNodes == 0 {
		return 0
	}
	spread := float64(imageState.Snapshot().NumNodes) / float64(totalNumNodes)
	return int64(float64(imageState.Size) * spread)
}

func (tl *templateLocality) refreshScores(templateIDs []string) {
	healthyByLabel := make(map[string]int)
	for _, templateID := range templateIDs {
		state := tl.image(templateID)
		if state == nil || state.OssClusterLabel == "" {
			continue
		}
		healthy, ok := healthyByLabel[state.OssClusterLabel]
		if !ok {
			healthy = GetHealthyNodesByInstanceType(-1, state.OssClusterLabel).Len()
			healthyByLabel[state.OssClusterLabel] = healthy
		}
		state.ScaledImageScore = scaledImageScore(state, healthy)
	}
}

func nodeOssClusterLabel(nodeID string) string {
	n, ok := GetNode(nodeID)
	if !ok || n == nil {
		return ""
	}
	return n.OssClusterLabel
}

func addIndexMember(index map[string]sets.Set[string], key, member string) {
	if index[key] == nil {
		index[key] = sets.New[string]()
	}
	index[key].Insert(member)
}

func removeIndexMember(index map[string]sets.Set[string], key, member string) {
	members := index[key]
	if members == nil {
		return
	}
	members.Delete(member)
	if members.Len() == 0 {
		delete(index, key)
	}
}

func setOrDeleteIndex(index map[string]sets.Set[string], key string, members sets.Set[string]) {
	if members.Len() == 0 {
		delete(index, key)
		return
	}
	index[key] = members
}

// SyncNodeTemplates replaces the heartbeat-reported inventory of nodeID on the
// process-wide cache. It never removes replicas registered by the template
// center; those stay local until the template center retracts them or the node
// leaves the cluster view.
func SyncNodeTemplates(ctx context.Context, nodeID string, templateIDs []string) {
	l.syncNodeTemplates(ctx, nodeID, templateIDs)
}

func (l *local) syncNodeTemplates(ctx context.Context, nodeID string, templateIDs []string) {
	if nodeID == "" {
		return
	}
	diff := l.locality.replaceReported(nodeID, normalizeTemplateIDSet(templateIDs))
	if len(diff.added) == 0 && len(diff.removed) == 0 && len(diff.keptByRegistration) == 0 {
		return
	}
	log.G(ctx).Infof("SyncNodeTemplates nodeID=%s added=%s removed=%s kept_by_registration=%s",
		nodeID, summarizeTemplateIDChanges(diff.added), summarizeTemplateIDChanges(diff.removed),
		summarizeTemplateIDChanges(diff.keptByRegistration))
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

func normalizeTemplateIDSet(templateIDs []string) sets.Set[string] {
	out := sets.New[string]()
	for _, templateID := range templateIDs {
		templateID = strings.TrimSpace(templateID)
		if templateID == "" {
			continue
		}
		out.Insert(templateID)
	}
	return out
}
