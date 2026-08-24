// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
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
// one of "manifest" | "binary" | "file" | "component-json".
type ComponentVersion struct {
	Component string `json:"component"`
	Version   string `json:"version,omitempty"`
	Commit    string `json:"commit,omitempty"`
	BuildTime string `json:"build_time,omitempty"`
	Source    string `json:"source,omitempty"`
	Variant   string `json:"variant,omitempty"` // kernel: bm|pvm
}

// HostFacts mirrors the cubelet-side masterclient.HostFacts. It carries the
// static host-level identity (CPU feature set, running host kernel, KVM ABI)
// used to judge cross-node snapshot restore compatibility.
type HostFacts struct {
	CPUVendor             string `json:"cpu_vendor,omitempty"`
	CPUModel              string `json:"cpu_model,omitempty"`
	CPUIDHash             string `json:"cpuid_hash,omitempty"`
	HostKernelRelease     string `json:"host_kernel_release,omitempty"`
	HostKernelFingerprint string `json:"host_kernel_fingerprint,omitempty"`
	KVMAPIVersion         int    `json:"kvm_api_version,omitempty"`
	KVMModuleFingerprint  string `json:"kvm_module_fingerprint,omitempty"`
	KVMModuleTaint        string `json:"kvm_module_taint,omitempty"`
	// KVMModuleScanned is a transient per-heartbeat collection signal, not a
	// persisted fact: it reports whether the cubelet successfully read /sys/module
	// this cycle, letting mergeIncomingHostFacts tell "module unloaded"
	// (authoritative empty) from "read gap" (preserve prev). It is zeroed before
	// the merged facts are stored or frozen onto a snapshot, so it never reaches
	// MySQL or the compatibility judgment.
	KVMModuleScanned bool `json:"kvm_module_scanned,omitempty"`
}

// IsZero reports whether no meaningful host fact was collected. KVMModuleScanned
// is a transient collection signal, not a fact, so it is excluded — a report
// carrying only Scanned=true is still "empty".
func (f *HostFacts) IsZero() bool {
	if f == nil {
		return true
	}
	return f.CPUVendor == "" && f.CPUModel == "" && f.CPUIDHash == "" &&
		f.HostKernelRelease == "" && f.HostKernelFingerprint == "" && f.KVMAPIVersion == 0 &&
		f.KVMModuleFingerprint == "" && f.KVMModuleTaint == ""
}

