// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/nodemetric"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
)

func buildSnapshotFromStore(reg *store.NodeRegistration, st *store.NodeStatus, versions []store.NodeComponentVersion) *model.NodeSnapshot {
	snap := &model.NodeSnapshot{
		NodeID:              reg.NodeID,
		HostIP:              reg.HostIP,
		GRPCPort:            reg.GRPCPort,
		Labels:              map[string]string{},
		Capacity:            model.ResourceSnapshot{},
		Allocatable:         model.ResourceSnapshot{},
		InstanceType:        reg.InstanceType,
		ClusterLabel:        reg.ClusterLabel,
		QuotaCPU:            reg.QuotaCPU,
		QuotaMemMB:          reg.QuotaMemMB,
		CreateConcurrentNum: reg.CreateConcurrentNum,
		MaxMvmNum:           reg.MaxMvmNum,
	}
	labels, err := store.ParseLabelsJSON(reg.LabelsJSON)
	if err != nil {
		snap.Labels = map[string]string{}
		snap.LabelsJSONCorrupt = true
	} else {
		snap.Labels = labels
	}
	_ = json.Unmarshal([]byte(reg.CapacityJSON), &snap.Capacity)
	_ = json.Unmarshal([]byte(reg.AllocatableJSON), &snap.Allocatable)

	// Restore HostFacts from the registration row.
	if reg.HostFactsJSON != "" {
		snap.HostFacts = unmarshalHostFacts(reg.HostFactsJSON)
	}

	if st != nil {
		_ = json.Unmarshal([]byte(st.ConditionsJSON), &snap.Conditions)
		_ = json.Unmarshal([]byte(st.ImagesJSON), &snap.Images)
		_ = json.Unmarshal([]byte(st.LocalTemplatesJSON), &snap.LocalTemplates)
		snap.HeartbeatTime = time.Unix(st.HeartbeatUnix, 0)
		snap.ReportedReady = st.Healthy
	}

	for _, v := range versions {
		snap.Versions = append(snap.Versions, model.ComponentVersion{
			Component: v.Component,
			Version:   v.Version,
			Commit:    v.Commit,
			BuildTime: v.BuildTime,
			Source:    v.Source,
		})
	}
	snap.VersionsHash = VersionsHash(snap.Versions)
	restoreMetricFromRedis(snap)
	applyCurrentHealth(snap, time.Now())
	snap.SchedulingDisabled = snapSchedulingDisabled(snap)
	return snap
}

// restoreMetricFromRedis overlays real-time metric from the Redis metric hash
// on a DB-rebuilt snapshot, so a rebuild cannot overwrite the live metrics.
func restoreMetricFromRedis(snap *model.NodeSnapshot) {
	if snap == nil || snap.NodeID == "" {
		return
	}
	m, err := nodemetric.ReadNodeMetric(snap.NodeID)
	if err != nil {
		logging.G(context.Background()).Warnf("nodemgmt: restore metric from redis failed: node=%s: %v", snap.NodeID, err)
		return
	}
	if m == nil {
		return // metric miss; leave snapshot metrics at zero
	}
	snap.MetricUpdate = m.MetricTime
	if m.HasAllocated {
		snap.QuotaCpuUsage = m.MilliCPUUsage
		snap.QuotaMemUsage = m.MemoryMBUsage
		snap.MvmNum = m.MvmNum
		snap.NicQueues = m.NicQueues
	}
	if m.HasDisk {
		snap.DataDiskUsagePer = m.DataDiskUsagePer
		snap.StorageDiskUsagePer = m.StorageDiskUsagePer
		snap.SysDiskUsagePer = m.SysDiskUsagePer
	}
}

func cloneSnapshotWithCurrentHealth(in *model.NodeSnapshot) *model.NodeSnapshot {
	return cloneSnapshotWithCurrentHealthAt(in, time.Now())
}

func cloneSnapshotWithCurrentHealthAt(in *model.NodeSnapshot, now time.Time) *model.NodeSnapshot {
	out := cloneSnapshot(in)
	applyCurrentHealth(out, now)
	out.SchedulingDisabled = snapSchedulingDisabled(out)
	return out
}

func applyCurrentHealth(snap *model.NodeSnapshot, now time.Time) {
	if snap == nil {
		return
	}
	h := EvaluateHealth(snap.ReportedReady, snap.HeartbeatTime, now, MetadataTimeout())
	snap.Healthy = h.Healthy
	snap.UnhealthyReason = h.UnhealthyReason
}

