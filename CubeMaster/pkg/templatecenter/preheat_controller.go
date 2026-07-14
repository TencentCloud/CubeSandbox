// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/prometheus/client_golang/prometheus"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	preheatReconcileInterval = 5 * time.Minute
	preheatDebounceDelay     = 2 * time.Second
	preheatLockName          = "cubemaster_templatecenter_preheat_v1"
)

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

var preheatDecisionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "cubemaster_preheat_decisions_total",
	Help: "Total preheat decisions by outcome and reason.",
}, []string{"decision", "reason"})

// ---------------------------------------------------------------------------
// Trigger channel — callbacks write, controller loop reads
// ---------------------------------------------------------------------------

var preheatTriggerCh = make(chan struct{}, 1)

func signalPreheatReconcile() {
	select {
	case preheatTriggerCh <- struct{}{}:
	default: // already pending — coalesce
	}
}

// ---------------------------------------------------------------------------
// Cooldown tracking — single-goroutine access (reconcile loop only)
// ---------------------------------------------------------------------------

var lastRedoSubmitByTemplate = make(map[string]time.Time)

func cooldownElapsed(templateID string, cooldown time.Duration) bool {
	if cooldown <= 0 {
		return true
	}
	last, ok := lastRedoSubmitByTemplate[templateID]
	if !ok {
		return true
	}
	return time.Since(last) >= cooldown
}

// ---------------------------------------------------------------------------
// Singleton + startup
// ---------------------------------------------------------------------------

var preheatOnce sync.Once

func startPreheatController(ctx context.Context) {
	preheatOnce.Do(func() {
		go runPreheatController(ctx)
	})
}

func runPreheatController(ctx context.Context) {
	// Immediate first pass (mirrors snapshot_reconciler + artifact_gc).
	runPreheatReconcilePass(detachTemplateImageJobContext(ctx, "preheat", nil))

	ticker := time.NewTicker(preheatReconcileInterval)
	defer ticker.Stop()

	// Debounce timer — reused across iterations.
	debounceTimer := time.NewTimer(0)
	if !debounceTimer.Stop() {
		<-debounceTimer.C
	}
	var debounceCh <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-preheatTriggerCh:
			debounceTimer.Reset(preheatDebounceDelay)
			debounceCh = debounceTimer.C
		case <-debounceCh:
			debounceCh = nil
			runPreheatReconcilePass(detachTemplateImageJobContext(ctx, "preheat", nil))
		case <-ticker.C:
			runPreheatReconcilePass(detachTemplateImageJobContext(ctx, "preheat", nil))
		}
	}
}

// ---------------------------------------------------------------------------
// Config helper
// ---------------------------------------------------------------------------

func getPreheatConfig() *config.TemplatePreheatConf {
	cfg := config.GetConfig()
	if cfg == nil {
		return nil
	}
	return cfg.TemplatePreheat
}

// ---------------------------------------------------------------------------
// Reconcile pass
// ---------------------------------------------------------------------------

func runPreheatReconcilePass(ctx context.Context) {
	if !isReady() {
		return
	}

	preheatCfg := getPreheatConfig()
	if preheatCfg == nil || !preheatCfg.Enabled {
		return
	}
	if preheatCfg.DownloadBaseURL == "" {
		log.G(ctx).Warn("preheat: download_base_url is empty, skipping pass")
		preheatDecisionTotal.WithLabelValues("skipped", "missing_download_base_url").Inc()
		return
	}

	logger := log.G(ctx).WithFields(map[string]any{"component": "preheat"})

	// Acquire component-scoped MySQL GET_LOCK (mirrors artifact_gc).
	var lockRes sql.NullInt64
	if err := store.db.WithContext(ctx).
		Raw("SELECT GET_LOCK(?, 0)", preheatLockName).Scan(&lockRes).Error; err != nil {
		logger.Warnf("preheat: acquire lock failed: %v", err)
		return
	}
	if !lockRes.Valid || lockRes.Int64 != 1 {
		return // another master is reconciling
	}
	defer func() {
		if err := store.db.WithContext(ctx).Exec("SELECT RELEASE_LOCK(?)", preheatLockName).Error; err != nil {
			logger.Warnf("preheat: release lock failed: %v", err)
		}
	}()

	// 1. List healthy nodes (in-memory).
	nodes, err := nodemeta.ListNodes(ctx)
	if err != nil {
		logger.Warnf("preheat: list nodes failed: %v", err)
		return
	}
	healthyNodes := filterHealthyNodes(nodes)

	// 2. Sort pinned templates by priority (desc), tie-break on template_id.
	pinned := sortPinnedTemplates(preheatCfg.PinnedTemplates)
	if len(pinned) == 0 {
		return
	}

	// 3. Batch query: all data for all pinned templates.
	templateIDs := make([]string, 0, len(pinned))
	for _, p := range pinned {
		templateIDs = append(templateIDs, p.TemplateID)
	}
	defs := batchGetDefinitions(ctx, templateIDs)
	activeJobs := batchGetActiveJobs(ctx, templateIDs)
	replicas := batchListReplicas(ctx, templateIDs)

	// 4. Batch query: per-node budget usage for candidate nodes.
	candidateNodeIDs := collectCandidateNodeIDs(pinned, healthyNodes, defs)
	nodeCounts, nodeBytes := batchComputeNodeBudgetUsage(ctx, candidateNodeIDs)

	// 5. Evaluate each pinned template using in-memory data.
	for _, p := range pinned {
		evaluatePinnedTemplate(ctx, preheatCfg, p, healthyNodes,
			nodeCounts, nodeBytes, defs, activeJobs, replicas)
	}

	logger.Infof("preheat: pass complete, %d pinned templates evaluated", len(pinned))
}

