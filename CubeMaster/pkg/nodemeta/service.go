// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/nodehealth"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	corev1 "k8s.io/api/core/v1"
)

type ResourceSnapshot struct {
	MilliCPU int64 `json:"milli_cpu,omitempty"`
	MemoryMB int64 `json:"memory_mb,omitempty"`
}

// ComponentVersion mirrors the cubelet-side masterclient.ComponentVersion.
// It carries the real version of one component installed on a node. Source is
// one of "manifest" | "binary" | "file".
type ComponentVersion struct {
	Component string `json:"component"`
	Version   string `json:"version,omitempty"`
	Commit    string `json:"commit,omitempty"`
	BuildTime string `json:"build_time,omitempty"`
	Source    string `json:"source,omitempty"`
}

type ContainerImage struct {
	Names     []string `json:"names,omitempty"`
	SizeBytes int64    `json:"size_bytes,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	MediaType string   `json:"media_type,omitempty"`
}

type LocalTemplate struct {
	TemplateID string `json:"template_id,omitempty"`
	ID         string `json:"id,omitempty"`
	Media      string `json:"media,omitempty"`
	Path       string `json:"path,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
}

type RegisterNodeRequest struct {
	RequestID           string             `json:"requestID,omitempty"`
	NodeID              string             `json:"node_id,omitempty"`
	HostIP              string             `json:"host_ip,omitempty"`
	GRPCPort            int                `json:"grpc_port,omitempty"`
	Labels              map[string]string  `json:"labels,omitempty"`
	Capacity            ResourceSnapshot   `json:"capacity,omitempty"`
	Allocatable         ResourceSnapshot   `json:"allocatable,omitempty"`
	InstanceType        string             `json:"instance_type,omitempty"`
	ClusterLabel        string             `json:"cluster_label,omitempty"`
	QuotaCPU            int64              `json:"quota_cpu,omitempty"`
	QuotaMemMB          int64              `json:"quota_mem_mb,omitempty"`
	CreateConcurrentNum int64              `json:"create_concurrent_num,omitempty"`
	MaxMvmNum           int64              `json:"max_mvm_num,omitempty"`
	Versions            []ComponentVersion `json:"versions,omitempty"`
}

type UpdateNodeStatusRequest struct {
	RequestID      string                 `json:"requestID,omitempty"`
	Conditions     []corev1.NodeCondition `json:"conditions,omitempty"`
	Images         []ContainerImage       `json:"images,omitempty"`
	LocalTemplates []LocalTemplate        `json:"local_templates,omitempty"`
	HeartbeatTime  time.Time              `json:"heartbeat_time,omitempty"`

	Allocated  *AllocatedResources `json:"allocated,omitempty"`
	DiskUsage  *DiskUsage          `json:"disk_usage,omitempty"`
	MetricTime time.Time           `json:"metric_time,omitempty"`

	Versions []ComponentVersion `json:"versions,omitempty"`
}

// AllocatedResources is cubelet-side aggregation of sandbox-quota CPU /
// memory / disk and counts for sandboxes currently held on the node. Field
// naming aligns with the scheduler-facing Redis schema (RedisNodeInfo).
type AllocatedResources struct {
	MilliCPU      int64 `json:"milli_cpu,omitempty"`
	MemoryMB      int64 `json:"memory_mb,omitempty"`
	MvmNum        int64 `json:"mvm_num,omitempty"`
	MvmRunningNum int64 `json:"mvm_running_num,omitempty"`
	NicQueues     int64 `json:"nic_queues,omitempty"`

	DataDiskMB    int64 `json:"data_disk_mb,omitempty"`
	StorageDiskMB int64 `json:"storage_disk_mb,omitempty"`
}

// DiskUsage carries actual filesystem fill ratios observed by cubelet
// (0~100). Each dimension is optional.
type DiskUsage struct {
	DataDiskUsagePer    float64 `json:"data_disk_usage_per,omitempty"`
	StorageDiskUsagePer float64 `json:"storage_disk_usage_per,omitempty"`
	SysDiskUsagePer     float64 `json:"sys_disk_usage_per,omitempty"`
}

