// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	preheatReconcileInterval  = 5 * time.Minute
	preheatDebounceDelay      = 2 * time.Second
	preheatLockReleaseTimeout = 5 * time.Second
	preheatLockName           = "cubemaster_templatecenter_preheat_v1"
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
	runPreheatReconcilePassSafely(detachTemplateImageJobContext(ctx, "preheat", nil))

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
			runPreheatReconcilePassSafely(detachTemplateImageJobContext(ctx, "preheat", nil))
		case <-ticker.C:
			runPreheatReconcilePassSafely(detachTemplateImageJobContext(ctx, "preheat", nil))
		}
	}
}

// runPreheatReconcilePassSafely runs one reconcile pass, recovering from any
// panic so a single bad pass never kills the controller goroutine permanently.
// A background controller must outlive a transient fault in one pass.
func runPreheatReconcilePassSafely(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.G(ctx).Errorf("preheat: reconcile pass recovered from panic: %v\n%s", r, debug.Stack())
		}
	}()
	runPreheatReconcilePass(ctx)
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

	// Pin ONE physical DB connection for the whole pass so the advisory lock
	// (session-scoped) is acquired and released on the same session. Using two
	// pooled statements would run RELEASE_LOCK on a non-owning connection: it
	// returns NULL (not a SQL error), the lock leaks, and every subsequent pass
	// on every master sees the lock held and skips — disabling preheat
	// cluster-wide. This mirrors artifact_gc's Connection(func(sess){...}) +
	// trySessionLock/releaseSessionLock/discardPinnedSession pattern (which also
	// supports PostgreSQL pg_try_advisory_lock, unlike raw GET_LOCK).
	err := store.db.WithContext(ctx).Connection(func(sess *gorm.DB) (retErr error) {
		locked, err := trySessionLock(sess, preheatLockName)
		if err != nil {
			return errors.Join(fmt.Errorf("preheat: acquire lock: %w", err), discardPinnedSession(sess))
		}
		if !locked {
			return nil // another master is reconciling
		}
		defer func() {
			releaseCtx, releaseCancel := context.WithTimeout(
				context.WithoutCancel(ctx), preheatLockReleaseTimeout)
			defer releaseCancel()

			releaseSess := pinnedSessionWithContext(sess, releaseCtx)
			released, releaseErr := releaseSessionLock(releaseSess, preheatLockName)
			if releaseErr != nil {
				// Lock state unknown: discard the session so it (and any held
				// advisory lock) cannot silently re-enter the pool.
				retErr = errors.Join(retErr, fmt.Errorf("preheat: release lock: %w", releaseErr), discardPinnedSession(sess))
				return
			}
			if !released {
				retErr = errors.Join(retErr, errors.New("preheat: release lock: current session did not hold lock"))
			}
		}()

		runPreheatReconcileLocked(ctx, sess, preheatCfg)
		return nil
	})
	if err != nil {
		logger.Warnf("preheat: pass aborted: %v", err)
	}
}

// runPreheatReconcileLocked performs the reconcile work while holding the
// advisory lock. All reads use the pinned session (sess) for consistency with
// the single-connection model and to avoid checking out further connections.
// Template distribution via SubmitRedoTemplateFromImage intentionally uses the
// caller's ctx (the mature async distribution path) — it enqueues jobs and
// returns; it does not block the lock.
func runPreheatReconcileLocked(ctx context.Context, sess *gorm.DB, cfg *config.TemplatePreheatConf) {
	logger := log.G(ctx).WithFields(map[string]any{"component": "preheat"})

	// 1. List healthy, non-cordoned nodes (in-memory).
	nodes, err := nodemeta.ListNodes(ctx)
	if err != nil {
		logger.Warnf("preheat: list nodes failed: %v", err)
		return
	}
	healthyNodes := filterHealthyNodes(nodes)

	// 2. Sort pinned templates by priority (desc), tie-break on template_id.
	pinned := sortPinnedTemplates(cfg.PinnedTemplates)
	if len(pinned) == 0 {
		return
	}

	// 3. Batch query: all data for all pinned templates.
	templateIDs := make([]string, 0, len(pinned))
	for _, p := range pinned {
		templateIDs = append(templateIDs, p.TemplateID)
	}
	defs := batchGetDefinitions(ctx, sess, templateIDs)
	activeJobs := batchGetActiveJobs(ctx, sess, templateIDs)
	replicas := batchListReplicas(ctx, sess, templateIDs)
	sizes := batchGetTemplateSizes(ctx, sess, defs)

	// 4. Batch query: per-node budget usage for candidate nodes.
	candidateNodeIDs := collectCandidateNodeIDs(pinned, healthyNodes, defs)
	nodeCounts, nodeBytes := batchComputeNodeBudgetUsage(ctx, sess, candidateNodeIDs)

	// 5. Evaluate each pinned template using in-memory data.
	for _, p := range pinned {
		evaluatePinnedTemplate(ctx, cfg, p, healthyNodes,
			nodeCounts, nodeBytes, defs, activeJobs, replicas, sizes)
	}

	logger.Infof("preheat: pass complete, %d pinned templates evaluated", len(pinned))
}

// ---------------------------------------------------------------------------
// Batch queries
// ---------------------------------------------------------------------------

func batchGetDefinitions(ctx context.Context, db *gorm.DB, templateIDs []string) map[string]*models.TemplateDefinition {
	var defs []models.TemplateDefinition
	if err := db.WithContext(ctx).Table(constants.TemplateDefinitionTableName).
		Where("template_id IN ?", templateIDs).Find(&defs).Error; err != nil {
		log.G(ctx).Warnf("preheat: batch get definitions failed: %v", err)
	}
	out := make(map[string]*models.TemplateDefinition, len(defs))
	for i := range defs {
		out[defs[i].TemplateID] = &defs[i]
	}
	return out
}

