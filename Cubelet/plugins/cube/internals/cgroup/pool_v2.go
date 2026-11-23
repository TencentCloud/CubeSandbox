// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/allocator"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/numa"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

type cgPoolV2 struct {
	handle handle.Interface

	// enabled records whether this process bootstrapped the per-NUMA pool. Set
	// once by cgPool.init() and used by every "disabled" branch below.
	enabled   bool
	numaPools []cgPoolV2Numa

	// legacyInUse tracks slots handed out by a previous run that are still in
	// the DB while the pool is disabled. They are reported by All/AllGroups and
	// released on destroy, and their cgroups are never touched - see cgPool.Put.
	legacyMutex sync.Mutex
	legacyInUse map[uint32]struct{}
}

type cgPoolV2Numa struct {
	allocator allocator.Allocator[uint16]
	cgRanger  *allocator.SimpleLinearRanger
	numaNode  numa.NumaNode
}

func (cgp2n *cgPoolV2Numa) All() []uint32 {
	allU16 := cgp2n.allocator.All()
	allU32 := make([]uint32, len(allU16))
	for i, v := range allU16 {
		allU32[i] = MakeFullCgID(v, true, int32(cgp2n.numaNode.NodeId))
	}
	return allU32
}

func (cgp2n *cgPoolV2Numa) cgroupPath() string {
	return fmt.Sprintf("numa%d", cgp2n.numaNode.NodeId)
}

// trackLegacy records a slot handed out by a previous run. The caller has already
// split the DB ids by CgPoolV1IdLimit, so this is always a v2 id.
func (p *cgPoolV2) trackLegacy(fullCgID uint32) {
	p.legacyMutex.Lock()
	defer p.legacyMutex.Unlock()
	if p.legacyInUse == nil {
		p.legacyInUse = make(map[uint32]struct{})
	}
	p.legacyInUse[fullCgID] = struct{}{}
}

// forgetLegacy drops a slot from the bookkeeping; a no-op if it was never tracked.
func (p *cgPoolV2) forgetLegacy(fullCgID uint32) {
	p.legacyMutex.Lock()
	defer p.legacyMutex.Unlock()
	delete(p.legacyInUse, fullCgID)
}

// legacyAll returns a copy of the tracked slots.
func (p *cgPoolV2) legacyAll() []uint32 {
	p.legacyMutex.Lock()
	defer p.legacyMutex.Unlock()
	all := make([]uint32, 0, len(p.legacyInUse))
	for id := range p.legacyInUse {
		all = append(all, id)
	}
	return all
}

func (p *cgPoolV2) initialAssign(fullCgID uint32) error {
	numa, err := CgroupID2NumaID(fullCgID)
	if err != nil {
		return err
	}

	if numa >= len(p.numaPools) {
		return fmt.Errorf("numa index out of range, got %d, expected less than %d", numa, len(p.numaPools))
	}

	numaPool := &p.numaPools[numa]

	idInNuma := uint16(fullCgID % CgPoolIdLimit)
	if !numaPool.cgRanger.Contains(idInNuma) {
		numaPool.cgRanger.ExpandTo(idInNuma)
	}
	if err := numaPool.allocator.Assign(idInNuma); err != nil {
		return fmt.Errorf("numa pool %d: failed to reserve cg %d: %w", numa, idInNuma, err)
	}
	return nil
}

func (p *cgPoolV2) allocatorRelease(fullCgID uint32) {
	if !p.enabled {
		// Nothing was bootstrapped: the slot can only be a legacy one.
		p.forgetLegacy(fullCgID)
		return
	}
	numa, err := CgroupID2NumaID(fullCgID)
	if err != nil {
		log.G(context.Background()).Errorf("poolv2.allocatorRelease: failed to get numa id for cgroup %d: %v", fullCgID, err)
		return
	}
	if numa >= len(p.numaPools) {
		log.G(context.Background()).Errorf("poolv2.allocatorRelease: numa index out of range, got %d, expected less than %d", numa, len(p.numaPools))
		return
	}
	numaPool := &p.numaPools[numa]
	idInNuma := uint16(fullCgID % CgPoolIdLimit)
	numaPool.allocator.Release(idInNuma)
}

func (p *cgPoolV2) All() []uint32 {
	var ids []uint32
	for _, numaPool := range p.numaPools {
		ids = append(ids, numaPool.All()...)
	}
	return append(ids, p.legacyAll()...)
}

func (p *cgPoolV2) Get(ctx context.Context, sandboxID string, numaNode int32) (*uint16, error) {
	if !p.enabled {
		return nil, fmt.Errorf("cgroup pool v2 is not enabled")
	}
	if numaNode < 0 || int(numaNode) >= len(p.numaPools) {
		return nil, fmt.Errorf("invalid numa node: %d", numaNode)
	}
	numa := &p.numaPools[numaNode]
	idInNuma, err := numa.allocator.Allocate(func() error {
		_, err0 := numa.cgRanger.Expand()
		if err0 != nil {
			return err0
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cgPoolV2 failed to allocate cg: %w", err)
	}

	return &idInNuma, nil
}

// AllGroups lists the pool v2 groups this process accounts for: the node
// directories when bootstrapped, otherwise just the legacy slots. It never walks
// the pool root while disabled, so startup does not pay for leftover directories.
func (p *cgPoolV2) AllGroups() ([]uint32, error) {
	if !p.enabled {
		return p.legacyAll(), nil
	}
	result := make([]uint32, 0)
	for _, numaPool := range p.numaPools {
		groups, err := p.handle.ListSubdir(numaPool.cgroupPath())
		if err != nil {
			return nil, fmt.Errorf("failed to list cgroups in numa pool %d: %w", numaPool.numaNode.NodeId, err)
		}

		for _, group := range groups {

			groupID, err := strconv.ParseUint(group, 10, 16)

			if err != nil {

				continue
			}
			fullCgID := MakeFullCgID(uint16(groupID), true, int32(numaPool.numaNode.NodeId))
			result = append(result, fullCgID)
		}

	}
	return result, nil
}