func snapSchedulingDisabled(snap *model.NodeSnapshot) bool {
	if snap == nil || snap.LabelsJSONCorrupt {
		return true
	}
	return DecodeSchedulingDisabled(snap.Labels)
}

func cloneSnapshot(in *model.NodeSnapshot) *model.NodeSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	out.Labels = cloneStringMap(in.Labels)
	out.Conditions = append([]model.NodeCondition(nil), in.Conditions...)
	out.Images = append([]model.ContainerImage(nil), in.Images...)
	out.LocalTemplates = append([]model.LocalTemplate(nil), in.LocalTemplates...)
	out.Versions = append([]model.ComponentVersion(nil), in.Versions...)
	out.VirtualNodeQuotaArray = append([]int64(nil), in.VirtualNodeQuotaArray...)
	if in.HostFacts != nil {
		hf := *in.HostFacts
		out.HostFacts = &hf
	}
	// Re-derive from Labels to guard against upstream only updating Labels
	// without syncing the cached SchedulingDisabled field.
	out.SchedulingDisabled = snapSchedulingDisabled(in)
	return &out
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

func sortSnapshots(snaps []*model.NodeSnapshot) {
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].NodeID < snaps[j].NodeID })
}

// applyHostMetaToSnapshot overlays the legacy static scheduler fields from the
// host registry, mirroring CubeMaster's localcache.db_cache join.
func applyHostMetaToSnapshot(
	snap *model.NodeSnapshot,
	hosts map[string]*store.HostInfo,
	hostTypes map[string]*store.HostTypeInfo,
	subHosts map[string]*store.SubHostInfo,
) {
	if snap == nil {
		return
	}
	if h, ok := hosts[snap.NodeID]; ok {
		snap.Zone = h.Zone
		snap.Region = h.Region
		snap.SystemDiskSize = h.SysDiskGB
		snap.DataDiskSize = h.DataDiskGB
		if snap.InstanceType == "" {
			snap.InstanceType = h.InstanceType
		}
		if snap.ClusterLabel == "" {
			snap.ClusterLabel = h.ClusterLabel
		}
		if snap.QuotaCPU == 0 {
			snap.QuotaCPU = h.QuotaCPU
		}
		if snap.QuotaMemMB == 0 {
			snap.QuotaMemMB = h.QuotaMemMB
		}
	}
	if t, ok := hostTypes[snap.InstanceType]; ok {
		snap.CPUType = t.CPUType
	}
	if m, ok := subHosts[snap.NodeID]; ok {
		snap.DeviceClass = m.DeviceClass
		snap.DeviceID = m.DeviceID
		snap.MachineHostIP = m.HostIP
		snap.InstanceFamily = m.InstanceFamily
		snap.DedicatedClusterID = m.DedicatedClusterID
		if m.VirtualNodeQuota != "" && m.VirtualNodeQuota != "[]" {
			var arr []int64
			if err := json.Unmarshal([]byte(m.VirtualNodeQuota), &arr); err == nil {
				snap.VirtualNodeQuotaArray = arr
			}
		}
	}
}