type NodeSnapshot struct {
	NodeID              string                 `json:"node_id,omitempty"`
	HostIP              string                 `json:"host_ip,omitempty"`
	GRPCPort            int                    `json:"grpc_port,omitempty"`
	Labels              map[string]string      `json:"labels,omitempty"`
	Capacity            ResourceSnapshot       `json:"capacity,omitempty"`
	Allocatable         ResourceSnapshot       `json:"allocatable,omitempty"`
	InstanceType        string                 `json:"instance_type,omitempty"`
	ClusterLabel        string                 `json:"cluster_label,omitempty"`
	QuotaCPU            int64                  `json:"quota_cpu,omitempty"`
	QuotaMemMB          int64                  `json:"quota_mem_mb,omitempty"`
	CreateConcurrentNum int64                  `json:"create_concurrent_num,omitempty"`
	MaxMvmNum           int64                  `json:"max_mvm_num,omitempty"`
	Conditions          []corev1.NodeCondition `json:"conditions,omitempty"`
	Images              []ContainerImage       `json:"images,omitempty"`
	LocalTemplates      []LocalTemplate        `json:"local_templates,omitempty"`
	Versions            []ComponentVersion     `json:"versions,omitempty"`
	HeartbeatTime       time.Time              `json:"heartbeat_time,omitempty"`
	ReportedReady       bool                   `json:"-"`
	Healthy             bool                   `json:"healthy"`
	UnhealthyReason     string                 `json:"unhealthy_reason,omitempty"`
	// Isolated marks an operator-applied cordon: the node stays visible and
	// keeps running existing instances, but the scheduler will not place new
	// instances on it. It is an administrative flag, independent of the
	// heartbeat-derived Healthy state.
	Isolated       bool   `json:"isolated"`
	IsolatedAt     int64  `json:"isolated_at,omitempty"`
	IsolatedBy     string `json:"isolated_by,omitempty"`
	IsolatedReason string `json:"isolated_reason,omitempty"`
	// versionsHash is the content hash of Versions, used to skip redundant DB
	// writes on every heartbeat. Not serialised to JSON.
	versionsHash string
}

type service struct {
	db    *gorm.DB
	mu    sync.RWMutex
	ready bool
	nodes map[string]*NodeSnapshot

	// declaredVersions is loaded once from the local release manifest during
	// service startup. The manifest is deployed as an immutable release asset,
	// so version-matrix reads should not parse it on every request.
	declaredVersions    map[string]string
	declaredVersionSets map[string]map[string]struct{}

	// versionWriteLocks serialises the hash-check/write/update sequence per
	// node so concurrent heartbeats cannot race each other and issue redundant
	// version writes or overwrite a newer in-memory hash with an older one.
	versionWriteLocks sync.Map

	// isolationWriteLocks serialises isolation read-modify-write per node so
	// concurrent isolate/un-isolate requests cannot interleave DB and in-memory
	// updates into an inconsistent state.
	isolationWriteLocks sync.Map
}

// isolationState mirrors the persisted isolation columns of a node.
type isolationState struct {
	Isolated       bool
	IsolatedAt     int64
	IsolatedBy     string
	IsolatedReason string
}

var global = &service{
	nodes:               make(map[string]*NodeSnapshot),
	declaredVersions:    map[string]string{},
	declaredVersionSets: map[string]map[string]struct{}{},
}

// OnGuestAgentVersionChanged is registered by template compatibility
// management. It must stay in nodemeta to avoid a package import cycle:
// nodemeta never imports templatecenter.
var OnGuestAgentVersionChanged func(nodeID string)

func Init(ctx context.Context) error {
	// Schema is owned by pkg/base/dao/migrate and applied at startup
	// before any package Init runs.
	global.db = db.Init(config.GetDbConfig())
	declaredInfo := loadDeclaredVersionInfo()
	global.declaredVersions = declaredInfo.Primary
	global.declaredVersionSets = declaredInfo.Sets
	if err := global.reload(); err != nil {
		return err
	}
	localcache.RegisterNodeLoader(ListSchedulerNodes)
	recov.GoWithRecover(func() {
		global.loopReconcileIsolation(ctx)
	}, func(panicErr interface{}) {
		log.G(ctx).Errorf("loopReconcileIsolation panic: %v", panicErr)
	})
	global.ready = true
	return nil
}

// isolationReconcileInterval controls how quickly an isolation change applied
// on one cubemaster replica propagates to the others. The registration table
// is the source of truth; each replica polls only the isolation columns and
// patches its in-memory snapshots. Kept short so the cordon "leak window" on
// peer replicas stays small.
const isolationReconcileInterval = 5 * time.Second