// ---------------------------------------------------------------------------
// Batch queries
// ---------------------------------------------------------------------------

func batchGetDefinitions(ctx context.Context, templateIDs []string) map[string]*models.TemplateDefinition {
	var defs []models.TemplateDefinition
	store.db.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
		Where("template_id IN ?", templateIDs).Find(&defs)
	out := make(map[string]*models.TemplateDefinition, len(defs))
	for i := range defs {
		out[defs[i].TemplateID] = &defs[i]
	}
	return out
}

func batchGetActiveJobs(ctx context.Context, templateIDs []string) map[string]bool {
	var jobs []models.TemplateImageJob
	store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("template_id IN ? AND status IN ?", templateIDs,
			[]string{JobStatusPending, JobStatusRunning}).Find(&jobs)
	out := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		out[j.TemplateID] = true
	}
	return out
}

func batchListReplicas(ctx context.Context, templateIDs []string) map[string][]models.TemplateReplica {
	var replicas []models.TemplateReplica
	store.db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id IN ?", templateIDs).Find(&replicas)
	out := make(map[string][]models.TemplateReplica)
	for _, r := range replicas {
		out[r.TemplateID] = append(out[r.TemplateID], r)
	}
	return out
}

// batchComputeNodeBudgetUsage computes per-node READY replica counts and total
// bytes. The 3-table JOIN ensures rootfs_size_bytes_at_snapshot=0 (image-based
// templates) falls back to rootfs_artifact.ext4_size_bytes, so the byte budget
// is enforced correctly for all template kinds.
func batchComputeNodeBudgetUsage(ctx context.Context, nodeIDs []string) (map[string]int, map[string]int64) {
	counts := make(map[string]int)
	bytes := make(map[string]int64)
	if len(nodeIDs) == 0 {
		return counts, bytes
	}

	type nodeUsage struct {
		NodeID     string `gorm:"column:node_id"`
		Count      int64  `gorm:"column:cnt"`
		TotalBytes int64  `gorm:"column:total_bytes"`
	}
	var results []nodeUsage
	store.db.WithContext(ctx).
		Table(constants.TemplateReplicaTableName+" AS r").
		Select(`r.node_id,
			COUNT(*) AS cnt,
			COALESCE(SUM(
				COALESCE(NULLIF(d.rootfs_size_bytes_at_snapshot, 0),
					COALESCE(a.ext4_size_bytes, 0)
				)
			), 0) AS total_bytes`).
		Joins("LEFT JOIN " + constants.TemplateDefinitionTableName + " AS d ON r.template_id = d.template_id").
		Joins("LEFT JOIN " + constants.RootfsArtifactTableName + " AS a ON d.rootfs_artifact_id = a.artifact_id").
		Where("r.node_id IN ? AND r.status = ?", nodeIDs, ReplicaStatusReady).
		Group("r.node_id").
		Find(&results)

	for _, r := range results {
		counts[r.NodeID] = int(r.Count)
		bytes[r.NodeID] = r.TotalBytes
	}
	return counts, bytes
}

// ---------------------------------------------------------------------------
// Per-template evaluation
// ---------------------------------------------------------------------------

