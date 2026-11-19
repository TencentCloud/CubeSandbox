// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/logging"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/store"
)

const (
	maxMachineTypeDisplayName = 128
	maxMachineTypeDescription = 512
)

var (
	// ErrMachineTypeExists is returned when the catalog name is already taken.
	ErrMachineTypeExists = errors.New("machine type already exists")
	// ErrMachineTypeNotFound is returned when the catalog has no such name.
	ErrMachineTypeNotFound = errors.New("machine type not found")
	// ErrMachineTypeInUse is returned when deleting a type that still has nodes
	// and the caller did not ask to unbind them.
	ErrMachineTypeInUse = errors.New("machine type is still bound to nodes")
	// ErrMachineTypeInvalid is returned for a rejected name or text field.
	ErrMachineTypeInvalid = errors.New("invalid machine type")
)

// CreateMachineType inserts a catalog row and optionally binds the given nodes.
func (svc *NodeService) CreateMachineType(ctx context.Context, req *model.CreateMachineTypeRequest, operator string) (*model.MachineTypeView, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: request is required", ErrMachineTypeInvalid)
	}
	name := strings.TrimSpace(req.Name)
	if err := ValidateMachineTypeValue(name); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMachineTypeInvalid, err)
	}
	display := strings.TrimSpace(req.DisplayName)
	desc := strings.TrimSpace(req.Description)
	if err := validateMachineTypeText(display, desc); err != nil {
		return nil, err
	}
	nodeIDs := uniqueNodeIDs(req.NodeIDs)
	for _, id := range nodeIDs {
		if _, err := svc.store.GetRegistration(ctx, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("%w: %s", ErrNodeNotFound, id)
			}
			return nil, err
		}
	}
	row := &store.MachineType{
		Name:        name,
		DisplayName: display,
		Description: desc,
		CreatedBy:   operator,
		UpdatedBy:   operator,
	}
	if err := svc.store.CreateMachineType(ctx, row); err != nil {
		if errors.Is(err, store.ErrMachineTypeExists) {
			return nil, fmt.Errorf("%w: %s", ErrMachineTypeExists, name)
		}
		return nil, err
	}
	for _, id := range nodeIDs {
		if err := svc.setMachineTypeLabel(ctx, id, name, operator); err != nil {
			return nil, fmt.Errorf("bind %s: %w", id, err)
		}
	}
	logging.G(ctx).Infof("nodemgmt: machine type created: name=%s operator=%s nodes=%d", name, operator, len(nodeIDs))
	return svc.GetMachineType(ctx, name)
}

// UpdateMachineType changes the display name and description. The name is immutable.
func (svc *NodeService) UpdateMachineType(ctx context.Context, name string, req *model.UpdateMachineTypeRequest, operator string) (*model.MachineTypeView, error) {
	name = strings.TrimSpace(name)
	if req == nil || (req.DisplayName == nil && req.Description == nil) {
		return nil, fmt.Errorf("%w: displayName or description is required", ErrMachineTypeInvalid)
	}
	row, err := svc.store.GetMachineType(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrMachineTypeNotFound, name)
		}
		return nil, err
	}
	if req.DisplayName != nil {
		row.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Description != nil {
		row.Description = strings.TrimSpace(*req.Description)
	}
	if err := validateMachineTypeText(row.DisplayName, row.Description); err != nil {
		return nil, err
	}
	row.UpdatedBy = operator
	if err := svc.store.UpdateMachineType(ctx, row); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrMachineTypeNotFound, name)
		}
		return nil, err
	}
	return svc.GetMachineType(ctx, name)
}

