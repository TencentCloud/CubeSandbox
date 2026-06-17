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
	_ = ctx
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
	global.ready = true
	go global.loopReload(ctx)
	return nil
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
	applyCurrentHealth(snap, time.Now())
	global.mu.Unlock()
	syncLocalcache(snap)
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
	s.applyReloadResult(next)
	return nil
}

// applyReloadResult merges a DB snapshot (next) into the live in-memory map.
// Registration fields and versions always take the DB value; status/heartbeat
// fields keep the in-memory value when it is fresher than the DB snapshot.
func (s *service) applyReloadResult(next map[string]*NodeSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for nodeID, newSnap := range next {
		if existing, ok := s.nodes[nodeID]; ok {
			// Registration fields: take from DB.  A theoretical race exists
			// where the reload() DB scan captures a row before a concurrent
			// RegisterNode write commits, causing applyReloadResult to
			// overwrite a fresher in-memory value with a stale DB snapshot.
			// This is an accepted trade-off: registration field changes are
			// rare, and any inconsistency self-corrects on the next reload
			// cycle (≤ SyncMetaDataInterval).
			existing.Labels = cloneStringMap(newSnap.Labels)
			existing.Capacity = newSnap.Capacity
			existing.Allocatable = newSnap.Allocatable
			existing.InstanceType = newSnap.InstanceType
			existing.ClusterLabel = newSnap.ClusterLabel
			existing.QuotaCPU = newSnap.QuotaCPU
			existing.QuotaMemMB = newSnap.QuotaMemMB
			existing.CreateConcurrentNum = newSnap.CreateConcurrentNum
			existing.MaxMvmNum = newSnap.MaxMvmNum
			existing.HostIP = newSnap.HostIP
			existing.GRPCPort = newSnap.GRPCPort
			existing.Versions = append([]ComponentVersion(nil), newSnap.Versions...)
			existing.versionsHash = newSnap.versionsHash
			// Status fields: keep in-memory version if fresher to avoid
			// regressing heartbeat state that arrived during the DB scan.
			if newSnap.HeartbeatTime.After(existing.HeartbeatTime) {
				existing.Conditions = newSnap.Conditions
				existing.Images = newSnap.Images
				existing.LocalTemplates = newSnap.LocalTemplates
				existing.HeartbeatTime = newSnap.HeartbeatTime
				existing.ReportedReady = newSnap.ReportedReady
			}
			applyCurrentHealth(existing, time.Now())
		} else {
			// New node discovered from DB (registered on another replica).
			s.nodes[nodeID] = newSnap
		}
	}
}

func (s *service) loopReload(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	checkDeadline := time.Now().Add(config.GetConfig().Common.SyncMetaDataInterval)
	for {
		select {
		case <-ticker.C:
			recov.WithRecover(func() {
				if checkDeadline.After(time.Now()) {
					return
				}
				defer func() {
					checkDeadline = time.Now().Add(config.GetConfig().Common.SyncMetaDataInterval)
				}()
				if err := s.reload(); err != nil {
					log.G(ctx).Warnf("nodemeta periodic reload failed: %v", err)
				}
			}, func(panicError interface{}) {
				checkDeadline = time.Now().Add(config.GetConfig().Common.SyncMetaDataInterval)
				log.G(context.Background()).Fatalf("nodemeta loopReload panic: %v", panicError)
			})
		case <-ctx.Done():
			return
		}
	}
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