// RestoreMatchFactsJSON freezes only the two fields used for cross-node
// restore matching: cpuid_hash and host_kernel_release. Vendor/model/KVM
// extras stay on the live node heartbeat and are not written to snapshot
// or pause-snapshot rows.
func RestoreMatchFactsJSON(facts *HostFacts) string {
	if facts == nil {
		return ""
	}
	slim := struct {
		CPUIDHash         string `json:"cpuid_hash,omitempty"`
		HostKernelRelease string `json:"host_kernel_release,omitempty"`
	}{
		CPUIDHash:         strings.TrimSpace(facts.CPUIDHash),
		HostKernelRelease: strings.TrimSpace(facts.HostKernelRelease),
	}
	if slim.CPUIDHash == "" && slim.HostKernelRelease == "" {
		return ""
	}
	raw, err := json.Marshal(slim)
	if err != nil {
		return ""
	}
	return string(raw)
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
	InventoryIncomplete bool               `json:"inventory_incomplete,omitempty"`
	HostFacts           *HostFacts         `json:"host_facts,omitempty"`
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

	Versions            []ComponentVersion `json:"versions,omitempty"`
	InventoryIncomplete bool               `json:"inventory_incomplete,omitempty"`
	HostFacts           *HostFacts         `json:"host_facts,omitempty"`
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
	HostFacts           *HostFacts             `json:"host_facts,omitempty"`
	HeartbeatTime       time.Time              `json:"heartbeat_time,omitempty"`
	ReportedReady       bool                   `json:"-"`
	Healthy             bool                   `json:"healthy"`
	UnhealthyReason     string                 `json:"unhealthy_reason,omitempty"`
	// SchedulingDisabled is the API-facing cordon view derived from the
	// reserved label (or labels_json corruption). Not omitempty so enabled
	// (false) remains distinguishable from older servers.
	SchedulingDisabled bool `json:"scheduling_disabled"`
	// versionsHash is the content hash of Versions, used to skip redundant DB
	// writes on every heartbeat. Not serialised to JSON.
	versionsHash string
	// labelsJSONCorrupt marks that labels_json failed to parse; scheduling is
	// fail-closed. Not serialised.
	labelsJSONCorrupt bool
	// hostFactsDirty marks that the last persistHostFacts write failed, so the
	// in-memory facts are ahead of MySQL. Since host facts are static, the next
	// heartbeat carries the same value and hostFactsEqual would otherwise skip
	// the write forever; this flag forces a retry until the DB catches up. Not
	// serialised.
	hostFactsDirty bool
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

	// labelWriteLocks serialises the complete per-node lifecycle (register,
	// heartbeat, labels, isolation, deletion) so DB commit and in-memory/cache
	// publication stay ordered on a replica. Database row locks provide the
	// corresponding cross-replica ordering.
	labelWriteLocks sync.Map

	// hostFactsWriteLocks serialises the host-facts persist per node so
	// concurrent heartbeats cannot interleave and let an older facts blob's write
	// land after a newer one's (last-write-wins), permanently diverging MySQL from
	// the in-memory value. The persist re-reads the latest in-memory facts under
	// this lock, so the write always reflects the most recent heartbeat.
	hostFactsWriteLocks sync.Map
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
	if _, ok := req.Labels[constants.LabelSchedulingDisabled]; ok {
		log.G(ctx).Warnf("cubelet attempted to set scheduling-disabled label node_id=%s", req.NodeID)
		return nil, ErrSchedulingLabelRejected
	}
	// Merge the reported facts against the last-known ones the same way the
	// heartbeat path does, so a re-registration landing during a transient
	// /sys/module read gap (KVMModuleScanned=false, empty module state) cannot
	// wipe the persisted kvm_module_taint that the compatible-nodes aggregate and
	// the target-side gate read. Without this the register path would write
	// req.HostFacts verbatim and bypass the guard the heartbeat path relies on.
	mergedFacts := req.HostFacts
	if !req.HostFacts.IsZero() {
		global.mu.RLock()
		var prev *HostFacts
		if snap, ok := global.nodes[req.NodeID]; ok {
			prev = snap.HostFacts
		}
		global.mu.RUnlock()
		mergedFacts = mergeIncomingHostFacts(prev, req.HostFacts)
	}
	reg := &models.NodeRegistration{
		NodeID:              req.NodeID,
		HostIP:              req.HostIP,
		GRPCPort:            req.GRPCPort,
		CapacityJSON:        mustJSON(req.Capacity),
		AllocatableJSON:     mustJSON(req.Allocatable),
		InstanceType:        req.InstanceType,
		ClusterLabel:        req.ClusterLabel,
		QuotaCPU:            req.QuotaCPU,
		QuotaMemMB:          req.QuotaMemMB,
		CreateConcurrentNum: req.CreateConcurrentNum,
		MaxMvmNum:           req.MaxMvmNum,
		HostFactsJSON:       marshalHostFacts(mergedFacts),
	}
	applyHostFactColumns(reg, mergedFacts)
	// Read existing labels from DB, merge cubelet labels (cubelet wins on
	// conflict for user keys), preserve the control-plane scheduling-disabled
	// key, then write back. Use SELECT ... FOR UPDATE inside a transaction
	// under the per-node label write lock.
	unlock := global.lockNodeLabels(req.NodeID)
	defer unlock()

	updateColumns := []string{
		"host_ip", "grpc_port", "capacity_json", "allocatable_json",
		"instance_type", "cluster_label", "quota_cpu", "quota_mem_mb",
		"create_concurrent_num", "max_mvm_num", "updated_at",
	}
	// Only overwrite the host-fact columns (json blob + promoted keys) when this
	// report actually carries facts, so an older cubelet (nil HostFacts) cannot
	// wipe previously stored facts.
	if !req.HostFacts.IsZero() {
		updateColumns = append(updateColumns,
			"host_facts_json", "cpu_vendor", "cpu_model", "cpuid_hash", "host_kernel_release")
	}

	var mergedLabels map[string]string
	if err := global.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "node_id"}},
			DoUpdates: clause.AssignmentColumns(updateColumns),
		}).Create(reg).Error; err != nil {
			return err
		}
		existing, err := readLabelsJSONForUpdate(tx, req.NodeID)
		if err != nil {
			return err
		}
		existing = stripAndPreserveSchedulingLabel(existing, req.Labels)
		if countUserLabels(existing) > maxLabelsPerNode {
			return fmt.Errorf("a node cannot have more than %d labels, got %d after merge", maxLabelsPerNode, countUserLabels(existing))
		}
		if err := tx.Table(constants.NodeMetaRegistrationTable).
			Where("node_id = ?", req.NodeID).
			Update("labels_json", mustJSON(existing)).Error; err != nil {
			return err
		}
		mergedLabels = existing
		return nil
	}); err != nil {
		return nil, err
	}

	snap := global.ensureNode(req.NodeID)
	global.mu.Lock()
	snap.NodeID = req.NodeID
	snap.HostIP = req.HostIP
	snap.GRPCPort = req.GRPCPort
	snap.Labels = cloneStringMap(mergedLabels)
	snap.labelsJSONCorrupt = false
	snap.Capacity = req.Capacity
	snap.Allocatable = req.Allocatable
	snap.InstanceType = req.InstanceType
	snap.ClusterLabel = req.ClusterLabel
	snap.QuotaCPU = req.QuotaCPU
	snap.QuotaMemMB = req.QuotaMemMB
	snap.CreateConcurrentNum = req.CreateConcurrentNum
	snap.MaxMvmNum = req.MaxMvmNum
	if !mergedFacts.IsZero() {
		hf := *mergedFacts
		snap.HostFacts = &hf
	}
	applyCurrentHealth(snap, time.Now())
	global.mu.Unlock()
	syncLocalcache(snap)
	global.persistVersions(ctx, req.NodeID, req.Versions, req.InventoryIncomplete)
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
	// Serialize status writes with the complete per-node lifecycle. The
	// registration row lock prevents a heartbeat from recreating status after
	// deletion has committed; a heartbeat for a retired registration therefore
	// fails instead of leaving an orphan NodeStatus row.
	unlock := global.lockNodeLabels(nodeID)
	defer unlock()
	if err := global.db.Transaction(func(tx *gorm.DB) error {
		var reg models.NodeRegistration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("node_id = ?", nodeID).Take(&reg).Error; err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "node_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"conditions_json", "images_json", "local_templates_json",
				"heartbeat_unix", "healthy", "updated_at",
			}),
		}).Create(status).Error
	}); err != nil {
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
	hostFactsChanged := false
	if !req.HostFacts.IsZero() {
		merged := mergeIncomingHostFacts(snap.HostFacts, req.HostFacts)
		if !hostFactsEqual(snap.HostFacts, merged) {
			snap.HostFacts = merged
			hostFactsChanged = true
		}
	}
	// Retry a previously-failed persist even when the facts did not change:
	// static facts mean there is normally no next *changed* heartbeat to piggy
	// back on, so a one-off write failure would otherwise leave MySQL stale
	// until a master restart.
	persistFacts := hostFactsChanged || (snap.hostFactsDirty && !snap.HostFacts.IsZero())
	var factsToPersist *HostFacts
	if persistFacts && snap.HostFacts != nil {
		hf := *snap.HostFacts
		factsToPersist = &hf
	}
	applyCurrentHealth(snap, time.Now())
	global.mu.Unlock()
	syncLocalcache(snap)

	// Resource metrics flow via Redis (shared across cubemaster replicas)
	// and in-process cache (immediate visibility for this replica). They
	// are intentionally not persisted to MySQL: every 10s heartbeat from
	// hundreds of nodes would otherwise dominate write traffic, and Redis
	// already provides the cross-replica fan-out used by the scheduler.
	fanOutResourceMetric(ctx, nodeID, req)
	global.persistVersions(ctx, nodeID, req.Versions, req.InventoryIncomplete)
	// Host facts are static per boot; persist only when they actually change (or
	// to retry a prior failed write) so the 10s heartbeat does not turn them into
	// a MySQL write storm.
	if factsToPersist != nil {
		// Serialise the persist per node so two interleaved heartbeats cannot let an
		// older facts blob's write land after a newer one's. Under the lock, re-read
		// the current in-memory facts just before writing: whichever heartbeat holds
		// the lock last writes the most recent value, so MySQL always converges to
		// what memory holds instead of a stale last-writer-wins blob.
		unlock := global.lockHostFactsWrite(nodeID)
		global.mu.Lock()
		var latest *HostFacts
		if snap.HostFacts != nil {
			hf := *snap.HostFacts
			latest = &hf
		}
		// Mark dirty *before* issuing the write so a reload landing mid-write sees
		// the in-memory facts as pending and does not adopt the older DB value over
		// them. Cleared only once the write confirms the DB caught up.
		snap.hostFactsDirty = true
		global.mu.Unlock()
		var err error
		if latest != nil && !latest.IsZero() {
			err = global.persistHostFacts(ctx, nodeID, latest)
		}
		global.mu.Lock()
		snap.hostFactsDirty = err != nil
		global.mu.Unlock()
		unlock()
	}
	return cloneSnapshot(snap), nil
}