// DeleteMachineType removes a catalog row. Bound nodes block the delete unless
// unbind is set, in which case their machine-type label is cleared first.
func (svc *NodeService) DeleteMachineType(ctx context.Context, name string, unbind bool, operator string) error {
	name = strings.TrimSpace(name)
	if _, err := svc.store.GetMachineType(ctx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: %s", ErrMachineTypeNotFound, name)
		}
		return err
	}
	members, err := svc.nodesWithMachineType(ctx, name)
	if err != nil {
		return err
	}
	if len(members) > 0 && !unbind {
		ids := make([]string, 0, len(members))
		for _, n := range members {
			ids = append(ids, n.NodeID)
		}
		return fmt.Errorf("%w: %s nodes=%s", ErrMachineTypeInUse, name, strings.Join(ids, ","))
	}
	for _, n := range members {
		// strictMismatch is false: a node that moved to another type since the
		// membership scan no longer occupies name.
		if err := svc.clearMachineTypeIfBoundTo(ctx, n.NodeID, name, false, operator); err != nil {
			return fmt.Errorf("unbind %s: %w", n.NodeID, err)
		}
	}
	if err := svc.store.DeleteMachineType(ctx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: %s", ErrMachineTypeNotFound, name)
		}
		return err
	}
	logging.G(ctx).Infof("nodemgmt: machine type deleted: name=%s operator=%s unbound=%d", name, operator, len(members))
	return nil
}

// GetMachineType returns one catalog row with its live members and hardware mix.
func (svc *NodeService) GetMachineType(ctx context.Context, name string) (*model.MachineTypeView, error) {
	name = strings.TrimSpace(name)
	row, err := svc.store.GetMachineType(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrMachineTypeNotFound, name)
		}
		return nil, err
	}
	nodes, err := svc.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	return buildMachineTypeView(row, nodes), nil
}

// ListMachineTypes returns every catalog row with live membership.
func (svc *NodeService) ListMachineTypes(ctx context.Context) ([]*model.MachineTypeView, error) {
	rows, err := svc.store.ListMachineTypes(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := svc.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*model.MachineTypeView, 0, len(rows))
	for i := range rows {
		out = append(out, buildMachineTypeView(&rows[i], nodes))
	}
	return out, nil
}

// SetNodeMachineType binds one node to a catalog machine type, moving it off
// any previous type.
func (svc *NodeService) SetNodeMachineType(ctx context.Context, nodeID, name, operator string) error {
	name = strings.TrimSpace(name)
	if err := ValidateMachineTypeValue(name); err != nil {
		return fmt.Errorf("%w: %v", ErrMachineTypeInvalid, err)
	}
	if _, err := svc.store.GetMachineType(ctx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: %s", ErrMachineTypeNotFound, name)
		}
		return err
	}
	return svc.setMachineTypeLabel(ctx, nodeID, name, operator)
}

// ClearNodeMachineType removes the machine-type label from one node.
func (svc *NodeService) ClearNodeMachineType(ctx context.Context, nodeID, operator string) error {
	return svc.setMachineTypeLabel(ctx, nodeID, "", operator)
}

// BindMachineTypeNodes moves each node onto name. A node that belonged to
// another type is moved; failures are reported per node.
func (svc *NodeService) BindMachineTypeNodes(ctx context.Context, name string, nodeIDs []string, operator string) (*model.MachineTypeBindResult, error) {
	name = strings.TrimSpace(name)
	if _, err := svc.requireMachineType(ctx, name); err != nil {
		return nil, err
	}
	ids := uniqueNodeIDs(nodeIDs)
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: at least one node id is required", ErrMachineTypeInvalid)
	}
	return svc.applyMachineTypeNodes(ctx, name, ids, operator, false), nil
}

// UnbindMachineTypeNodes clears the label only when the node currently belongs
// to name. A node that already has no machine type is a success.
func (svc *NodeService) UnbindMachineTypeNodes(ctx context.Context, name string, nodeIDs []string, operator string) (*model.MachineTypeBindResult, error) {
	name = strings.TrimSpace(name)
	if _, err := svc.requireMachineType(ctx, name); err != nil {
		return nil, err
	}
	ids := uniqueNodeIDs(nodeIDs)
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: at least one node id is required", ErrMachineTypeInvalid)
	}
	result := &model.MachineTypeBindResult{Name: name, Results: make([]model.MachineTypeNodeResult, 0, len(ids))}
	for _, id := range ids {
		if err := svc.clearMachineTypeIfBoundTo(ctx, id, name, true, operator); err != nil {
			result.Results = append(result.Results, model.MachineTypeNodeResult{NodeID: id, Error: err.Error()})
			continue
		}
		result.Results = append(result.Results, model.MachineTypeNodeResult{NodeID: id, OK: true})
	}
	return result, nil
}

