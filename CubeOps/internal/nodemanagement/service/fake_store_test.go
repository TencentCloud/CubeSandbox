// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
)

type fakeNodeStore struct {
	mu       sync.Mutex
	regs     map[string]*store.NodeRegistration
	statuses map[string]*store.NodeStatus
	versions map[string][]store.NodeComponentVersion
	ops      []*store.NodeOperation

	// failOn injects errors for specific methods (non-nil → returned).
	failOnGetRegistration error
	failOnUpdateLabels    error
	failOnUpdateQuota     error

	updateQuotaCalls     int
	updateHostFactsCalls int
	lastQuota            model.QuotaReport

	specs                 map[string]*store.NodeQuotaSpec
	failOnGetQuotaSpec    error
	failOnUpsertQuotaSpec error

	machineTypes map[string]*store.MachineType
}

func newFakeNodeStore() *fakeNodeStore {
	return &fakeNodeStore{
		regs:         map[string]*store.NodeRegistration{},
		statuses:     map[string]*store.NodeStatus{},
		versions:     map[string][]store.NodeComponentVersion{},
		specs:        map[string]*store.NodeQuotaSpec{},
		machineTypes: map[string]*store.MachineType{},
	}
}

func (f *fakeNodeStore) UpsertRegistration(_ context.Context, reg *store.NodeRegistration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Mirror gormNodeStore.UpsertRegistration: keep labels_json on an existing
	// row so re-registering an isolated node can be reproduced in tests.
	if existing, ok := f.regs[reg.NodeID]; ok {
		existing.HostIP = reg.HostIP
		existing.GRPCPort = reg.GRPCPort
		existing.CapacityJSON = reg.CapacityJSON
		existing.AllocatableJSON = reg.AllocatableJSON
		existing.InstanceType = reg.InstanceType
		existing.ClusterLabel = reg.ClusterLabel
		existing.QuotaCPU = reg.QuotaCPU
		existing.QuotaMemMB = reg.QuotaMemMB
		existing.CreateConcurrentNum = reg.CreateConcurrentNum
		existing.MaxMvmNum = reg.MaxMvmNum
		existing.HostFactsJSON = reg.HostFactsJSON
		existing.CPUIDHash = reg.CPUIDHash
		existing.HostKernelRelease = reg.HostKernelRelease
		f.regs[reg.NodeID] = existing
		return nil
	}
	f.regs[reg.NodeID] = reg
	return nil
}

func (f *fakeNodeStore) GetRegistration(ctx context.Context, nodeID string) (*store.NodeRegistration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnGetRegistration != nil {
		return nil, f.failOnGetRegistration
	}
	reg, ok := f.regs[nodeID]
	if !ok {
		return nil, store.ErrNotFound
	}
	// Return a copy so callers mutate without racing the stored row.
	cp := *reg
	return &cp, nil
}

func (f *fakeNodeStore) ListRegistrations(_ context.Context) ([]store.NodeRegistration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.NodeRegistration, 0, len(f.regs))
	for _, r := range f.regs {
		out = append(out, *r)
	}
	return out, nil
}

func (f *fakeNodeStore) UpdateLabels(_ context.Context, nodeID string, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnUpdateLabels != nil {
		return f.failOnUpdateLabels
	}
	reg, ok := f.regs[nodeID]
	if !ok {
		return store.ErrNotFound
	}
	reg.LabelsJSON = model.MustJSON(labels)
	return nil
}

func (f *fakeNodeStore) MutateLabels(_ context.Context, nodeID string, fn func(current map[string]string) (map[string]string, bool, error)) (map[string]string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnGetRegistration != nil {
		return nil, false, f.failOnGetRegistration
	}
	reg, ok := f.regs[nodeID]
	if !ok {
		return nil, false, store.ErrNotFound
	}
	current, err := store.ParseLabelsJSON(reg.LabelsJSON)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", store.ErrCorruptLabelsJSON, err)
	}
	cp := make(map[string]string, len(current))
	for k, v := range current {
		cp[k] = v
	}
	next, changed, err := fn(cp)
	if err != nil {
		return nil, false, err
	}
	if next == nil {
		next = map[string]string{}
	}
	if changed {
		if f.failOnUpdateLabels != nil {
			return nil, false, f.failOnUpdateLabels
		}
		reg.LabelsJSON = model.MustJSON(next)
	}
	out := make(map[string]string, len(next))
	for k, v := range next {
		out[k] = v
	}
	return out, changed, nil
}