// persistHostFacts writes the node's host facts to the registration row. It
// returns the write error (nil on success) so the caller can mark the in-memory
// snapshot dirty and retry on a later heartbeat — host facts are static, so a
// dropped write has no next *changed* heartbeat to piggyback on.
func (s *service) persistHostFacts(ctx context.Context, nodeID string, facts *HostFacts) error {
	if facts.IsZero() {
		return nil
	}
	// Write the json blob and the promoted query columns together so the indexed
	// compatible-nodes lookup can never observe a row whose columns disagree with
	// its host_facts_json.
	res := s.db.WithContext(ctx).Table(constants.NodeMetaRegistrationTable).
		Where("node_id = ?", nodeID).
		Updates(map[string]interface{}{
			"host_facts_json":     marshalHostFacts(facts),
			"cpu_vendor":          facts.CPUVendor,
			"cpu_model":           facts.CPUModel,
			"cpuid_hash":          facts.CPUIDHash,
			"host_kernel_release": facts.HostKernelRelease,
		})
	if err := res.Error; err != nil {
		log.G(ctx).Errorf("persist host facts failed node_id=%s: %v", nodeID, err)
		return err
	}
	// Zero rows affected is ambiguous: it means "no row matched" only under the
	// go-sql-driver default (changed-rows semantics), where re-writing identical
	// values to an existing row also reports 0. Since the DSN does not set
	// clientFoundRows, a lagging replica writing values already in the DB would
	// otherwise be treated as a permanent failure and stay dirty forever. Confirm
	// the row is actually missing before reporting a retryable error; a
	// matched-but-unchanged write is a success.
	if res.RowsAffected == 0 {
		var count int64
		if err := s.db.WithContext(ctx).Table(constants.NodeMetaRegistrationTable).
			Where("node_id = ?", nodeID).Count(&count).Error; err != nil {
			log.G(ctx).Errorf("persist host facts existence check failed node_id=%s: %v", nodeID, err)
			return err
		}
		if count == 0 {
			log.G(ctx).Warnf("persist host facts affected 0 rows node_id=%s: registration row missing, will retry", nodeID)
			return fmt.Errorf("persist host facts: no registration row for node_id=%s", nodeID)
		}
	}
	return nil
}

// applyHostFactColumns denormalises the promoted query keys onto the
// registration row. Called wherever host_facts_json is set so the indexed
// columns stay in lockstep with the blob.
func applyHostFactColumns(reg *models.NodeRegistration, facts *HostFacts) {
	if facts.IsZero() {
		return
	}
	reg.CPUVendor = facts.CPUVendor
	reg.CPUModel = facts.CPUModel
	reg.CPUIDHash = facts.CPUIDHash
	reg.HostKernelRelease = facts.HostKernelRelease
}

