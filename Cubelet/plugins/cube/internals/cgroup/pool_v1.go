// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/allocator"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

type cgPoolV1 struct {
	enabled     bool
	prewarmSize int
	legacyMutex sync.Mutex
	legacyInUse map[uint32]struct{}
	handle      handle.Interface
	allocator   allocator.Allocator[uint16]
	cgRanger    *allocator.SimpleLinearRanger
}

// Disabled pools retain only the DB assignments from a previous run. Their
// directories, limits and cpusets are left untouched, matching poolV2.
func (cgp1 *cgPoolV1) trackLegacy(id uint32) {
	cgp1.legacyMutex.Lock()
	defer cgp1.legacyMutex.Unlock()
	if cgp1.legacyInUse == nil {
		cgp1.legacyInUse = make(map[uint32]struct{})
	}
	cgp1.legacyInUse[id] = struct{}{}
}

func (cgp1 *cgPoolV1) forgetLegacy(id uint32) {
	cgp1.legacyMutex.Lock()
	defer cgp1.legacyMutex.Unlock()
	delete(cgp1.legacyInUse, id)
}

func (cgp1 *cgPoolV1) legacyAll() []uint32 {
	cgp1.legacyMutex.Lock()
	defer cgp1.legacyMutex.Unlock()
	ids := make([]uint32, 0, len(cgp1.legacyInUse))
	for id := range cgp1.legacyInUse {
		ids = append(ids, id)
	}
	return ids
}

func (cgp1 *cgPoolV1) initialAssign(inuseCgIdInt uint32) error {
	// v1 ids span the full uint16 range; in-use ids must not be treated as
	// free after a restart.
	if inuseCgIdInt > math.MaxUint16 {
		return nil
	}

	cgId := uint16(inuseCgIdInt)

	if !cgp1.cgRanger.Contains(cgId) {
		cgp1.cgRanger.ExpandTo(cgId)
	}

	if err := cgp1.allocator.Assign(cgId); err != nil {

		return fmt.Errorf("failed to reserve cg %d: %w", cgId, err)
	}
	return nil
}

func (cgp1 *cgPoolV1) Get(ctx context.Context, sandboxID string) (*uint16, error) {
	if !cgp1.enabled {
		return nil, fmt.Errorf("cgroup pool v1 is not enabled")
	}
	cgId, err := cgp1.allocator.Allocate(func() error {
		_, err0 := cgp1.cgRanger.Expand()
		if err0 != nil {
			return err0
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cgPoolV1 failed to allocate cg: %w", err)
	}

	return &cgId, nil
}

func (cgp1 *cgPoolV1) allocatorRelease(id uint16) {
	if !cgp1.enabled {
		cgp1.forgetLegacy(uint32(id))
		return
	}
	cgp1.allocator.Release(id)
}

func (cgp1 *cgPoolV1) All() []uint32 {
	if !cgp1.enabled {
		return cgp1.legacyAll()
	}
	allV1 := cgp1.allocator.All()
	all := make([]uint32, len(allV1))
	for i, v := range allV1 {
		all[i] = uint32(v)
	}
	return all
}

func (cgp1 *cgPoolV1) AllGroups() ([]uint32, error) {
	if !cgp1.enabled {
		return cgp1.legacyAll(), nil
	}
	groups, err := cgp1.handle.List()
	if err != nil {
		return nil, fmt.Errorf("failed to list cgPoolV1: %w", err)
	}

	result := make([]uint32, 0)
	for _, group := range groups {
		groupID, err := strconv.ParseUint(group, 10, 16)
		if err != nil {
			continue
		}
		result = append(result, MakeFullCgID(uint16(groupID), false, -1))
	}

	return result, nil
}