func (f *fakeNodeStore) UpdateHostFacts(_ context.Context, nodeID string, factsJSON, cpuidHash, kernelRelease string, cpuCount, memTotalMB int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateHostFactsCalls++
	reg, ok := f.regs[nodeID]
	if !ok {
		return store.ErrNotFound
	}
	reg.HostFactsJSON = factsJSON
	reg.CPUIDHash = cpuidHash
	reg.HostKernelRelease = kernelRelease
	reg.CPUCount = cpuCount
	reg.MemTotalMB = memTotalMB
	return nil
}

func (f *fakeNodeStore) UpdateQuota(_ context.Context, nodeID string, q model.QuotaReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateQuotaCalls++
	if f.failOnUpdateQuota != nil {
		return f.failOnUpdateQuota
	}
	reg, ok := f.regs[nodeID]
	if !ok {
		return store.ErrNotFound
	}
	reg.QuotaCPU = q.MilliCPU
	reg.QuotaMemMB = q.MemMB
	reg.MaxMvmNum = q.MaxMvmNum
	reg.CreateConcurrentNum = q.CreateConcurrentNum
	// Mirror gormNodeStore.UpdateQuota: Capacity/Allocatable track quota.
	snapshot := model.MustJSON(model.ResourceSnapshot{MilliCPU: q.MilliCPU, MemoryMB: q.MemMB})
	reg.CapacityJSON = snapshot
	reg.AllocatableJSON = snapshot
	if q.PausedReleaseRatio != nil {
		reg.PausedReleaseRatio = q.PausedReleaseRatio
	}
	f.lastQuota = q
	return nil
}

func (f *fakeNodeStore) GetQuotaSpec(_ context.Context, nodeID string) (*store.NodeQuotaSpec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnGetQuotaSpec != nil {
		return nil, f.failOnGetQuotaSpec
	}
	spec, ok := f.specs[nodeID]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *spec
	return &cp, nil
}

func (f *fakeNodeStore) UpsertQuotaSpec(_ context.Context, spec *store.NodeQuotaSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnUpsertQuotaSpec != nil {
		return f.failOnUpsertQuotaSpec
	}
	if existing, ok := f.specs[spec.NodeID]; ok {
		existing.Revision++
	} else {
		cp := *spec
		f.specs[spec.NodeID] = &cp
	}
	return nil
}

func (f *fakeNodeStore) CreateQuotaSpecIfAbsent(_ context.Context, spec *store.NodeQuotaSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnUpsertQuotaSpec != nil {
		return f.failOnUpsertQuotaSpec
	}
	if _, ok := f.specs[spec.NodeID]; ok {
		return store.ErrQuotaSpecExists
	}
	cp := *spec
	f.specs[spec.NodeID] = &cp
	return nil
}

func (f *fakeNodeStore) UpdateQuotaSpecIfRevision(_ context.Context, spec *store.NodeQuotaSpec, expected int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing, ok := f.specs[spec.NodeID]
	if !ok || existing.Revision != expected {
		return false, nil
	}
	cp := *spec
	cp.Revision = existing.Revision + 1
	f.specs[spec.NodeID] = &cp
	return true, nil
}

func (f *fakeNodeStore) ListQuotaSpecs(_ context.Context) ([]store.NodeQuotaSpec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.NodeQuotaSpec, 0, len(f.specs))
	for _, s := range f.specs {
		cp := *s
		out = append(out, cp)
	}
	return out, nil
}

func (f *fakeNodeStore) ListClusterQuotaFollowers(_ context.Context) ([]store.NodeQuotaSpec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.NodeQuotaSpec, 0)
	for _, s := range f.specs {
		if s.NodeID == store.ClusterQuotaSentinelID || s.PausedReleaseRatio != nil {
			continue
		}
		cp := *s
		out = append(out, cp)
	}
	return out, nil
}

func (f *fakeNodeStore) BumpClusterQuotaFollowers(_ context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, s := range f.specs {
		if s.NodeID == store.ClusterQuotaSentinelID || s.PausedReleaseRatio != nil {
			continue
		}
		s.Revision++
		n++
	}
	return n, nil
}

func (f *fakeNodeStore) UpsertStatus(_ context.Context, st *store.NodeStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[st.NodeID] = st
	return nil
}

func (f *fakeNodeStore) GetStatus(_ context.Context, nodeID string) (*store.NodeStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.statuses[nodeID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return st, nil
}

func (f *fakeNodeStore) ListStatuses(_ context.Context) ([]store.NodeStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.NodeStatus, 0, len(f.statuses))
	for _, s := range f.statuses {
		out = append(out, *s)
	}
	return out, nil
}