func evaluatePinnedTemplate(
	ctx context.Context,
	cfg *config.TemplatePreheatConf,
	pinned config.PinnedTemplateConf,
	healthyNodes []*nodemeta.NodeSnapshot,
	nodeCounts map[string]int,
	nodeBytes map[string]int64,
	defs map[string]*models.TemplateDefinition,
	activeJobs map[string]bool,
	replicas map[string][]models.TemplateReplica,
) {
	logger := log.G(ctx).WithFields(map[string]any{
		"component":   "preheat",
		"template_id": pinned.TemplateID,
	})

	// a. Get template definition (from batch).
	def, ok := defs[pinned.TemplateID]
	if !ok {
		logDecision(ctx, pinned.TemplateID, "skipped", "template_not_found")
		return
	}

	// b. Get template size for candidate budget check.
	templateSize := getTemplateSizeFromDef(ctx, def)

	// c. Get replicas (from batch).
	templateReplicas := replicas[pinned.TemplateID]

	// d. Find nodes matching node_selector AND template's instance type.
	matchingNodes := filterNodesForTemplate(healthyNodes, pinned.NodeSelector, def.InstanceType)

	// e. Count ready replicas on matching nodes (cluster-level semantics).
	matchingNodeSet := nodeSnapshotIDSet(matchingNodes)
	readyOnMatching := 0
	existingReady := make(map[string]struct{})
	for _, r := range templateReplicas {
		if r.Status != ReplicaStatusReady {
			continue
		}
		if _, ok := matchingNodeSet[r.NodeID]; ok {
			readyOnMatching++
			existingReady[r.NodeID] = struct{}{}
		}
	}

	// f. Check min_replicas floor.
	if readyOnMatching >= pinned.MinReplicas {
		logDecision(ctx, pinned.TemplateID, "skipped", "min_replicas_met")
		return
	}

	// g. Check max_replicas ceiling.
	if readyOnMatching >= pinned.MaxReplicas {
		logDecision(ctx, pinned.TemplateID, "skipped", "max_replicas_reached")
		return
	}

	// h. Check active job dedup (from batch).
	if activeJobs[pinned.TemplateID] {
		logDecision(ctx, pinned.TemplateID, "skipped", "active_job")
		return
	}

	// i. Check per-template cooldown.
	if !cooldownElapsed(pinned.TemplateID, cfg.PerTemplateMinRedoInterval) {
		logDecision(ctx, pinned.TemplateID, "skipped", "cooldown")
		return
	}

	// j. Select candidate nodes.
	deficit := pinned.MinReplicas - readyOnMatching
	maxAdmitable := pinned.MaxReplicas - readyOnMatching
	admitCount := deficit
	if maxAdmitable < admitCount {
		admitCount = maxAdmitable
	}
	if admitCount <= 0 {
		logDecision(ctx, pinned.TemplateID, "skipped", "no_deficit")
		return
	}

	candidates := selectCandidates(
		matchingNodes, existingReady, admitCount,
		nodeCounts, nodeBytes, cfg, templateSize,
	)
	if len(candidates) == 0 {
		logDecision(ctx, pinned.TemplateID, "skipped", "no_candidates")
		return
	}

	// k. Submit redo with DistributionScope.
	scope := make([]string, 0, len(candidates))
	for _, n := range candidates {
		scope = append(scope, n.NodeID)
	}
	req := &types.RedoTemplateFromImageReq{
		Request:           &types.Request{RequestID: uuid.NewString()},
		TemplateID:        pinned.TemplateID,
		DistributionScope: scope,
	}

	jobInfo, err := SubmitRedoTemplateFromImage(ctx, req, cfg.DownloadBaseURL)
	if err != nil {
		if errors.Is(err, ErrTemplateAttemptInProgress) {
			logDecision(ctx, pinned.TemplateID, "skipped", "active_job")
		} else {
			logger.Warnf("preheat: submit redo for %s failed: %v", pinned.TemplateID, err)
			logDecision(ctx, pinned.TemplateID, "failed", "submit_error")
		}
		return
	}

	lastRedoSubmitByTemplate[pinned.TemplateID] = time.Now()
	logger.Infof("preheat: submitted redo for %s on %d nodes, job=%s",
		pinned.TemplateID, len(candidates), jobInfo.JobID)
	preheatDecisionTotal.WithLabelValues("submitted", "").Inc()
}

// ---------------------------------------------------------------------------
// Instance-type filter + node-selector matching
// ---------------------------------------------------------------------------