// ReplaceMachineTypeNodes makes name's members exactly nodeIDs.
func (svc *NodeService) ReplaceMachineTypeNodes(ctx context.Context, name string, nodeIDs []string, operator string) (*model.MachineTypeBindResult, error) {
	name = strings.TrimSpace(name)
	if _, err := svc.requireMachineType(ctx, name); err != nil {
		return nil, err
	}
	if nodeIDs == nil {
		return nil, fmt.Errorf("%w: nodeIDs is required; send [] to clear members", ErrMachineTypeInvalid)
	}
	target := uniqueNodeIDs(nodeIDs)
	members, err := svc.nodesWithMachineType(ctx, name)
	if err != nil {
		return nil, err
	}
	current := map[string]struct{}{}
	for _, n := range members {
		current[n.NodeID] = struct{}{}
	}
	want := map[string]struct{}{}
	for _, id := range target {
		want[id] = struct{}{}
	}
	var add, remove []string
	for _, id := range target {
		if _, ok := current[id]; !ok {
			add = append(add, id)
		}
	}
	for _, n := range members {
		if _, ok := want[n.NodeID]; !ok {
			remove = append(remove, n.NodeID)
		}
	}
	sort.Strings(remove)
	added := svc.applyMachineTypeNodes(ctx, name, add, operator, false)
	removed := svc.applyMachineTypeNodes(ctx, name, remove, operator, true)
	return &model.MachineTypeBindResult{
		Name:    name,
		Results: append(added.Results, removed.Results...),
	}, nil
}

func (svc *NodeService) requireMachineType(ctx context.Context, name string) (*store.MachineType, error) {
	row, err := svc.store.GetMachineType(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrMachineTypeNotFound, name)
		}
		return nil, err
	}
	return row, nil
}

// applyMachineTypeNodes binds (clear=false) or conditionally clears (clear=true)
// each id. A clear only removes the label when the locked row still belongs to
// name, so a concurrent rebind is left alone.
func (svc *NodeService) applyMachineTypeNodes(ctx context.Context, name string, nodeIDs []string, operator string, clear bool) *model.MachineTypeBindResult {
	result := &model.MachineTypeBindResult{Name: name, Results: make([]model.MachineTypeNodeResult, 0, len(nodeIDs))}
	for _, id := range nodeIDs {
		var err error
		if clear {
			err = svc.clearMachineTypeIfBoundTo(ctx, id, name, false, operator)
		} else {
			err = svc.setMachineTypeLabel(ctx, id, name, operator)
		}
		if err != nil {
			result.Results = append(result.Results, model.MachineTypeNodeResult{NodeID: id, Error: err.Error()})
			continue
		}
		result.Results = append(result.Results, model.MachineTypeNodeResult{NodeID: id, OK: true})
	}
	return result
}

// setMachineTypeLabel sets or explicitly clears the machine-type label. An
// empty next value clears the stored type. A no-op returns nil without an
// audit row.
func (svc *NodeService) setMachineTypeLabel(ctx context.Context, nodeID, next, operator string) error {
	nodeID = strings.TrimSpace(nodeID)
	next = strings.TrimSpace(next)
	if nodeID == "" {
		return ErrNodeIDRequired
	}
	if next != "" {
		if err := ValidateMachineTypeValue(next); err != nil {
			return fmt.Errorf("%w: %v", ErrMachineTypeInvalid, err)
		}
	}
	var prev string
	_, changed, err := svc.store.MutateLabels(ctx, nodeID, func(current map[string]string) (map[string]string, bool, error) {
		prev = model.MachineTypeValue(current)
		if prev == next {
			return current, false, nil
		}
		if next == "" {
			delete(current, model.LabelMachineType)
		} else {
			current[model.LabelMachineType] = next
		}
		return current, true, nil
	})
	if err != nil {
		return mapLabelMutateErr(nodeID, err)
	}
	if !changed {
		return nil
	}
	svc.finishMachineTypeWrite(ctx, nodeID, prev, next, operator)
	return nil
}