// loopReconcileIsolation periodically reconciles the isolation columns from the
// registration table into the in-memory snapshots. This is the cross-replica
// propagation mechanism: a write done via SetNodeIsolated on replica A becomes
// visible to replica B within one interval, even though B's snapshots are
// otherwise only mutated by the cubelets that heartbeat to B.
func (s *service) loopReconcileIsolation(ctx context.Context) {
	ticker := time.NewTicker(isolationReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileIsolation(ctx)
		}
	}
}

func (s *service) reconcileIsolation(ctx context.Context) {
	isolatedNodeIDs := s.listInMemoryIsolatedNodeIDs()
	regs := make([]*models.NodeRegistration, 0)
	query := s.db.Table(constants.NodeMetaRegistrationTable).
		Select("node_id", "isolated", "isolated_at", "isolated_by", "isolated_reason")
	if len(isolatedNodeIDs) > 0 {
		query = query.Where("isolated = ? OR node_id IN ?", true, isolatedNodeIDs)
	} else {
		query = query.Where("isolated = ?", true)
	}
	if err := query.Find(&regs).Error; err != nil {
		log.G(ctx).Warnf("reconcile isolation: load registrations failed: %v", err)
		return
	}
	changed := make([]*NodeSnapshot, 0)
	s.mu.Lock()
	for _, reg := range regs {
		snap, ok := s.nodes[reg.NodeID]
		if !ok || snap == nil {
			continue
		}
		if snap.Isolated == reg.Isolated &&
			snap.IsolatedAt == reg.IsolatedAt &&
			snap.IsolatedBy == reg.IsolatedBy &&
			snap.IsolatedReason == reg.IsolatedReason {
			continue
		}
		snap.Isolated = reg.Isolated
		snap.IsolatedAt = reg.IsolatedAt
		snap.IsolatedBy = reg.IsolatedBy
		snap.IsolatedReason = reg.IsolatedReason
		changed = append(changed, cloneSnapshot(snap))
	}
	s.mu.Unlock()
	for _, snap := range changed {
		log.G(ctx).Infof("reconcile isolation: node=%s isolated=%v", snap.NodeID, snap.Isolated)
		syncLocalcache(snap)
	}
}

func (s *service) listInMemoryIsolatedNodeIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0)
	for nodeID, snap := range s.nodes {
		if snap != nil && snap.Isolated {
			ids = append(ids, nodeID)
		}
	}
	sort.Strings(ids)
	return ids
}

func Ready() bool {
	global.mu.RLock()
	defer global.mu.RUnlock()
	return global.ready
}

func RegisterNode(ctx context.Context, req *RegisterNodeRequest) (*NodeSnapshot, error) {
	if req == nil || req.NodeID == "" {
		return nil, fmt.Errorf("node_id is required")
	}
	if req.HostIP == "" {
		req.HostIP = req.NodeID
	}
	reg := &models.NodeRegistration{
		NodeID:              req.NodeID,
		HostIP:              req.HostIP,
		GRPCPort:            req.GRPCPort,
		LabelsJSON:          mustJSON(req.Labels),
		CapacityJSON:        mustJSON(req.Capacity),
		AllocatableJSON:     mustJSON(req.Allocatable),
		InstanceType:        req.InstanceType,
		ClusterLabel:        req.ClusterLabel,
		QuotaCPU:            req.QuotaCPU,
		QuotaMemMB:          req.QuotaMemMB,
		CreateConcurrentNum: req.CreateConcurrentNum,
		MaxMvmNum:           req.MaxMvmNum,
	}
	if err := global.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "node_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"host_ip", "grpc_port", "labels_json", "capacity_json", "allocatable_json",
			"instance_type", "cluster_label", "quota_cpu", "quota_mem_mb",
			"create_concurrent_num", "max_mvm_num", "updated_at",
		}),
	}).Create(reg).Error; err != nil {
		return nil, err
	}

	// Re-read the persisted isolation columns: the register upsert above
	// deliberately does not touch them, so a node that was isolated on another
	// replica (or before a restart) must have that administrative state
	// reflected here. Without this backfill a cubelet re-registration would
	// leave the in-memory snapshot with Isolated=false and silently allow the
	// scheduler on this replica to place new instances on a cordoned node.
	//
	// Hold the per-node isolation lock across the read+apply so a concurrent
	// SetNodeIsolated cannot land its DB+memory write in between (which would
	// otherwise let this stale read clobber a just-applied cordon until the
	// next reconcile tick).
	unlockIso := global.lockIsolationWrite(req.NodeID)
	iso := loadIsolationFromDB(req.NodeID)

	snap := global.ensureNode(req.NodeID)
	global.mu.Lock()
	snap.NodeID = req.NodeID
	snap.HostIP = req.HostIP
	snap.GRPCPort = req.GRPCPort
	snap.Labels = cloneStringMap(req.Labels)
	snap.Capacity = req.Capacity
	snap.Allocatable = req.Allocatable
	snap.InstanceType = req.InstanceType
	snap.ClusterLabel = req.ClusterLabel
	snap.QuotaCPU = req.QuotaCPU
	snap.QuotaMemMB = req.QuotaMemMB
	snap.CreateConcurrentNum = req.CreateConcurrentNum
	snap.MaxMvmNum = req.MaxMvmNum
	if iso != nil {
		snap.Isolated = iso.Isolated
		snap.IsolatedAt = iso.IsolatedAt
		snap.IsolatedBy = iso.IsolatedBy
		snap.IsolatedReason = iso.IsolatedReason
	}
	applyCurrentHealth(snap, time.Now())
	global.mu.Unlock()
	syncLocalcache(snap)
	unlockIso()
	global.persistVersions(ctx, req.NodeID, req.Versions)
	return cloneSnapshot(snap), nil
}