func ToSchedulerNode(snap *model.NodeSnapshot) *model.SchedulerNode {
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
		instanceType = model.DefaultInstanceTypeName
	}
	localTemplates := make([]string, 0, len(snap.LocalTemplates))
	for _, t := range snap.LocalTemplates {
		if t.TemplateID != "" {
			localTemplates = append(localTemplates, t.TemplateID)
		}
	}
	return &model.SchedulerNode{
		InsID:                 snap.NodeID,
		UUID:                  snap.NodeID,
		IP:                    hostIP,
		CpuTotal:              int(snap.Capacity.MilliCPU / 1000),
		MemMBTotal:            snap.Capacity.MemoryMB,
		SystemDiskSize:        snap.SystemDiskSize,
		DataDiskSize:          snap.DataDiskSize,
		Zone:                  snap.Zone,
		Region:                snap.Region,
		CPUType:               snap.CPUType,
		DeviceClass:           snap.DeviceClass,
		DeviceID:              snap.DeviceID,
		MachineHostIP:         snap.MachineHostIP,
		InstanceFamily:        snap.InstanceFamily,
		DedicatedClusterId:    snap.DedicatedClusterID,
		VirtualNodeQuotaArray: append([]int64(nil), snap.VirtualNodeQuotaArray...),
		ClusterLabel:          snap.ClusterLabel,
		OssClusterLabel:       snap.ClusterLabel,
		InstanceType:          instanceType,
		HostStatus:            model.HostStatusRunning,
		ReportedReady:         snap.ReportedReady,
		Healthy:               snap.Healthy,
		UnhealthyReason:       snap.UnhealthyReason,
		CreateConcurrentNum:   snap.CreateConcurrentNum,
		MaxMvmLimit:           snap.MaxMvmNum,
		MetaDataUpdateAt:      snap.HeartbeatTime,
		MetricUpdate:          snap.MetricUpdate,
		MetricLocalUpdateAt:   snap.MetricLocalUpdateAt,
		QuotaCpuUsage:         snap.QuotaCpuUsage,
		QuotaMemUsage:         snap.QuotaMemUsage,
		MvmNum:                snap.MvmNum,
		DataDiskUsagePer:      snap.DataDiskUsagePer,
		StorageDiskUsagePer:   snap.StorageDiskUsagePer,
		SysDiskUsagePer:       snap.SysDiskUsagePer,
		NicQueues:             snap.NicQueues,
		NodeLabels:            cloneStringMap(snap.Labels),
		SchedulingDisabled:    snapSchedulingDisabled(snap),
		LocalTemplates:        localTemplates,
		Versions:              snap.Versions,
		HostFacts:             cloneHostFacts(snap.HostFacts),
	}
}

func SchedulerNodeScoreView(snap *model.NodeSnapshot) *model.SchedulerNode {
	return &model.SchedulerNode{
		InsID:               snap.NodeID,
		Score:               snap.Score,
		MetricUpdate:        snap.MetricUpdate,
		MetricLocalUpdateAt: snap.MetricLocalUpdateAt,
		MetaDataUpdateAt:    snap.HeartbeatTime,
	}
}

// cloneHostFacts returns a deep copy of f (nil-safe).
func cloneHostFacts(f *model.HostFacts) *model.HostFacts {
	if f == nil {
		return nil
	}
	hf := *f
	return &hf
}

// marshalHostFacts serialises HostFacts to JSON for DB storage. The transient
// KVMModuleScanned signal is cleared so it never reaches MySQL.
func marshalHostFacts(f *model.HostFacts) string {
	if f == nil || f.IsZero() {
		return ""
	}
	cp := *f
	cp.KVMModuleScanned = false
	return model.MustJSON(&cp)
}

// unmarshalHostFacts deserialises HostFacts from JSON.
func unmarshalHostFacts(raw string) *model.HostFacts {
	if raw == "" {
		return nil
	}
	f := &model.HostFacts{}
	if err := json.Unmarshal([]byte(raw), f); err != nil {
		return nil
	}
	if f.IsZero() {
		return nil
	}
	return f
}

// applyHostFactsToRegistration fills the HostFactsJSON and redundant columns
// (cpuid_hash, host_kernel_release) on a NodeRegistration for DB upsert.
func applyHostFactsToRegistration(reg *store.NodeRegistration, f *model.HostFacts) {
	if reg == nil {
		return
	}
	if f == nil || f.IsZero() {
		reg.HostFactsJSON = ""
		reg.CPUIDHash = ""
		reg.HostKernelRelease = ""
		return
	}
	reg.HostFactsJSON = marshalHostFacts(f)
	reg.CPUIDHash = f.CPUIDHash
	reg.HostKernelRelease = f.HostKernelRelease
}

// mergeIncomingHostFacts reconciles a fresh heartbeat against prev. When
// KVMModuleScanned=false (read gap), prev module state is preserved.
// The result has Scanned cleared so it never persists.
func mergeIncomingHostFacts(prev, incoming *model.HostFacts) *model.HostFacts {
	if incoming == nil || incoming.IsZero() {
		return prev
	}
	if prev == nil {
		out := *incoming
		out.KVMModuleScanned = false
		return &out
	}
	out := *incoming
	if !incoming.KVMModuleScanned {
		out.KVMModuleFingerprint = prev.KVMModuleFingerprint
		out.KVMModuleTaint = prev.KVMModuleTaint
	}
	out.KVMModuleScanned = false
	return &out
}