// clearMachineTypeIfBoundTo deletes the machine-type label only when the locked
// row still stores expectedName. When the node has moved to another type,
// strictMismatch reports the conflict; otherwise the move is treated as done.
func (svc *NodeService) clearMachineTypeIfBoundTo(ctx context.Context, nodeID, expectedName string, strictMismatch bool, operator string) error {
	nodeID = strings.TrimSpace(nodeID)
	expectedName = strings.TrimSpace(expectedName)
	if nodeID == "" {
		return ErrNodeIDRequired
	}
	var prev string
	_, changed, err := svc.store.MutateLabels(ctx, nodeID, func(current map[string]string) (map[string]string, bool, error) {
		prev = model.MachineTypeValue(current)
		if prev == "" || prev != expectedName {
			if prev != "" && prev != expectedName && strictMismatch {
				return nil, false, fmt.Errorf("node is bound to %q", prev)
			}
			return current, false, nil
		}
		delete(current, model.LabelMachineType)
		return current, true, nil
	})
	if err != nil {
		return mapLabelMutateErr(nodeID, err)
	}
	if !changed {
		return nil
	}
	svc.finishMachineTypeWrite(ctx, nodeID, prev, "", operator)
	return nil
}

func (svc *NodeService) finishMachineTypeWrite(ctx context.Context, nodeID, prev, next, operator string) {
	svc.syncRegistrationLabelsToRedis(ctx, nodeID)
	op := model.OpSetMachineType
	detail := fmt.Sprintf("machine_type %q -> %q", prev, next)
	if next == "" {
		op = model.OpClearMachineType
		detail = fmt.Sprintf("cleared machine_type %q", prev)
	}
	if err := svc.recordOperation(ctx, nodeID, op, operator, detail); err != nil {
		logging.G(ctx).Warnf("nodemgmt: machine type audit failed: node=%s: %v", nodeID, err)
	}
	logging.G(ctx).Infof("nodemgmt: machine type label updated: node=%s %s operator=%s", nodeID, detail, operator)
}

func mapLabelMutateErr(nodeID string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrNodeNotFound, nodeID)
	}
	if errors.Is(err, store.ErrCorruptLabelsJSON) {
		return fmt.Errorf("%w: %v", ErrLabelsJSONCorrupt, err)
	}
	return err
}