func UpdateNodeStatus(ctx context.Context, nodeID string, req *UpdateNodeStatusRequest) (*NodeSnapshot, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("node_id is required")
	}
	if req == nil {
		req = &UpdateNodeStatusRequest{}
	}
	if req.HeartbeatTime.IsZero() {
		req.HeartbeatTime = time.Now()
	}
	reportedReady := nodehealth.ReadyConditionTrue(req.Conditions)
	status := &models.NodeStatus{
		NodeID:             nodeID,
		ConditionsJSON:     mustJSON(req.Conditions),
		ImagesJSON:         mustJSON(req.Images),
		LocalTemplatesJSON: mustJSON(req.LocalTemplates),
		HeartbeatUnix:      req.HeartbeatTime.Unix(),
		Healthy:            reportedReady,
	}
	if err := global.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "node_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"conditions_json", "images_json", "local_templates_json",
			"heartbeat_unix", "healthy", "updated_at",
		}),
	}).Create(status).Error; err != nil {
		return nil, err
	}

	snap := global.ensureNode(nodeID)
	global.mu.Lock()
	snap.NodeID = nodeID
	snap.Conditions = append([]corev1.NodeCondition(nil), req.Conditions...)
	snap.Images = append([]ContainerImage(nil), req.Images...)
	snap.LocalTemplates = append([]LocalTemplate(nil), req.LocalTemplates...)
	snap.HeartbeatTime = req.HeartbeatTime
	snap.ReportedReady = reportedReady
	applyCurrentHealth(snap, time.Now())
	global.mu.Unlock()
	syncLocalcache(snap)

	// Resource metrics flow via Redis (shared across cubemaster replicas)
	// and in-process cache (immediate visibility for this replica). They
	// are intentionally not persisted to MySQL: every 10s heartbeat from
	// hundreds of nodes would otherwise dominate write traffic, and Redis
	// already provides the cross-replica fan-out used by the scheduler.
	fanOutResourceMetric(ctx, nodeID, req)
	global.persistVersions(ctx, nodeID, req.Versions)
	return cloneSnapshot(snap), nil
}

// persistVersions records the node's component versions, skipping the DB
// write entirely when the reported set is unchanged (content-hash compare
// against the in-memory snapshot). This keeps the 10s heartbeat from turning
// slow-changing version data into a MySQL write storm.
func (s *service) persistVersions(ctx context.Context, nodeID string, versions []ComponentVersion) {
	s.persistVersionsWithWriter(ctx, nodeID, versions, s.writeVersions)
}