func (f *fakeNodeStore) WriteComponentVersions(_ context.Context, nodeID string, versions []model.ComponentVersion, inventoryIncomplete bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := make([]store.NodeComponentVersion, 0, len(versions))
	for _, v := range versions {
		rows = append(rows, store.NodeComponentVersion{
			NodeID:    nodeID,
			Component: v.Component,
			Version:   v.Version,
		})
	}
	if inventoryIncomplete {
		existing := f.versions[nodeID]
		merged := map[string]store.NodeComponentVersion{}
		for _, v := range existing {
			merged[v.Component] = v
		}
		for _, v := range rows {
			merged[v.Component] = v
		}
		rows = make([]store.NodeComponentVersion, 0, len(merged))
		for _, v := range merged {
			rows = append(rows, v)
		}
	}
	f.versions[nodeID] = rows
	return nil
}

func (f *fakeNodeStore) ListComponentVersions(_ context.Context) ([]store.NodeComponentVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.NodeComponentVersion, 0)
	for _, rows := range f.versions {
		out = append(out, rows...)
	}
	return out, nil
}

func (f *fakeNodeStore) ListComponentVersionsByNode(_ context.Context, nodeID string) ([]store.NodeComponentVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.NodeComponentVersion(nil), f.versions[nodeID]...), nil
}

func (f *fakeNodeStore) CreateOperation(_ context.Context, op *store.NodeOperation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, op)
	return nil
}

func (f *fakeNodeStore) ListOperations(_ context.Context, nodeID string, limit int) ([]store.NodeOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.NodeOperation, 0)
	for _, op := range f.ops {
		if op.NodeID == nodeID {
			out = append(out, *op)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeNodeStore) ListOperationsByTypes(_ context.Context, nodeID string, types []string, limit int) ([]store.NodeOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	allowed := make(map[string]bool, len(types))
	for _, t := range types {
		allowed[t] = true
	}
	out := make([]store.NodeOperation, 0)
	for _, op := range f.ops {
		if op.NodeID == nodeID && allowed[op.Type] {
			out = append(out, *op)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// DeleteRegistration removes only the registration row. Returns
// store.ErrNotFound when the node is absent, mirroring the gorm store.
func (f *fakeNodeStore) DeleteRegistration(_ context.Context, nodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.regs[nodeID]; !ok {
		return store.ErrNotFound
	}
	delete(f.regs, nodeID)
	return nil
}

// DeleteNode mirrors the gorm store: removes registration, status and
// component versions in one logical transaction. Operations rows are
// preserved as an audit trail. Returns store.ErrNotFound when the
// registration row is absent.
func (f *fakeNodeStore) DeleteNode(_ context.Context, nodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.regs[nodeID]; !ok {
		return store.ErrNotFound
	}
	delete(f.regs, nodeID)
	delete(f.statuses, nodeID)
	delete(f.versions, nodeID)
	return nil
}

func (f *fakeNodeStore) CreateMachineType(_ context.Context, mt *store.MachineType) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if mt == nil || mt.Name == "" {
		return errors.New("machine type name is required")
	}
	if _, ok := f.machineTypes[mt.Name]; ok {
		return store.ErrMachineTypeExists
	}
	cp := *mt
	f.machineTypes[mt.Name] = &cp
	return nil
}

func (f *fakeNodeStore) GetMachineType(_ context.Context, name string) (*store.MachineType, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mt, ok := f.machineTypes[name]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *mt
	return &cp, nil
}

func (f *fakeNodeStore) ListMachineTypes(_ context.Context) ([]store.MachineType, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.MachineType, 0, len(f.machineTypes))
	for _, mt := range f.machineTypes {
		out = append(out, *mt)
	}
	return out, nil
}

func (f *fakeNodeStore) UpdateMachineType(_ context.Context, mt *store.MachineType) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing, ok := f.machineTypes[mt.Name]
	if !ok {
		return store.ErrNotFound
	}
	existing.DisplayName = mt.DisplayName
	existing.Description = mt.Description
	existing.UpdatedBy = mt.UpdatedBy
	existing.UpdatedAt = mt.UpdatedAt
	return nil
}

func (f *fakeNodeStore) DeleteMachineType(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.machineTypes[name]; !ok {
		return store.ErrNotFound
	}
	delete(f.machineTypes, name)
	return nil
}