func (svc *NodeService) nodesWithMachineType(ctx context.Context, name string) ([]*model.NodeSnapshot, error) {
	nodes, err := svc.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*model.NodeSnapshot, 0)
	for _, n := range nodes {
		if n != nil && model.MachineTypeValue(n.Labels) == name {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

func validateMachineTypeText(display, desc string) error {
	if utf8.RuneCountInString(display) > maxMachineTypeDisplayName {
		return fmt.Errorf("%w: display name must be at most %d characters", ErrMachineTypeInvalid, maxMachineTypeDisplayName)
	}
	if utf8.RuneCountInString(desc) > maxMachineTypeDescription {
		return fmt.Errorf("%w: description must be at most %d characters", ErrMachineTypeInvalid, maxMachineTypeDescription)
	}
	return nil
}

func uniqueNodeIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func buildMachineTypeView(row *store.MachineType, nodes []*model.NodeSnapshot) *model.MachineTypeView {
	view := &model.MachineTypeView{
		Name:           row.Name,
		DisplayName:    row.DisplayName,
		Description:    row.Description,
		CreatedBy:      row.CreatedBy,
		UpdatedBy:      row.UpdatedBy,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
		Nodes:          []model.MachineTypeNode{},
		CPUIDGroups:    []model.MachineTypeCPUIDGroup{},
		KernelReleases: []model.MachineTypeKernelGroup{},
		Warnings:       []string{},
	}
	type cpuAcc struct {
		hash    string
		unknown bool
		vendor  string
		model   string
		ids     []string
	}
	cpus := map[string]*cpuAcc{}
	kernels := map[string]int{}
	var cpuKeys []string
	for _, n := range nodes {
		if n == nil || model.MachineTypeValue(n.Labels) != row.Name {
			continue
		}
		member := machineTypeNodeFrom(n)
		view.Nodes = append(view.Nodes, member)
		view.NodeCount++
		if n.Healthy {
			view.HealthyNodeCount++
		}
		key := member.CPUIDHash
		unknown := key == ""
		if unknown {
			key = "unknown"
		}
		acc, ok := cpus[key]
		if !ok {
			acc = &cpuAcc{hash: member.CPUIDHash, unknown: unknown, vendor: member.CPUVendor, model: member.CPUModel}
			cpus[key] = acc
			cpuKeys = append(cpuKeys, key)
		}
		if acc.vendor == "" {
			acc.vendor = member.CPUVendor
		}
		if acc.model == "" {
			acc.model = member.CPUModel
		}
		acc.ids = append(acc.ids, member.NodeID)
		kernels[member.HostKernelRelease]++
	}
	sort.Slice(view.Nodes, func(i, j int) bool { return view.Nodes[i].NodeID < view.Nodes[j].NodeID })
	sort.Slice(cpuKeys, func(i, j int) bool {
		if cpus[cpuKeys[i]].unknown != cpus[cpuKeys[j]].unknown {
			return !cpus[cpuKeys[i]].unknown
		}
		return cpuKeys[i] < cpuKeys[j]
	})
	distinctCPU := 0
	missingFacts := false
	for _, key := range cpuKeys {
		acc := cpus[key]
		sort.Strings(acc.ids)
		if acc.unknown {
			missingFacts = true
		} else {
			distinctCPU++
		}
		view.CPUIDGroups = append(view.CPUIDGroups, model.MachineTypeCPUIDGroup{
			CPUIDHash: acc.hash,
			Unknown:   acc.unknown,
			CPUVendor: acc.vendor,
			CPUModel:  acc.model,
			NodeCount: len(acc.ids),
			NodeIDs:   acc.ids,
		})
	}
	kKeys := make([]string, 0, len(kernels))
	for k := range kernels {
		kKeys = append(kKeys, k)
	}
	sort.Strings(kKeys)
	for _, k := range kKeys {
		view.KernelReleases = append(view.KernelReleases, model.MachineTypeKernelGroup{
			Release:   k,
			NodeCount: kernels[k],
		})
	}
	if distinctCPU > 1 {
		view.Warnings = append(view.Warnings, model.MachineTypeWarningMultipleCPUID)
	}
	if missingFacts {
		view.Warnings = append(view.Warnings, model.MachineTypeWarningMissingHostFacts)
	}
	return view
}

func machineTypeNodeFrom(n *model.NodeSnapshot) model.MachineTypeNode {
	member := model.MachineTypeNode{
		NodeID:             n.NodeID,
		HostIP:             n.HostIP,
		Healthy:            n.Healthy,
		SchedulingDisabled: n.SchedulingDisabled,
	}
	if n.HostFacts != nil {
		member.CPUVendor = strings.TrimSpace(n.HostFacts.CPUVendor)
		member.CPUModel = strings.TrimSpace(n.HostFacts.CPUModel)
		member.CPUIDHash = strings.TrimSpace(n.HostFacts.CPUIDHash)
		member.HostKernelRelease = strings.TrimSpace(n.HostFacts.HostKernelRelease)
	}
	return member
}