func batchGetActiveJobs(ctx context.Context, db *gorm.DB, templateIDs []string) map[string]bool {
	var jobs []models.TemplateImageJob
	if err := db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("template_id IN ? AND status IN ?", templateIDs,
			[]string{JobStatusPending, JobStatusRunning}).Find(&jobs).Error; err != nil {
		log.G(ctx).Warnf("preheat: batch get active jobs failed: %v", err)
	}
	out := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		out[j.TemplateID] = true
	}
	return out
}

func batchListReplicas(ctx context.Context, db *gorm.DB, templateIDs []string) map[string][]models.TemplateReplica {
	var replicas []models.TemplateReplica
	if err := db.WithContext(ctx).Table(constants.TemplateReplicaTableName).
		Where("template_id IN ?", templateIDs).Find(&replicas).Error; err != nil {
		log.G(ctx).Warnf("preheat: batch list replicas failed: %v", err)
	}
	out := make(map[string][]models.TemplateReplica)
	for _, r := range replicas {
		out[r.TemplateID] = append(out[r.TemplateID], r)
	}
	return out
}

// batchGetTemplateSizes resolves the effective rootfs byte size for every
// template definition in a single query. The snapshot size wins; otherwise the
// artifact ext4 size is used (the same precedence the per-template lookup had,
// now batched to honor the "no N+1 per pass" contract). This is the size used
// for per-node byte budget accounting, so it must match the accounting in
// batchComputeNodeBudgetUsage (NULLIF(rootfs_size_bytes_at_snapshot,0) → ext4).
func batchGetTemplateSizes(ctx context.Context, db *gorm.DB, defs map[string]*models.TemplateDefinition) map[string]int64 {
	out := make(map[string]int64, len(defs))
	artifactIDs := make([]string, 0, len(defs))
	for _, d := range defs {
		if d == nil {
			continue
		}
		if d.RootfsSizeBytesAtSnapshot > 0 {
			out[d.TemplateID] = int64(d.RootfsSizeBytesAtSnapshot)
		} else if d.RootfsArtifactID != "" {
			artifactIDs = append(artifactIDs, d.RootfsArtifactID)
		}
	}
	if len(artifactIDs) == 0 {
		return out
	}
	var arts []models.RootfsArtifact
	if err := db.WithContext(ctx).Table(constants.RootfsArtifactTableName).
		Where("artifact_id IN ?", artifactIDs).Find(&arts).Error; err != nil {
		log.G(ctx).Warnf("preheat: batch get artifact sizes failed: %v", err)
		return out
	}
	artByID := make(map[string]int64, len(arts))
	for _, a := range arts {
		artByID[a.ArtifactID] = a.Ext4SizeBytes
	}
	for tid, d := range defs {
		if d == nil || d.RootfsSizeBytesAtSnapshot > 0 {
			continue // already resolved from snapshot size
		}
		if sz, ok := artByID[d.RootfsArtifactID]; ok {
			out[tid] = sz
		}
	}
	return out
}

// batchComputeNodeBudgetUsage computes per-node READY replica counts and total
// bytes. The 3-table JOIN ensures rootfs_size_bytes_at_snapshot=0 (image-based
// templates) falls back to rootfs_artifact.ext4_size_bytes, so the byte budget
// is enforced correctly for all template kinds.
func batchComputeNodeBudgetUsage(ctx context.Context, db *gorm.DB, nodeIDs []string) (map[string]int, map[string]int64) {
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
	if err := db.WithContext(ctx).
		Table(constants.TemplateReplicaTableName+" AS r").
		Select(`r.node_id,
			COUNT(*) AS cnt,
			COALESCE(SUM(
				COALESCE(NULLIF(d.rootfs_size_bytes_at_snapshot, 0),
					COALESCE(a.ext4_size_bytes, 0)
				)
			), 0) AS total_bytes`).
		Joins("LEFT JOIN "+constants.TemplateDefinitionTableName+" AS d ON r.template_id = d.template_id").
		Joins("LEFT JOIN "+constants.RootfsArtifactTableName+" AS a ON d.rootfs_artifact_id = a.artifact_id").
		Where("r.node_id IN ? AND r.status = ?", nodeIDs, ReplicaStatusReady).
		Group("r.node_id").
		Find(&results).Error; err != nil {
		log.G(ctx).Warnf("preheat: batch compute node budget usage failed: %v", err)
	}

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
	sizes map[string]int64,
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

	// b. Get template size for candidate budget check (from batch).
	templateSize := sizes[pinned.TemplateID]

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
	// Clamp to the available node count: admitCount is derived from operator
	// config and could exceed the matching set (or be a typo'd huge value).
	// make(..., 0, admitCount) would pre-allocate a multi-GB array otherwise.
	if admitCount > len(matchingNodes) {
		admitCount = len(matchingNodes)
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
// Helpers
// ---------------------------------------------------------------------------

// filterHealthyNodes returns nodes that are healthy AND schedulable. A cordoned
// node (SchedulingDisabled, e.g. under drain/quarantine) is excluded so preheat
// never pushes templates onto a node the operator is draining — consistent with
// the normal sandbox scheduler, which gates placement on SchedulingDisabled.
func filterHealthyNodes(nodes []*nodemeta.NodeSnapshot) []*nodemeta.NodeSnapshot {
	var out []*nodemeta.NodeSnapshot
	for _, n := range nodes {
		if n != nil && n.Healthy && !n.SchedulingDisabled {
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