func hostFactsEqual(a, b *HostFacts) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// mergeIncomingHostFacts folds a fresh heartbeat's facts onto the last-known
// snapshot, guarding the KVM module signals against a transient read failure on
// the node while still letting a genuine module unload clear a stale taint.
//
// The cubelet reports KVMModuleScanned=true whenever it successfully read
// /sys/module this cycle (even when no KVM module is loaded) and false only on a
// read failure. So an empty incoming module state has two meanings:
//   - Scanned=false → a read gap. Preserving the previous fingerprint+taint
//     avoids (a) churning the DB (empty differs from the stored value, so it
//     persists, then the next heartbeat restores and persists again) and (b)
//     silently disabling the absolute kvm_module_taint gate — and freezing a
//     clean-looking origin fingerprint onto any snapshot created in that window.
//   - Scanned=true → the module is authoritatively absent (kvm.ko unloaded); the
//     empty state is adopted so a once-observed taint can actually clear instead
//     of latching until reboot.
//
// A non-empty incoming module state is always adopted (a reload). Every other
// (boot-static) fact is taken from the incoming report as-is. KVMModuleScanned
// itself is a transient collection signal, so it is zeroed on the merged result
// and never stored or frozen onto a snapshot.
func mergeIncomingHostFacts(prev, incoming *HostFacts) *HostFacts {
	merged := *incoming
	if prev != nil && incoming.KVMModuleFingerprint == "" && incoming.KVMModuleTaint == "" &&
		!incoming.KVMModuleScanned {
		merged.KVMModuleFingerprint = prev.KVMModuleFingerprint
		merged.KVMModuleTaint = prev.KVMModuleTaint
	}
	merged.KVMModuleScanned = false
	return &merged
}

func marshalHostFacts(facts *HostFacts) string {
	if facts.IsZero() {
		return ""
	}
	return mustJSON(facts)
}

func unmarshalHostFacts(raw string) *HostFacts {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var facts HostFacts
	if err := json.Unmarshal([]byte(raw), &facts); err != nil {
		return nil
	}
	if facts.IsZero() {
		return nil
	}
	return &facts
}

// incompleteVersionsHashTag is appended to the content hash when the report
// is inventory-incomplete, so a later complete report with the same merge
// result still triggers a write (and can hard-delete stale rows).
const incompleteVersionsHashTag = "|incomplete"

// persistVersions records the node's component versions, skipping the DB
// write entirely when the reported set is unchanged (content-hash compare
// against the in-memory snapshot). This keeps the 10s heartbeat from turning
// slow-changing version data into a MySQL write storm.
//
// When inventoryIncomplete is true, missing components are not deleted from
// the DB (upsert-only) so a transient collection gap cannot wipe history.
// The skip-hash then compares against merge(prev, reported) so incomplete
// heartbeats that do not change the effective inventory are no-ops.
func (s *service) persistVersions(ctx context.Context, nodeID string, versions []ComponentVersion, inventoryIncomplete bool) {
	s.persistVersionsWithWriter(ctx, nodeID, versions, inventoryIncomplete, s.writeVersions)
}

func (s *service) persistVersionsWithWriter(
	ctx context.Context,
	nodeID string,
	versions []ComponentVersion,
	inventoryIncomplete bool,
	writer func(string, []ComponentVersion, bool) error,
) {
	if len(versions) == 0 {
		return
	}
	unlock := s.lockVersionWrite(nodeID)
	defer unlock()
	snap := s.ensureNode(nodeID)
	s.mu.RLock()
	prevVersions := append([]ComponentVersion(nil), snap.Versions...)
	prevHash := snap.versionsHash
	prevCompat := compatRelevantVersions(snap.Versions)
	s.mu.RUnlock()

	// Complete reports hash the payload as-is. Incomplete reports hash the
	// merge with the previous snapshot so a partial collect does not look
	// like a wholesale version change (and does not delete absent keys).
	var h string
	var merged []ComponentVersion
	if inventoryIncomplete {
		merged = mergeComponentVersions(prevVersions, versions)
		h = versionsHash(merged) + incompleteVersionsHashTag
	} else {
		h = versionsHash(versions)
	}
	if prevHash == h {
		log.G(ctx).Debugf("version_write_skipped node=%s", nodeID)
		return
	}
	if err := writer(nodeID, versions, inventoryIncomplete); err != nil {
		log.G(ctx).Warnf("write node component versions failed for %s: %v", nodeID, err)
		return
	}
	s.mu.Lock()
	if inventoryIncomplete {
		snap.Versions = merged
		snap.versionsHash = h
	} else {
		snap.Versions = append([]ComponentVersion(nil), versions...)
		snap.versionsHash = h
	}
	s.mu.Unlock()
	log.G(ctx).Debugf("version_write_applied node=%s components=%d incomplete=%v", nodeID, len(versions), inventoryIncomplete)
	if OnGuestAgentVersionChanged != nil && compatVersionsChanged(prevCompat, compatRelevantVersions(snap.Versions)) {
		go OnGuestAgentVersionChanged(nodeID)
	}
}

func mergeComponentVersions(prev, next []ComponentVersion) []ComponentVersion {
	byName := make(map[string]ComponentVersion, len(prev)+len(next))
	for _, v := range prev {
		if v.Component == "" {
			continue
		}
		byName[v.Component] = v
	}
	for _, v := range next {
		if v.Component == "" {
			continue
		}
		byName[v.Component] = v
	}
	out := make([]ComponentVersion, 0, len(byName))
	for _, v := range byName {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Component < out[j].Component })
	return out
}