func (s *service) persistVersionsWithWriter(
	ctx context.Context,
	nodeID string,
	versions []ComponentVersion,
	writer func(string, []ComponentVersion) error,
) {
	if len(versions) == 0 {
		return
	}
	unlock := s.lockVersionWrite(nodeID)
	defer unlock()
	h := versionsHash(versions)
	snap := s.ensureNode(nodeID)
	s.mu.RLock()
	unchanged := snap.versionsHash == h
	prevCompat := compatRelevantVersions(snap.Versions)
	s.mu.RUnlock()
	if unchanged {
		log.G(ctx).Debugf("version_write_skipped node=%s", nodeID)
		return
	}
	if err := writer(nodeID, versions); err != nil {
		log.G(ctx).Warnf("write node component versions failed for %s: %v", nodeID, err)
		return
	}
	s.mu.Lock()
	snap.Versions = append([]ComponentVersion(nil), versions...)
	snap.versionsHash = h
	s.mu.Unlock()
	log.G(ctx).Debugf("version_write_applied node=%s components=%d", nodeID, len(versions))
	if OnGuestAgentVersionChanged != nil && compatVersionsChanged(prevCompat, compatRelevantVersions(versions)) {
		go OnGuestAgentVersionChanged(nodeID)
	}
}

func (s *service) lockVersionWrite(nodeID string) func() {
	lockAny, _ := s.versionWriteLocks.LoadOrStore(nodeID, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func (s *service) lockIsolationWrite(nodeID string) func() {
	lockAny, _ := s.isolationWriteLocks.LoadOrStore(nodeID, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

// loadIsolationFromDB reads the persisted isolation columns for a node. Returns
// nil when the registration row does not exist or the read fails, so callers
// fall back to the existing in-memory value rather than wrongly clearing it.
func loadIsolationFromDB(nodeID string) *isolationState {
	reg := &models.NodeRegistration{}
	if err := global.db.Table(constants.NodeMetaRegistrationTable).
		Select("isolated", "isolated_at", "isolated_by", "isolated_reason").
		Where("node_id = ?", nodeID).
		Take(reg).Error; err != nil {
		return nil
	}
	return &isolationState{
		Isolated:       reg.Isolated,
		IsolatedAt:     reg.IsolatedAt,
		IsolatedBy:     reg.IsolatedBy,
		IsolatedReason: reg.IsolatedReason,
	}
}

// SetNodeIsolated applies or clears an administrative cordon on a node. The
// node must already be registered (no ghost nodes are created). The isolation
// columns are persisted to the registration row, the in-memory snapshot is
// updated, and the scheduler cache is synced immediately so this replica stops
// (or resumes) placing new instances on the node without waiting for the next
// heartbeat. Peer replicas converge via loopReconcileIsolation.
func SetNodeIsolated(ctx context.Context, nodeID string, isolated bool, by, reason string) (*NodeSnapshot, error) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return nil, fmt.Errorf("node_id is required")
	}

	unlock := global.lockIsolationWrite(nodeID)
	defer unlock()

	// Require an existing registration row. ensureNode would otherwise create a
	// phantom snapshot for an arbitrary id supplied by the caller.
	existing := &models.NodeRegistration{}
	if err := global.db.Table(constants.NodeMetaRegistrationTable).
		Select("node_id").
		Where("node_id = ?", nodeID).
		Take(existing).Error; err != nil {
		return nil, err
	}

	now := time.Now()
	updates := map[string]interface{}{
		"isolated":   isolated,
		"updated_at": now,
	}
	if isolated {
		updates["isolated_at"] = now.Unix()
		updates["isolated_by"] = by
		updates["isolated_reason"] = reason
	} else {
		// Explicitly clear the metadata on un-isolate so a later reconcile or
		// reload does not resurrect stale operator/reason values.
		updates["isolated_at"] = 0
		updates["isolated_by"] = ""
		updates["isolated_reason"] = ""
	}
	if err := global.db.Table(constants.NodeMetaRegistrationTable).
		Where("node_id = ?", nodeID).
		Updates(updates).Error; err != nil {
		return nil, err
	}

	snap := global.ensureNode(nodeID)
	global.mu.Lock()
	snap.Isolated = isolated
	if isolated {
		snap.IsolatedAt = now.Unix()
		snap.IsolatedBy = by
		snap.IsolatedReason = reason
	} else {
		snap.IsolatedAt = 0
		snap.IsolatedBy = ""
		snap.IsolatedReason = ""
	}
	out := cloneSnapshotWithCurrentHealth(snap, time.Now())
	global.mu.Unlock()

	syncLocalcache(out)
	log.G(ctx).Infof("set node isolation node=%s isolated=%v by=%s", nodeID, isolated, by)
	return out, nil
}

// writeVersions upserts the reported component rows and physically removes
// any component previously recorded for the node but absent from this report.
// The table carries no soft-delete column, so Delete is a hard delete by
// design (see models.NodeComponentVersion).
func (s *service) writeVersions(nodeID string, versions []ComponentVersion) error {
	now := time.Now().Unix()
	rows := make([]*models.NodeComponentVersion, 0, len(versions))
	keep := make([]string, 0, len(versions))
	for _, v := range versions {
		if v.Component == "" {
			continue
		}
		rows = append(rows, &models.NodeComponentVersion{
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
	return s.db.Transaction(func(tx *gorm.DB) error {
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
		del := tx.Where("node_id = ?", nodeID)
		if len(keep) > 0 {
			del = del.Where("component NOT IN ?", keep)
		}
		return del.Delete(&models.NodeComponentVersion{}).Error
	})
}

// versionsHash returns a stable content hash of the version set, order
// independent (components are sorted first).
func versionsHash(versions []ComponentVersion) string {
	if len(versions) == 0 {
		return ""
	}
	sorted := append([]ComponentVersion(nil), versions...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Component < sorted[j].Component })
	h := fnv.New64a()
	for _, v := range sorted {
		fmt.Fprintf(h, "%s|%s|%s|%s|%s\n", v.Component, v.Version, v.Commit, v.BuildTime, v.Source)
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

func compatRelevantVersions(versions []ComponentVersion) map[string]string {
	out := map[string]string{
		"guest-image": "",
		"cube-agent":  "",
	}
	for _, v := range versions {
		switch v.Component {
		case "guest-image", "cube-agent":
			out[v.Component] = strings.TrimSpace(v.Version)
		}
	}
	return out
}

func compatVersionsChanged(prev, next map[string]string) bool {
	for _, component := range []string{"guest-image", "cube-agent"} {
		if strings.TrimSpace(prev[component]) != strings.TrimSpace(next[component]) {
			return true
		}
	}
	return false
}

// GetNodeComponentVersions returns the current trusted guest-environment
// versions for a healthy node. The boolean is false when the node is unknown,
// unhealthy, or its heartbeat has expired; callers should treat that as
// UNKNOWN rather than reusing stale DB values.
func GetNodeComponentVersions(ctx context.Context, nodeID string) (map[string]string, bool) {
	_ = ctx
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return nil, false
	}
	global.mu.RLock()
	snap, ok := global.nodes[nodeID]
	if !ok || snap == nil {
		global.mu.RUnlock()
		return nil, false
	}
	cloned := cloneSnapshotWithCurrentHealth(snap, time.Now())
	global.mu.RUnlock()
	if !cloned.Healthy {
		return nil, false
	}
	return compatRelevantVersions(cloned.Versions), true
}

// fanOutResourceMetric is best-effort: write failures to Redis fall back
// to in-process update so the receiving replica still schedules correctly,
// and the next heartbeat (≤NodeStatusUpdateFrequency) reattempts the write.
func fanOutResourceMetric(ctx context.Context, nodeID string, req *UpdateNodeStatusRequest) {
	if req == nil || (req.Allocated == nil && req.DiskUsage == nil) {
		return
	}
	metricTime := req.MetricTime
	if metricTime.IsZero() {
		metricTime = time.Now()
	}
	m := &localcache.NodeMetric{NodeID: nodeID, MetricTime: metricTime}
	// HasAllocated / HasDisk track which sub-structures the cubelet
	// actually populated, so the downstream writers can skip the other
	// group entirely instead of overwriting it with zero values.
	if a := req.Allocated; a != nil {
		m.HasAllocated = true
		m.MilliCPUUsage = a.MilliCPU
		m.MemoryMBUsage = a.MemoryMB
		m.MvmNum = a.MvmNum
		m.NicQueues = a.NicQueues
	}
	if d := req.DiskUsage; d != nil {
		m.HasDisk = true
		m.DataDiskUsagePer = d.DataDiskUsagePer
		m.StorageDiskUsagePer = d.StorageDiskUsagePer
		m.SysDiskUsagePer = d.SysDiskUsagePer
	}
	if err := localcache.WriteNodeMetric(ctx, m); err != nil {
		log.G(ctx).Warnf("write node metric to redis failed for %s: %v", nodeID, err)
	}
	if err := localcache.UpdateNodeMetricInProcess(m); err != nil {
		// Missing in-process entry is normal during cold start (this
		// replica has not yet reloaded the registration). Other replicas
		// pick up the metric via Redis tick, and this one will converge
		// on the next reload cycle.
		log.G(ctx).Debugf("in-process metric update skipped for %s: %v", nodeID, err)
	}
}

func GetNode(ctx context.Context, nodeID string) (*NodeSnapshot, error) {
	_ = ctx
	global.mu.RLock()
	defer global.mu.RUnlock()
	snap, ok := global.nodes[nodeID]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return cloneSnapshotWithCurrentHealth(snap, time.Now()), nil
}

func ListNodes(ctx context.Context) ([]*NodeSnapshot, error) {
	_ = ctx
	global.mu.RLock()
	defer global.mu.RUnlock()
	out := make([]*NodeSnapshot, 0, len(global.nodes))
	now := time.Now()
	for _, snap := range global.nodes {
		out = append(out, cloneSnapshotWithCurrentHealth(snap, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

func ListSchedulerNodes(ctx context.Context) ([]*node.Node, error) {
	snaps, err := ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*node.Node, 0, len(snaps))
	for _, snap := range snaps {
		out = append(out, toSchedulerNode(snap))
	}
	return out, nil
}

func (s *service) ensureNode(nodeID string) *NodeSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap, ok := s.nodes[nodeID]; ok {
		return snap
	}
	snap := &NodeSnapshot{NodeID: nodeID}
	s.nodes[nodeID] = snap
	return snap
}

func (s *service) reload() error {
	regs := make([]*models.NodeRegistration, 0)
	if err := s.db.Table(constants.NodeMetaRegistrationTable).Find(&regs).Error; err != nil {
		return err
	}
	statuses := make([]*models.NodeStatus, 0)
	if err := s.db.Table(constants.NodeMetaStatusTable).Find(&statuses).Error; err != nil {
		return err
	}
	next := make(map[string]*NodeSnapshot, len(regs))
	for _, reg := range regs {
		snap := &NodeSnapshot{
			NodeID:              reg.NodeID,
			HostIP:              reg.HostIP,
			GRPCPort:            reg.GRPCPort,
			Labels:              map[string]string{},
			Capacity:            ResourceSnapshot{},
			Allocatable:         ResourceSnapshot{},
			InstanceType:        reg.InstanceType,
			ClusterLabel:        reg.ClusterLabel,
			QuotaCPU:            reg.QuotaCPU,
			QuotaMemMB:          reg.QuotaMemMB,
			CreateConcurrentNum: reg.CreateConcurrentNum,
			MaxMvmNum:           reg.MaxMvmNum,
			Isolated:            reg.Isolated,
			IsolatedAt:          reg.IsolatedAt,
			IsolatedBy:          reg.IsolatedBy,
			IsolatedReason:      reg.IsolatedReason,
		}
		_ = json.Unmarshal([]byte(reg.LabelsJSON), &snap.Labels)
		_ = json.Unmarshal([]byte(reg.CapacityJSON), &snap.Capacity)
		_ = json.Unmarshal([]byte(reg.AllocatableJSON), &snap.Allocatable)
		next[reg.NodeID] = snap
	}
	for _, st := range statuses {
		snap, ok := next[st.NodeID]
		if !ok {
			snap = &NodeSnapshot{NodeID: st.NodeID}
			next[st.NodeID] = snap
		}
		_ = json.Unmarshal([]byte(st.ConditionsJSON), &snap.Conditions)
		_ = json.Unmarshal([]byte(st.ImagesJSON), &snap.Images)
		_ = json.Unmarshal([]byte(st.LocalTemplatesJSON), &snap.LocalTemplates)
		snap.HeartbeatTime = time.Unix(st.HeartbeatUnix, 0)
		snap.ReportedReady = st.Healthy
		applyCurrentHealth(snap, time.Now())
	}
	versions := make([]*models.NodeComponentVersion, 0)
	if err := s.db.Model(&models.NodeComponentVersion{}).Find(&versions).Error; err != nil {
		return err
	}
	for _, v := range versions {
		snap, ok := next[v.NodeID]
		if !ok {
			snap = &NodeSnapshot{NodeID: v.NodeID}
			next[v.NodeID] = snap
		}
		snap.Versions = append(snap.Versions, ComponentVersion{
			Component: v.Component,
			Version:   v.Version,
			Commit:    v.Commit,
			BuildTime: v.BuildTime,
			Source:    v.Source,
		})
	}
	for _, snap := range next {
		snap.versionsHash = versionsHash(snap.Versions)
	}
	s.mu.Lock()
	s.nodes = next
	s.mu.Unlock()
	return nil
}

func healthTimeout() time.Duration {
	return nodehealth.MetadataTimeout(config.GetConfig().Common.SyncMetaDataInterval)
}

func currentHealthStatus(snap *NodeSnapshot, now time.Time) nodehealth.Status {
	if snap == nil {
		return nodehealth.Status{Healthy: false, UnhealthyReason: nodehealth.ReasonHeartbeatExpired}
	}
	return nodehealth.EvaluateFromFacts(snap.ReportedReady, snap.HeartbeatTime, now, healthTimeout())
}

func applyCurrentHealth(snap *NodeSnapshot, now time.Time) {
	if snap == nil {
		return
	}
	status := currentHealthStatus(snap, now)
	snap.Healthy = status.Healthy
	snap.UnhealthyReason = status.UnhealthyReason
}

func toSchedulerNode(snap *NodeSnapshot) *node.Node {
	if snap == nil {
		return nil
	}
	quotaCPU := snap.QuotaCPU
	if quotaCPU == 0 {
		quotaCPU = snap.Allocatable.MilliCPU
	}
	quotaMem := snap.QuotaMemMB
	if quotaMem == 0 {
		quotaMem = snap.Allocatable.MemoryMB
	}
	hostIP := snap.HostIP
	if hostIP == "" {
		hostIP = snap.NodeID
	}
	instanceType := snap.InstanceType
	if instanceType == "" {
		instanceType = constants.DefaultInstanceTypeName
	}
	return &node.Node{
		InsID:               snap.NodeID,
		UUID:                snap.NodeID,
		IP:                  hostIP,
		CpuTotal:            int(snap.Capacity.MilliCPU / 1000),
		MemMBTotal:          snap.Capacity.MemoryMB,
		QuotaCpu:            quotaCPU,
		QuotaMem:            quotaMem,
		ClusterLabel:        snap.ClusterLabel,
		OssClusterLabel:     snap.ClusterLabel,
		InstanceType:        instanceType,
		HostStatus:          constants.HostStatusRunning,
		ReportedReady:       snap.ReportedReady,
		Healthy:             snap.Healthy,
		UnhealthyReason:     snap.UnhealthyReason,
		CreateConcurrentNum: snap.CreateConcurrentNum,
		MaxMvmLimit:         snap.MaxMvmNum,
		MetaDataUpdateAt:    snap.HeartbeatTime,
		Isolated:            snap.Isolated,
		NodeLabels:          cloneStringMap(snap.Labels),
		// MetricUpdate / MetricLocalUpdateAt are intentionally left
		// zero-valued here. They are owned by the resource-metric path
		// (Redis tick or UpdateNodeMetricInProcess) so prefilter's
		// MetricUpdateTimeout reflects metric freshness, not heartbeat
		// freshness. A node with a fresh heartbeat but no metric will
		// correctly be excluded by the timeout filter until cubelet
		// reports usage.
	}
}

func syncLocalcache(snap *NodeSnapshot) {
	localcache.UpsertNode(toSchedulerNode(snap))
	localcache.SyncNodeTemplates(snap.NodeID, templateIDsFromLocalTemplates(snap.LocalTemplates))
}

func templateIDsFromLocalTemplates(localTemplates []LocalTemplate) []string {
	if len(localTemplates) == 0 {
		return nil
	}
	templateIDs := make([]string, 0, len(localTemplates))
	for _, localTemplate := range localTemplates {
		if localTemplate.TemplateID == "" {
			continue
		}
		templateIDs = append(templateIDs, localTemplate.TemplateID)
	}
	return templateIDs
}

func cloneSnapshot(in *NodeSnapshot) *NodeSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	out.Labels = cloneStringMap(in.Labels)
	out.Conditions = append([]corev1.NodeCondition(nil), in.Conditions...)
	out.Images = append([]ContainerImage(nil), in.Images...)
	out.LocalTemplates = append([]LocalTemplate(nil), in.LocalTemplates...)
	out.Versions = append([]ComponentVersion(nil), in.Versions...)
	return &out
}

func cloneSnapshotWithCurrentHealth(in *NodeSnapshot, now time.Time) *NodeSnapshot {
	out := cloneSnapshot(in)
	applyCurrentHealth(out, now)
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mustJSON(v interface{}) string {
	if v == nil {
		return ""
	}
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}