// filterNodesForTemplate returns healthy nodes that match both the operator's
// node_selector AND the template's own instance type. The instance type filter
// is mandatory because resolveTemplateNodes (in the redo path) filters by
// instance type first — a wrong-type node in DistributionScope fails the
// entire redo (all-or-nothing).
func filterNodesForTemplate(
	nodes []*nodemeta.NodeSnapshot,
	selector map[string]string,
	templateInstanceType string,
) []*nodemeta.NodeSnapshot {
	var out []*nodemeta.NodeSnapshot
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.InstanceType != templateInstanceType {
			continue
		}
		if !matchNodeSelector(n, selector) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// matchNodeSelector checks node labels against the selector map.
// An empty selector {} matches all nodes of the correct instance type.
func matchNodeSelector(node *nodemeta.NodeSnapshot, selector map[string]string) bool {
	for k, v := range selector {
		nodeVal, ok := node.Labels[k]
		if !ok || nodeVal != v {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Candidate selection with budget enforcement
// ---------------------------------------------------------------------------

func selectCandidates(
	matchingNodes []*nodemeta.NodeSnapshot,
	existingReady map[string]struct{},
	admitCount int,
	nodeCounts map[string]int,
	nodeBytes map[string]int64,
	cfg *config.TemplatePreheatConf,
	templateSize int64,
) []*nodemeta.NodeSnapshot {
	selected := make([]*nodemeta.NodeSnapshot, 0, admitCount)
	for _, node := range matchingNodes {
		if len(selected) >= admitCount {
			break
		}
		if _, ok := existingReady[node.NodeID]; ok {
			continue
		}
		if cfg.PerNodeMaxTemplates > 0 && nodeCounts[node.NodeID] >= cfg.PerNodeMaxTemplates {
			continue
		}
		if cfg.PerNodeMaxBytes > 0 && nodeBytes[node.NodeID]+templateSize > cfg.PerNodeMaxBytes {
			continue
		}
		selected = append(selected, node)
		// Optimistic increment so subsequent templates in the same pass
		// see the updated budget.
		nodeCounts[node.NodeID]++
		nodeBytes[node.NodeID] += templateSize
	}
	return selected
}

// ---------------------------------------------------------------------------
// Template size lookup
// ---------------------------------------------------------------------------

func getTemplateSizeFromDef(ctx context.Context, def *models.TemplateDefinition) int64 {
	if def.RootfsSizeBytesAtSnapshot > 0 {
		return int64(def.RootfsSizeBytesAtSnapshot)
	}
	if def.RootfsArtifactID == "" {
		return 0
	}
	var artifact models.RootfsArtifact
	err := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).
		Where("artifact_id = ?", def.RootfsArtifactID).First(&artifact).Error
	if err != nil {
		return 0
	}
	return artifact.Ext4SizeBytes
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func filterHealthyNodes(nodes []*nodemeta.NodeSnapshot) []*nodemeta.NodeSnapshot {
	var out []*nodemeta.NodeSnapshot
	for _, n := range nodes {
		if n != nil && n.Healthy {
			out = append(out, n)
		}
	}
	return out
}

func sortPinnedTemplates(pinned []config.PinnedTemplateConf) []config.PinnedTemplateConf {
	out := make([]config.PinnedTemplateConf, len(pinned))
	copy(out, pinned)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].TemplateID < out[j].TemplateID
	})
	return out
}

func collectCandidateNodeIDs(
	pinned []config.PinnedTemplateConf,
	healthyNodes []*nodemeta.NodeSnapshot,
	defs map[string]*models.TemplateDefinition,
) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, p := range pinned {
		def, ok := defs[p.TemplateID]
		if !ok {
			continue
		}
		matching := filterNodesForTemplate(healthyNodes, p.NodeSelector, def.InstanceType)
		for _, n := range matching {
			if _, ok := seen[n.NodeID]; ok {
				continue
			}
			seen[n.NodeID] = struct{}{}
			out = append(out, n.NodeID)
		}
	}
	return out
}

func nodeSnapshotIDSet(nodes []*nodemeta.NodeSnapshot) map[string]struct{} {
	out := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		if n != nil {
			out[n.NodeID] = struct{}{}
		}
	}
	return out
}

func logDecision(ctx context.Context, templateID, decision, reason string) {
	log.G(ctx).WithFields(map[string]any{
		"template_id": templateID,
		"decision":    decision,
		"reason":      reason,
	}).Debug("preheat decision")
	preheatDecisionTotal.WithLabelValues(decision, reason).Inc()
}
