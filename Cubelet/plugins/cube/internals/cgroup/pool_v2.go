// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"fmt"
	"strconv"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/allocator"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/numa"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

type cgPoolV2 struct {
	handle    handle.Interface
	numaPools []cgPoolV2Numa
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
	return ids
}

func (p *cgPoolV2) Get(ctx context.Context, sandboxID string, numaNode int32) (*uint16, error) {
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

func (p *cgPoolV2) AllGroups() ([]uint32, error) {
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