func (s *service) lockVersionWrite(nodeID string) func() {
	lockAny, _ := s.versionWriteLocks.LoadOrStore(nodeID, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func (s *service) lockHostFactsWrite(nodeID string) func() {
	lockAny, _ := s.hostFactsWriteLocks.LoadOrStore(nodeID, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

// writeVersions upserts the reported component rows. When inventoryIncomplete
// is false, physically removes any component previously recorded for the node
// but absent from this report. When true, only upserts (plan M0).
func (s *service) writeVersions(nodeID string, versions []ComponentVersion, inventoryIncomplete bool) error {
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
		if inventoryIncomplete {
			return nil
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
		fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s\n", v.Component, v.Version, v.Commit, v.BuildTime, v.Source, v.Variant)
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

// GetNodeHostFacts returns the trusted host facts (CPU feature set, host
// kernel, KVM ABI) for a healthy node. The boolean is false when the node is
// unknown, unhealthy, its heartbeat has expired, or no host facts were ever
// reported; callers should treat that as UNKNOWN rather than reusing stale
// values. Mirrors GetNodeComponentVersions gating semantics.
func GetNodeHostFacts(ctx context.Context, nodeID string) (*HostFacts, bool) {
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
	if !cloned.Healthy || cloned.HostFacts.IsZero() {
		return nil, false
	}
	return cloned.HostFacts, true
}

// GetPersistedNodeHostFacts reads the node's last-persisted host facts straight
// from the registration row, ignoring live health. It exists as a backfill for
// snapshot create: GetNodeHostFacts fails closed on a momentary heartbeat
// expiry, and freezing that empty result onto a snapshot would permanently
// degrade it to origin_fingerprint_unknown even though the node's real facts are
// still on disk. Host facts are boot-static, so the persisted value is a safe
// stand-in when the live node is transiently unhealthy. Returns false when no
// row exists or no facts were ever persisted.
func GetPersistedNodeHostFacts(ctx context.Context, nodeID string) (*HostFacts, bool) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return nil, false
	}
	if global.db == nil {
		return nil, false
	}
	var row struct {
		HostFactsJSON string
	}
	err := global.db.WithContext(ctx).
		Table(constants.NodeMetaRegistrationTable).
		Select("host_facts_json").
		Where("node_id = ?", nodeID).
		Scan(&row).Error
	if err != nil {
		return nil, false
	}
	facts := unmarshalHostFacts(row.HostFactsJSON)
	if facts.IsZero() {
		return nil, false
	}
	return facts, true
}

// CandidateNode is one healthy node's full host facts, returned by the
// host-fact candidate query. HostFacts is decoded from host_facts_json so the
// caller can apply the taint gate and build the per-node dimensions.
type CandidateNode struct {
	NodeID    string
	HostIP    string
	HostFacts *HostFacts
}

// QueryHostFactCandidates returns the healthy nodes to evaluate for restore
// compatibility, pushing the two required (blocking) equality keys down to the
// database so the common path is a single indexed SELECT instead of a full
// in-memory scan + JSON decode of every node.
//
// It joins t_cube_node_registration against t_cube_node_status and keeps only
// rows whose heartbeat is within the health timeout (the same freshness rule the
// in-memory view applies), so a stale-heartbeat node is never offered as a
// restore target.
//
// The two required keys are equality-filtered in SQL. Everything else — the
// absolute kvm_module_taint gate and the informational dimensions — stays in
// host_facts_json and is applied in-app by the caller, because the taint gate is
// not an origin==target comparison and cannot be expressed as a column predicate.
//
// When matchAll is true the required-key predicate is dropped and every healthy
// node with facts is returned, so a diagnostic caller (include_all) can see why
// each node fails; the caller still runs the full judgment per node.
//
// ARM-safe: the required keys are cpuid_hash and host_kernel_release, both
// populated on aarch64 (cpu_vendor/cpu_model are empty there and are never part
// of the predicate).
func QueryHostFactCandidates(ctx context.Context, requiredCPUIDHash, requiredKernelRelease string, matchAll bool) ([]*CandidateNode, error) {
	cutoff := time.Now().Add(-healthTimeout()).Unix()
	q := global.db.WithContext(ctx).
		Table(constants.NodeMetaRegistrationTable+" AS reg").
		Select("reg.node_id AS node_id, reg.host_ip AS host_ip, reg.host_facts_json AS host_facts_json").
		Joins("JOIN "+constants.NodeMetaStatusTable+" AS st ON st.node_id = reg.node_id").
		Where("st.healthy = ?", true).
		Where("st.heartbeat_unix >= ?", cutoff).
		Where("reg.host_facts_json <> ''")
	if !matchAll {
		q = q.Where("reg.cpuid_hash = ?", requiredCPUIDHash).
			Where("reg.host_kernel_release = ?", requiredKernelRelease)
	}
	var rows []struct {
		NodeID        string
		HostIP        string
		HostFactsJSON string
	}
	if err := q.Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*CandidateNode, 0, len(rows))
	for _, r := range rows {
		facts := unmarshalHostFacts(r.HostFactsJSON)
		if facts == nil {
			continue
		}
		out = append(out, &CandidateNode{NodeID: r.NodeID, HostIP: r.HostIP, HostFacts: facts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
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
		return nil, fmt.Errorf("%w: %s", ErrNodeNotFound, nodeID)
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

type UpdateNodeLabelsRequest struct {
	Labels map[string]string `json:"labels"`
}

func UpdateNodeLabels(ctx context.Context, nodeID string, labels map[string]string) error {
	if nodeID == "" {
		return fmt.Errorf("node_id is required")
	}
	if err := validateNodeLabels(labels); err != nil {
		return err
	}
	unlock := global.lockNodeLabels(nodeID)
	defer unlock()

	var nodeLabels map[string]string
	if err := global.db.Transaction(func(tx *gorm.DB) error {
		existing, err := readLabelsJSONForUpdate(tx, nodeID)
		if err != nil {
			return err
		}
		for k, v := range labels {
			existing[k] = v
		}
		if countUserLabels(existing) > maxLabelsPerNode {
			return fmt.Errorf("a node cannot have more than %d labels, got %d after merge", maxLabelsPerNode, countUserLabels(existing))
		}
		if err := tx.Table(constants.NodeMetaRegistrationTable).
			Where("node_id = ?", nodeID).
			Updates(map[string]interface{}{
				"labels_json": mustJSON(existing),
				"updated_at":  time.Now(),
			}).Error; err != nil {
			return err
		}
		nodeLabels = existing
		return nil
	}); err != nil {
		return err
	}

	snap := global.ensureNode(nodeID)
	global.mu.Lock()
	snap.Labels = cloneStringMap(nodeLabels)
	snap.labelsJSONCorrupt = false
	global.mu.Unlock()
	syncLocalcache(snap)
	return nil
}

func DeleteNodeLabel(ctx context.Context, nodeID, key string) error {
	if nodeID == "" {
		return fmt.Errorf("node_id is required")
	}
	if err := validateNodeLabelKey(key); err != nil {
		return err
	}
	unlock := global.lockNodeLabels(nodeID)
	defer unlock()

	var nodeLabels map[string]string
	if err := global.db.Transaction(func(tx *gorm.DB) error {
		existing, err := readLabelsJSONForUpdate(tx, nodeID)
		if err != nil {
			return err
		}
		delete(existing, key)
		if err := tx.Table(constants.NodeMetaRegistrationTable).
			Where("node_id = ?", nodeID).
			Updates(map[string]interface{}{
				"labels_json": mustJSON(existing),
				"updated_at":  time.Now(),
			}).Error; err != nil {
			return err
		}
		nodeLabels = existing
		return nil
	}); err != nil {
		return err
	}
	snap := global.ensureNode(nodeID)
	global.mu.Lock()
	snap.Labels = cloneStringMap(nodeLabels)
	snap.labelsJSONCorrupt = false
	global.mu.Unlock()
	syncLocalcache(snap)
	return nil
}

// readLabelsJSONForUpdate reads labels_json with SELECT ... FOR UPDATE.
// Corrupt JSON returns ErrLabelsJSONCorrupt (fail-closed for all label writers:
// register merge, admin labels, and isolation).
func readLabelsJSONForUpdate(tx *gorm.DB, nodeID string) (map[string]string, error) {
	var reg models.NodeRegistration
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Table(constants.NodeMetaRegistrationTable).
		Where("node_id = ?", nodeID).
		Take(&reg).Error; err != nil {
		return nil, err
	}
	labels, err := parseLabelsJSON(reg.LabelsJSON)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLabelsJSONCorrupt, err)
	}
	return labels, nil
}

func parseLabelsJSON(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}, nil
	}
	m := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	if m == nil {
		return map[string]string{}, nil
	}
	return m, nil
}

// Label validation follows Kubernetes conventions.
// See: k8s.io/apimachinery/pkg/util/validation, k8s.io/apimachinery/pkg/api/validate/content
//
// Key format:   [prefix/]name
//   - prefix: optional, DNS1123 subdomain (lowercase alphanumeric, '-' or '.', max 253)
//   - name:   required, qualified name (alphanumeric, '-' '_' or '.', max 63, must start/end with alphanumeric)
//
// Value format: empty string or qualified name (same constraints as name, max 63)

const (
	qualifiedNameMaxLength    = 63
	dns1123SubdomainMaxLength = 253

	// Matches a qualified name: alphanumeric, '-', '_', '.', must start and end with alphanumeric.
	qualifiedNameFmt = `([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]`

	qualifiedNameErrMsg = `a qualified name must consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character`

	// Matches a DNS1123 subdomain: lowercase alphanumeric, '-' or '.', segments separated by '.'.
	dns1123SubdomainFmt = `[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*`

	dns1123SubdomainErrMsg = `a DNS-1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character`

	maxLabelsPerNode = 64
)

var (
	qualifiedNameRegexp    = regexp.MustCompile(`^` + qualifiedNameFmt + `$`)
	dns1123SubdomainRegexp = regexp.MustCompile(`^` + dns1123SubdomainFmt + `$`)
)

func validateNodeLabels(labels map[string]string) error {
	if len(labels) > maxLabelsPerNode {
		return fmt.Errorf("label update request cannot contain more than %d labels, got %d", maxLabelsPerNode, len(labels))
	}
	for k, v := range labels {
		if err := validateNodeLabelKey(k); err != nil {
			return err
		}
		if errs := isValidLabelValue(v); len(errs) != 0 {
			return fmt.Errorf("label value for key %q is invalid: %s", k, strings.Join(errs, ", "))
		}
	}
	return nil
}

func validateNodeLabelKey(key string) error {
	if errs := isQualifiedLabelKey(key); len(errs) != 0 {
		return fmt.Errorf("label key %q is invalid: %s", key, strings.Join(errs, ", "))
	}
	return nil
}

// isQualifiedLabelKey validates a label key, matching K8s IsQualifiedName logic.
// Returns a list of error strings if invalid, empty list if valid.
func isQualifiedLabelKey(key string) []string {
	var errs []string

	if key == "" {
		return append(errs, "must not be empty")
	}
	if config.IsReservedLabelKey(key) {
		return append(errs, "is reserved for system use")
	}

	parts := strings.Split(key, "/")
	var name string
	switch len(parts) {
	case 1:
		name = parts[0]
	case 2:
		prefix := parts[0]
		name = parts[1]
		if prefix == "" {
			errs = append(errs, "prefix part must not be empty")
		} else if len(prefix) > dns1123SubdomainMaxLength {
			errs = append(errs, fmt.Sprintf("prefix part must be no more than %d characters", dns1123SubdomainMaxLength))
		} else if !dns1123SubdomainRegexp.MatchString(prefix) {
			errs = append(errs, "prefix part "+dns1123SubdomainErrMsg)
		}
	default:
		return append(errs, "must be in the form prefix/name or name (e.g. 'MyName' or 'example.com/MyName')")
	}

	if name == "" {
		errs = append(errs, "name part must not be empty")
	} else if len(name) > qualifiedNameMaxLength {
		errs = append(errs, fmt.Sprintf("name part must be no more than %d characters", qualifiedNameMaxLength))
	} else if !qualifiedNameRegexp.MatchString(name) {
		errs = append(errs, "name part "+qualifiedNameErrMsg)
	}

	return errs
}

// isValidLabelValue validates a label value, matching K8s IsValidLabelValue logic.
// Returns a list of error strings if invalid, empty list if valid.
func isValidLabelValue(value string) []string {
	var errs []string
	if value == "" {
		return errs
	}
	if len(value) > qualifiedNameMaxLength {
		errs = append(errs, fmt.Sprintf("must be no more than %d characters", qualifiedNameMaxLength))
	}
	if !qualifiedNameRegexp.MatchString(value) {
		errs = append(errs, qualifiedNameErrMsg)
	}
	return errs
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
		labels, err := parseLabelsJSON(reg.LabelsJSON)
		if err != nil {
			log.G(context.Background()).Warnf("node labels_json corrupt node_id=%s err=%v; scheduling fail-closed", reg.NodeID, err)
			snap.Labels = map[string]string{}
			snap.labelsJSONCorrupt = true
		} else {
			snap.Labels = labels
		}
		_ = json.Unmarshal([]byte(reg.CapacityJSON), &snap.Capacity)
		_ = json.Unmarshal([]byte(reg.AllocatableJSON), &snap.Allocatable)
		snap.HostFacts = unmarshalHostFacts(reg.HostFactsJSON)
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

// applyReloadResult merges a DB snapshot (next) into the live in-memory map and
// then re-syncs node health into localcache for every node the reload touched.
//
// Registration fields and versions always take the DB value; status/heartbeat
// fields keep the in-memory value when it is fresher than the DB snapshot.
//
// The re-sync pushes ONLY the scheduler node cache (health, capacity, cordon
// state); see syncLocalcacheNodeHealth. Template locality is deliberately left
// to templatecenter, the authoritative owner of the imageCache (startup
// warmReadyTemplateLocality + on-demand EnsureTemplateLocalityReady DB fallback).
//
// Why node health must be re-synced here: a replica that only learned a node via
// DB reload (it registered/heartbeated on another replica) otherwise kept an
// empty healthy-node set for it. EnsureTemplateLocalityReady matches a DB-Ready
// template replica against localcache's healthy nodes, so with the node absent
// it could not match and sandbox creation failed with "template has no ready
// replica" (130400). Pushing node health here lets that DB fallback match and
// self-heal template locality on demand.
//
// Known race: next is a point-in-time DB snapshot. If node deletion commits
// after reload reads that snapshot but before it is merged here, the stale
// snapshot can briefly reinsert the deleted node into s.nodes and localcache.
// The node was required to be isolated before deletion, so the stale snapshot
// remains scheduling-disabled and does not admit new sandboxes; a subsequent
// reload removes it after observing the missing registration. We currently
// accept this short-lived visibility inconsistency instead of adding deletion
// tombstones or per-node snapshot generations.
func (s *service) applyReloadResult(next map[string]*NodeSnapshot) {
	syncSnaps, evicted := s.mergeReloadResult(next)

	// s.ready is set true at the end of Init, after the initial reload and
	// before localcache.Init. The very first reload therefore skips the sync
	// (localcache caches do not exist yet); localcache.Init then loads all nodes
	// from the DB itself. Periodic loopReload runs long after both packages are
	// initialised.
	if !s.ready {
		return
	}
	// Sync outside s.mu (localcache has its own locks).
	for _, snap := range syncSnaps {
		syncNodeHealthFn(snap)
	}
	for _, nodeID := range evicted {
		func() {
			// RegisterNode holds the same lifecycle lock until its in-memory and
			// localcache publications finish. If a registration raced with this
			// stale DB snapshot, wait for it and do not evict the new registration.
			unlock := s.lockNodeLabels(nodeID)
			defer unlock()
			s.mu.RLock()
			_, registered := s.nodes[nodeID]
			s.mu.RUnlock()
			if !registered {
				evictNodeFn(nodeID)
			}
		}()
	}
}

// mergeReloadResult merges the DB reload snapshot into the in-memory node map
// under s.mu and returns clones of touched nodes plus IDs removed by the
// snapshot. Computing both results in the same critical section prevents a
// concurrent registration from being classified using an earlier view.
// The caller syncs localcache outside the lock. The critical section uses
// defer s.mu.Unlock() so a panic in the merge loop cannot leak the lock and
// silently deadlock subsequent register/heartbeat handlers.
func (s *service) mergeReloadResult(next map[string]*NodeSnapshot) ([]*NodeSnapshot, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	evicted := make([]string, 0)
	for nodeID := range s.nodes {
		if _, stillPresent := next[nodeID]; !stillPresent {
			delete(s.nodes, nodeID)
			evicted = append(evicted, nodeID)
		}
	}
	syncSnaps := make([]*NodeSnapshot, 0, len(next))
	for nodeID, newSnap := range next {
		if existing, ok := s.nodes[nodeID]; ok {
			existing.Labels = cloneStringMap(newSnap.Labels)
			existing.labelsJSONCorrupt = newSnap.labelsJSONCorrupt
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
			// Host facts: adopt the DB value when it is fresher than what this
			// replica holds (another replica may have persisted updated facts
			// under a newer heartbeat), or when memory has none. A stale DB read
			// never clobbers a fresher in-memory value written by a heartbeat on
			// this replica. Keying on the same heartbeat freshness as the block
			// below prevents a replica that learned the node via an early reload
			// from serving indefinitely-stale facts to restore-compat.
			//
			// hostFactsDirty guards against the status/persist clock skew: the
			// status HeartbeatTime advances on every heartbeat, but host facts are
			// only written to the DB when they change (and the write may fail). If
			// this replica holds facts pending a (re)persist, the DB is known to
			// carry the *older* facts even though its status heartbeat may be newer
			// — adopting it would revert the pending facts (e.g. a fresh
			// KVMModuleTaint), so the dirty in-memory value always wins.
			if newSnap.HostFacts != nil && !existing.hostFactsDirty &&
				(existing.HostFacts == nil || newSnap.HeartbeatTime.After(existing.HeartbeatTime)) {
				hf := *newSnap.HostFacts
				existing.HostFacts = &hf
			}
			if newSnap.HeartbeatTime.After(existing.HeartbeatTime) {
				existing.Conditions = newSnap.Conditions
				existing.Images = newSnap.Images
				existing.LocalTemplates = newSnap.LocalTemplates
				existing.HeartbeatTime = newSnap.HeartbeatTime
				existing.ReportedReady = newSnap.ReportedReady
			}
			applyCurrentHealth(existing, time.Now())
			syncSnaps = append(syncSnaps, cloneSnapshot(existing))
		} else {
			s.nodes[nodeID] = newSnap
			syncSnaps = append(syncSnaps, cloneSnapshot(newSnap))
		}
	}
	return syncSnaps, evicted
}

// evictNodeFn is a test seam for the reload -> localcache eviction path.
var evictNodeFn = localcache.EvictNode

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
	n := &node.Node{
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
	if snap.HostFacts != nil {
		n.HostFacts = &node.HostFacts{
			CPUVendor:             snap.HostFacts.CPUVendor,
			CPUModel:              snap.HostFacts.CPUModel,
			CPUIDHash:             snap.HostFacts.CPUIDHash,
			HostKernelRelease:     snap.HostFacts.HostKernelRelease,
			HostKernelFingerprint: snap.HostFacts.HostKernelFingerprint,
			KVMAPIVersion:         snap.HostFacts.KVMAPIVersion,
			KVMModuleFingerprint:  snap.HostFacts.KVMModuleFingerprint,
			KVMModuleTaint:        snap.HostFacts.KVMModuleTaint,
		}
	}
	n.SetSchedulingDisabled(snapshotSchedulingDisabled(snap))
	return n
}

func syncLocalcache(snap *NodeSnapshot) {
	localcache.UpsertNode(toSchedulerNode(snap))
	localcache.SyncNodeTemplates(snap.NodeID, templateIDsFromLocalTemplates(snap.LocalTemplates))
}

// syncNodeHealthFn is the reload -> localcache sync hook invoked by
// applyReloadResult for every touched node. It is a package var (not a direct
// call to syncLocalcacheNodeHealth) so unit tests can observe the sync path
// without initialising the global localcache singleton.
var syncNodeHealthFn = syncLocalcacheNodeHealth

// syncLocalcacheNodeHealth pushes only the scheduler node cache (health,
// capacity, cordon state) into localcache. Unlike syncLocalcache it deliberately
// does NOT call SyncNodeTemplates: template locality lives in the imageCache,
// which templatecenter owns authoritatively (warmReadyTemplateLocality at
// startup + EnsureTemplateLocalityReady's DB fallback on demand). The periodic
// reload only needs the node itself to be present/healthy so that DB fallback
// can match a ready template replica to it; having reload also write the
// imageCache would race that authoritative owner.
func syncLocalcacheNodeHealth(snap *NodeSnapshot) {
	localcache.UpsertNode(toSchedulerNode(snap))
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
	if in.HostFacts != nil {
		hf := *in.HostFacts
		out.HostFacts = &hf
	}
	out.SchedulingDisabled = snapshotSchedulingDisabled(in)
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
